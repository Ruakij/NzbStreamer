package nzbservice

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"time"

	"git.ruekov.eu/ruakij/nzbStreamer/internal/filehealth"
	"git.ruekov.eu/ruakij/nzbStreamer/internal/nzbstore"
	"git.ruekov.eu/ruakij/nzbStreamer/pkg/nzbparser"
)

// PeriodicScanConfig is what the background pass runs with. The confidence is
// the settled one, reached against a release clients can already read; MinAge
// is shared with the add, since how old a post must be for a miss to count as
// final is not a property of either pass.
type PeriodicScanConfig struct {
	// Interval is how often the pass runs and how long an answer counts as
	// fresh. 0 or less disables the pass.
	Interval time.Duration
	// MinAge is how old a post must be for a miss to be recorded as a verdict
	// rather than as a retry.
	MinAge time.Duration
	// Confidence is the fraction of a group's segments the pass probes for;
	// with the tolerance at zero it is also the covered fraction. 0 scans
	// nothing.
	Confidence float64
}

// SetPeriodicScan wires the background pass that continues what an add's check
// covered. It runs on a ticker of the interval, takes the same slot a build
// takes, and holds the context Init is given, which the service cancels on
// shutdown. A nil checker or a non-positive interval leaves it off.
func (s *Service) SetPeriodicScan(checker filehealth.Checker, config PeriodicScanConfig) {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	s.periodicChecker = checker
	s.periodicConfig = config
}

// probeFlushSize bounds how many verdicts sit unflushed while a scan runs, so a
// restart repeats at most this many probes of a group rather than all of them.
const probeFlushSize = 500

// runPeriodicScans is the background pass. One tick walks every completed
// record, and every failed record a retry has come round on, and re-asks what
// is left of each, to the periodic confidence. It runs with the settings it
// was started with, which is what SetPeriodicScan wired before Init let it
// loose.
func (s *Service) runPeriodicScans(ctx context.Context) {
	s.mutex.RLock()
	config := s.periodicConfig
	s.mutex.RUnlock()

	ticker := time.NewTicker(config.Interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.periodicPass(ctx, config)
		}
	}
}

// periodicPass rescans what is worth rescanning. A completed record is asked
// for what it has left, to the periodic confidence. A failed one has had its
// verdict and its groups are never re-probed - with the one exception the
// minimum age carves out: a miss made before the post was old enough to be
// final left a retry instead of a verdict, and once that retry has come round
// the pass re-asks exactly that group.
func (s *Service) periodicPass(ctx context.Context, config PeriodicScanConfig) {
	records, err := s.store.List()
	if err != nil {
		slog.Warn("Failed listing nzbs for the periodic scan", "error", err)
		return
	}

	for _, record := range records {
		stage := Stage(record.Stage)
		if stage != StageCompleted && stage != StageFailed {
			continue
		}
		if stage == StageFailed && !s.retryDue(record.Data) {
			continue
		}
		if ctx.Err() != nil {
			return
		}
		s.scanRecord(ctx, record, config)
	}
}

// retryDue reports whether any file of the nzb has a retry that has come
// round, which is the one thing that makes a failed record worth visiting.
// The schedule is what this process recorded; the store keeps retry_after but
// answers no read for it.
func (s *Service) retryDue(nzbData *nzbparser.NzbData) bool {
	if nzbData == nil {
		return false
	}

	s.mutex.RLock()
	defer s.mutex.RUnlock()

	now := time.Now()
	for i := range nzbData.Files {
		if after, ok := s.retryAfter[nzbData.MetaName][nzbData.Files[i].Filename]; ok && !after.After(now) {
			return true
		}
	}
	return false
}

// scanRecord brings one completed release a step towards its settled
// confidence, and a failed release the group whose retry has come round. The
// store rows are the scan state: what a restart finds is what the last flush
// learned, and the pass resumes from there on its own.
func (s *Service) scanRecord(ctx context.Context, record nzbstore.Record, config PeriodicScanConfig) {
	if record.Data == nil || len(record.Data.Files) == 0 {
		return
	}

	// The pass probes what the add would have presented, on a copy of the
	// record's data: the blacklist is the add's decision, and the record's
	// data is what a rebuild reads too
	nzbData := *record.Data
	nzbData.Files = slices.Clone(record.Data.Files)
	s.filterNzbFiles(&nzbData)
	if len(nzbData.Files) == 0 {
		return
	}
	metaName := nzbData.MetaName
	failedRecord := Stage(record.Stage) == StageFailed

	groups := filehealth.Groups(&nzbData)
	if len(groups) == 0 {
		return
	}

	// The rows a verdict hangs off are in place before anything reports against
	// them; the upsert is idempotent, so this only fills the gap where the add
	// path has not recorded the files yet
	s.ensureSourceFiles(&nzbData)

	verdicts, err := s.store.SegmentVerdicts(metaName, contentFilenames(groups))
	if err != nil {
		slog.Warn("Failed reading segment verdicts, skipping the scan of an nzb",
			"nzb", metaName, "error", err)
		return
	}

	// Fresh coverage is a segment confirmed present within the interval, by a
	// probe or by a read delivering its bytes; older answers are expired and
	// are what the scan spends its probes on. known is keyed the way the
	// checker flattens a group, so its walk skips exactly these positions.
	freshCutoff := time.Now().Add(-config.Interval)
	known := make(map[string][]int, len(groups))
	// reAsked are the groups a passed retry sent back to the server; settled
	// are the ones whose verdict is already durable and are probed never again
	var reAsked, settled []filehealth.ContentGroup
	for _, g := range groups {
		var fresh []int
		missing := false
		for i, file := range g.Files {
			for local := range file.Segments {
				verdict, knownRow := verdicts[file.Filename][local]
				if !knownRow {
					continue
				}
				if !verdict.Present {
					missing = true
					continue
				}
				freshAt := verdict.CheckedAt
				if verdict.FetchedAt.After(freshAt) {
					freshAt = verdict.FetchedAt
				}
				if freshAt.After(freshCutoff) {
					fresh = append(fresh, g.Offsets[i]+local)
				}
			}
		}
		known[g.Name] = fresh

		// A group with a missing segment is settled and never re-probed, with
		// one exception: a miss made before the post was old enough to be final
		// left a retry instead of a verdict, and once that has passed the post
		// may have propagated, which is worth one more question.
		if s.retryAfterPassed(metaName, g) {
			reAsked = append(reAsked, g)
			continue
		}
		if missing {
			known[g.Name] = allPositions(g.Segments)
			settled = append(settled, g)
			continue
		}
		// The rest of a failed record has had its verdict, and probing a
		// healthy group inside a release that failed for an unrelated reason
		// buys nobody anything
		if failedRecord {
			known[g.Name] = allPositions(g.Segments)
		}
	}

	var failed []filehealth.FailedGroup
	// A settled group's files may still be presented: a crash between the
	// presenter removal and the store delete re-registers them on the next
	// start, and a verdict a read fed back lands while they are up. Removal is
	// idempotent, so it simply runs again, and the record says why.
	if len(settled) > 0 {
		sources := make(map[string]bool)
		for _, g := range settled {
			for _, file := range g.Files {
				sources[file.Filename] = true
			}
		}
		if len(s.presentedPaths(metaName, sources)) > 0 {
			for _, g := range settled {
				failed = append(failed, filehealth.FailedGroup{Name: g.Name, Files: filenamesOf(g)})
			}
		}
	}

	// Nothing presents the progress of a scan the service is being torn down in
	if !s.beginScan(metaName, ctx, Stage(record.Stage)) {
		return
	}

	recorder := newProbeRecorder(s, metaName, s.probeMinAge(), offsetsOf(groups))
	release := s.slots.acquire()
	defer release()
	// A panic in the check must leave the item where a restart finds it and
	// the slot free; one failScan moved on to failed keeps that
	defer s.endScan(metaName)

	// The bar covers the whole record, so each group's probes are added to
	// what the groups before it spent
	var accDone, accTotal, lastDone, lastTotal int
	failed = append(failed, s.periodicChecker.Scan(ctx, &nzbData, config.Confidence, known,
		recorder.report, func(done, total int) {
			if done < lastDone {
				// The check reports per group; a drop back is the next one starting
				lastDone, lastTotal = 0, 0
			}
			accDone += done - lastDone
			accTotal += total - lastTotal
			lastDone, lastTotal = done, total
			if ctx.Err() == nil {
				s.progress(metaName, accDone, accTotal)
			}
		})...)
	recorder.flush()

	// The re-asks are over and their answers are in the verdicts, so the
	// schedule that asked for them is spent. One cut short by a cancel may
	// have no answer yet, and keeps its retry.
	if ctx.Err() == nil {
		s.clearRetryAfter(metaName, reAsked)
	}

	if len(failed) > 0 {
		s.failScan(metaName, failed)
	} else {
		s.endScan(metaName)
	}
}

// failScan pulls the failed groups back and moves the record to failed. The
// files were presented and possibly imported, so removal is what protects the
// next open; the record says what happened to them.
func (s *Service) failScan(metaName string, failed []filehealth.FailedGroup) {
	filenames := make(map[string]bool)
	for _, group := range failed {
		for _, file := range group.Files {
			filenames[file] = true
		}
	}

	s.removePresented(metaName, filenames)

	names := slices.Sorted(maps.Keys(filenames))
	err := fmt.Errorf("%w: %d files beyond repair and not presented: %s",
		ErrHealthCheckFailed, len(names), strings.Join(names, ", "))
	s.failedScan(metaName, err)
}

// presentedPaths returns the presented paths built from the named source
// files, which is what a verdict on those files pulls back. Rows that do not
// say which source file they were built from cannot be attributed to a group,
// so a tree from before that attribution goes whole rather than leaving a
// damaged file presented.
func (s *Service) presentedPaths(metaName string, sources map[string]bool) []string {
	stored, err := s.store.Files(metaName)
	if err != nil {
		slog.Error("Failed reading the presented files of an nzb, nothing is pulled back",
			"nzb", metaName, "error", err)
		return nil
	}

	var paths []string
	attributed := false
	for _, file := range stored {
		if file.Source == "" {
			continue
		}
		attributed = true
		if sources[file.Source] {
			paths = append(paths, file.Path)
		}
	}
	if !attributed && len(stored) > 0 {
		for _, file := range stored {
			paths = append(paths, file.Path)
		}
	}
	return paths
}

// removePresented takes the presented paths built from the failed source files
// out of the presenters and out of the store. It is idempotent: paths and rows
// already gone are no-ops, so a removal a crash cut short simply runs again.
func (s *Service) removePresented(metaName string, failedFiles map[string]bool) {
	paths := s.presentedPaths(metaName, failedFiles)
	if len(paths) == 0 {
		return
	}

	for _, p := range paths {
		for _, presenter := range s.presenters {
			if err := presenter.RemoveFile(p); err != nil {
				// One presenter failing does not keep the others honest: the
				// next open of a pulled file fails on its own anyway
				slog.Error("Failed removing a file from a presenter",
					"nzb", metaName, "file", p, "error", err)
			}
		}
	}

	s.mutex.Lock()
	remaining := s.nzbFiles[metaName][:0]
	for _, file := range s.nzbFiles[metaName] {
		if !slices.Contains(paths, file.Path) {
			remaining = append(remaining, file)
		}
	}
	s.nzbFiles[metaName] = remaining
	if len(remaining) == 0 {
		// Nothing is left to report on, so the record loses its files the way
		// an add whose every file was dropped does
		s.unregister(metaName)
	}
	s.mutex.Unlock()

	if err := s.store.RemoveFiles(metaName, paths); err != nil {
		slog.Error("Failed removing the rows of files a scan pulled back",
			"nzb", metaName, "error", err)
	}
}

// failedScan records a scan that failed a group, the way finish records an add
// that did: the item is history, the store says why. An item the scan does not
// hold - one a re-add replaced, or none at all - is not this scan's to mark,
// and neither is the record behind it.
func (s *Service) failedScan(id string, err error) {
	s.queueMutex.Lock()
	item := s.find(id)
	if item == nil || item.Stage != StageScanning {
		s.queueMutex.Unlock()
		return
	}
	item.Stage = StageFailed
	item.Err = err.Error()
	item.HealthCheckFailed = errors.Is(err, ErrHealthCheckFailed)
	message := item.Err
	s.queueMutex.Unlock()

	if err := s.store.SetStage(id, string(StageFailed), message); err != nil {
		slog.Error("Failed recording a scan that failed a group", "MetaName", id, "error", err)
	}
}

// beginScan claims an item for the pass. A completed one moves into
// StageScanning, where it sits in the history the way a rebuilding one does; a
// failed one keeps its stage and its error, since the re-ask acts on one group
// and the record already says what the add found. It reports whether the scan
// may go on: an item that is not the record this pass picked up is not this
// pass's to mark, which is what a re-add of the same name in between looks
// like.
func (s *Service) beginScan(id string, ctx context.Context, from Stage) bool {
	if ctx.Err() != nil {
		return false
	}

	s.queueMutex.Lock()
	defer s.queueMutex.Unlock()

	item := s.find(id)
	if item == nil || item.Stage != from {
		return false
	}
	if from == StageCompleted {
		item.Stage = StageScanning
		item.stageDone, item.stageTotal = 0, 0
	}
	return true
}

// endScan moves a scanning item back to completed. One that failedScan moved
// on to failed keeps that.
func (s *Service) endScan(id string) {
	s.queueMutex.Lock()
	defer s.queueMutex.Unlock()

	item := s.find(id)
	if item != nil && item.Stage == StageScanning {
		item.Stage = StageCompleted
		item.stageDone, item.stageTotal = 0, 0
	}
}

// recordRetryAfter asks the store to re-ask a file's posts when they are old
// enough, and remembers having asked: the store keeps retry_after but answers
// no read for it, so the pass consults what this process set. A restart
// re-derives the schedule on its own, because a miss below MinAge leaves no
// verdict row for the pass to skip the group on.
func (s *Service) recordRetryAfter(metaName, filename string, after time.Time) {
	s.mutex.Lock()
	if s.retryAfter == nil {
		s.retryAfter = make(map[string]map[string]time.Time)
	}
	if s.retryAfter[metaName] == nil {
		s.retryAfter[metaName] = make(map[string]time.Time)
	}
	s.retryAfter[metaName][filename] = after
	s.mutex.Unlock()

	if err := s.store.SetRetryAfter(metaName, filename, after); err != nil {
		slog.Warn("Failed recording a retry for a young post",
			"nzb", metaName, "file", filename, "error", err)
	}
}

// retryAfterPassed reports whether any file of the group has a retry that has
// come round. No retry recorded is a group that stays settled.
func (s *Service) retryAfterPassed(metaName string, g filehealth.ContentGroup) bool {
	s.mutex.RLock()
	defer s.mutex.RUnlock()

	now := time.Now()
	for _, file := range g.Files {
		if after, ok := s.retryAfter[metaName][file.Filename]; ok && !after.After(now) {
			return true
		}
	}
	return false
}

// clearRetryAfter spends the passed retries of the groups a re-ask answered:
// the verdicts it wrote are what settles or clears them now, and a schedule
// that has come round would only ask the same question again. A retry still in
// the future is left alone, and so is one of a file the re-ask found young
// again, which is a new schedule, not a spent one.
func (s *Service) clearRetryAfter(metaName string, groups []filehealth.ContentGroup) {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	now := time.Now()
	for _, g := range groups {
		for _, file := range g.Files {
			if after, ok := s.retryAfter[metaName][file.Filename]; ok && !after.After(now) {
				delete(s.retryAfter[metaName], file.Filename)
			}
		}
	}
}

func (s *Service) probeMinAge() time.Duration {
	s.mutex.RLock()
	defer s.mutex.RUnlock()
	return s.periodicConfig.MinAge
}

// filenamesOf lists the nzb's own files a group is made of, which is what a
// verdict names.
func filenamesOf(g filehealth.ContentGroup) []string {
	names := make([]string, len(g.Files))
	for i, file := range g.Files {
		names[i] = file.Filename
	}
	return names
}

// allPositions is what a scan is told a group already knows, when the group is
// settled and nothing is to be asked about it: every position counts, so the
// target is met before the first probe is spent.
func allPositions(segments int) []int {
	positions := make([]int, segments)
	for i := range positions {
		positions[i] = i
	}
	return positions
}

func contentFilenames(groups []filehealth.ContentGroup) []string {
	var names []string
	for _, g := range groups {
		for _, file := range g.Files {
			names = append(names, file.Filename)
		}
	}
	return names
}

// offsetsOf maps each content filename to where it starts in its group's
// flattened index, which is how a reported group position is read back as the
// segment's own index within its file.
func offsetsOf(groups []filehealth.ContentGroup) map[string]int {
	offsets := make(map[string]int)
	for _, g := range groups {
		for i, file := range g.Files {
			offsets[file.Filename] = g.Offsets[i]
		}
	}
	return offsets
}

// ensureSourceFiles records the nzb's own content files, the rows a health
// verdict hangs off, before anything reports against them. PostedAt is what
// PROBE_MIN_AGE is measured against.
func (s *Service) ensureSourceFiles(nzbData *nzbparser.NzbData) {
	groups := filehealth.Groups(nzbData)
	if len(groups) == 0 {
		return
	}

	files := make([]nzbstore.SourceFile, 0, len(nzbData.Files))
	for _, g := range groups {
		for _, file := range g.Files {
			files = append(files, nzbstore.SourceFile{
				Filename: file.Filename,
				PostedAt: file.ParsedDate,
			})
		}
	}
	if err := s.store.EnsureSourceFiles(nzbData.MetaName, files); err != nil {
		slog.Warn("Failed recording the nzb's own files", "nzb", nzbData.MetaName, "error", err)
	}
}

// probeRecorder collects the verdicts a check or a scan reports and persists
// them in batches, so a scan is not one write per segment and a restart
// repeats at most a flush. A miss on a post younger than MinAge is no verdict:
// nothing durable says the article is gone, so it becomes a retry instead,
// which is what leaves the group re-askable once the post is old enough.
type probeRecorder struct {
	service *Service
	nzb     string
	minAge  time.Duration
	offsets map[string]int

	probes []nzbstore.ProbeResult
	// The files whose miss was too young to settle, with the retry it earns
	young map[string]time.Time
}

func newProbeRecorder(service *Service, nzb string, minAge time.Duration, offsets map[string]int) *probeRecorder {
	return &probeRecorder{
		service: service,
		nzb:     nzb,
		minAge:  minAge,
		offsets: offsets,
		young:   make(map[string]time.Time),
	}
}

// report receives the verdicts in the order the checker answers them, which it
// serializes. index is the position in the group's flattened space; the file's
// offset within its group turns it back into the segment's own index.
func (r *probeRecorder) report(file *nzbparser.File, index int, present bool) {
	if file == nil {
		return
	}
	local := index - r.offsets[file.Filename]
	if local < 0 || local >= len(file.Segments) {
		// A position the offsets cannot place would be recorded against the
		// wrong segment; a verdict misfiled is worse than one missing
		return
	}

	if !present && r.minAge > 0 && time.Since(file.ParsedDate) < r.minAge {
		// The post may still be propagating: fail the group, but settle nothing
		r.young[file.Filename] = file.ParsedDate.Add(r.minAge)
		return
	}

	r.probes = append(r.probes, nzbstore.ProbeResult{
		Filename:  file.Filename,
		Index:     local,
		MessageID: file.Segments[local].ID,
		Present:   present,
	})
	if len(r.probes) >= probeFlushSize {
		r.flushProbes()
	}
}

// flushProbes persists what has accumulated, so a restart resumes at the last
// flush rather than repeating a group.
func (r *probeRecorder) flushProbes() {
	if len(r.probes) == 0 {
		return
	}
	probes := r.probes
	r.probes = make([]nzbstore.ProbeResult, 0, probeFlushSize)
	if err := r.service.store.RecordProbes(r.nzb, probes); err != nil {
		slog.Warn("Failed storing probe verdicts", "nzb", r.nzb, "error", err)
	}
}

// flush persists everything the scan reported. It is called once the scan is
// over and before anything acts on a failure, since the verdict is what a
// removal acts on.
func (r *probeRecorder) flush() {
	r.flushProbes()
	for filename, after := range r.young {
		r.service.recordRetryAfter(r.nzb, filename, after)
	}
	r.young = make(map[string]time.Time)
}
