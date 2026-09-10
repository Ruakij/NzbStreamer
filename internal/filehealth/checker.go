// Package filehealth probes a sample of a group's segments on the server and
// reports the groups that lost a segment.
package filehealth

import (
	"context"
	"errors"
	"log/slog"
	"math"
	"sync"
	"time"

	"git.ruekov.eu/ruakij/nzbStreamer/pkg/filenameops"
	"git.ruekov.eu/ruakij/nzbStreamer/pkg/nzbparser"
)

var ErrSegmentsMissing = errors.New("segments missing on server")

// ErrProbeFailed marks a check that learned nothing: a probe errored, which
// says nothing about whether the segment exists, so it is no verdict.
var ErrProbeFailed = errors.New("segment probe failed")

// SegmentExistsFunc reports whether a segment is retrievable from the server.
type SegmentExistsFunc func(id string) (bool, error)

type CheckerConfig struct {
	// AddConfidence is the fraction of a group's segments probed at add time.
	// With the tolerance at zero it is also the covered fraction: being c sure
	// that nothing is missing means probing c of the segments. 0 disables
	// checking.
	AddConfidence float64
	// Maximum concurrent segment-checks.
	MaxParallel int

	// Reserved for a nonzero tolerance (par2 repair), and unused while the
	// tolerance is zero:
	MaxMissingPercent float64
	Par2Safety        float64
	UndecidedAccept   bool
}

// Ensure DefaultChecker implements Checker interface
var _ Checker = (*DefaultChecker)(nil)

// DefaultChecker verifies that a groups segments are still present on the server.
type DefaultChecker struct {
	config CheckerConfig
	exists SegmentExistsFunc
}

func NewDefaultChecker(config CheckerConfig, exists SegmentExistsFunc) *DefaultChecker {
	if config.MaxParallel < 1 {
		config.MaxParallel = 1
	}
	return &DefaultChecker{config: config, exists: exists}
}

// Groups splits the nzb's content files into their units. Volume sets are
// grouped with the same function the build uses, so the check and the tree
// cannot disagree about what a unit is. The nzb's own file order is the walk
// order, so the groups come back in a stable order.
func Groups(nzbData *nzbparser.NzbData) []ContentGroup {
	content := contentFiles(nzbData)
	if len(content) == 0 {
		return nil
	}

	grouped := filenameops.GroupPartFilenames(groupedNames(content))
	filenameops.SortGroupedFilenames(grouped)

	// Map each filename back to its group name, so the nzb's file order can
	// supply the group order and a stable first-encounter grouping.
	fileToGroup := make(map[string]string, len(content))
	for name, files := range grouped {
		for _, f := range files {
			fileToGroup[f] = name
		}
	}
	byFile := make(map[string]*nzbparser.File, len(content))
	for _, file := range content {
		byFile[file.Filename] = file
	}

	// Walk content in the nzb's own order, creating a group on first encounter.
	order := make([]string, 0, len(grouped))
	seen := make(map[string]bool, len(grouped))
	for _, file := range content {
		name := fileToGroup[file.Filename]
		if !seen[name] {
			seen[name] = true
			order = append(order, name)
		}
	}

	result := make([]ContentGroup, 0, len(order))
	for _, name := range order {
		g := ContentGroup{Name: name}
		// Offsets are where each file starts in the group's flattened segment
		// index, which is the space a reported verdict position is read back in
		offset := 0
		for _, filename := range grouped[name] {
			file := byFile[filename]
			g.Files = append(g.Files, file)
			g.Offsets = append(g.Offsets, offset)
			offset += len(file.Segments)
		}
		g.Segments = offset
		result = append(result, g)
	}
	return result
}

// groupedNames returns the filenames of content as a fresh slice, which
// GroupPartFilenames consumes.
func groupedNames(content []*nzbparser.File) []string {
	names := make([]string, len(content))
	for i, file := range content {
		names[i] = file.Filename
	}
	return names
}

// probeOrder is the order the segments of a group are probed in, such that any
// prefix of it is spread evenly over the whole: 0, N/2, N/4, 3N/4, ...
// Bit-reversal gives that for free, so stopping anywhere leaves a sample with no
// gap larger than twice the smallest, and resuming is an index into it.
// length<=1 returns {0} or the single index; length==0 returns nil.
func probeOrder(length int) []int {
	if length <= 1 {
		if length == 0 {
			return nil
		}
		return []int{0}
	}

	// smallest power of two >= length
	size := 1
	for size < length {
		size <<= 1
	}

	order := make([]int, 0, length)
	for i := 0; i < size; i++ {
		// bit-reverse i over log2(size) bits
		rev := 0
		for x, b := i, size>>1; b > 0; b >>= 1 {
			rev = rev<<1 | x&1
			x >>= 1
		}
		if rev < length {
			order = append(order, rev)
		}
	}
	return order
}

// progressReporter counts the probes of one group against the probes that group
// planned, and hands both to whoever asked. A nil report is the caller that
// does not want to know.
type progressReporter struct {
	report ProgressFunc

	mu    sync.Mutex
	done  int
	total int
}

func (p *progressReporter) plan(segments int) {
	p.update(0, segments)
}

func (p *progressReporter) step() {
	p.update(1, 0)
}

func (p *progressReporter) update(done, total int) {
	if p.report == nil {
		return
	}

	// Reported under the lock, so what the caller sees only ever moves forward
	p.mu.Lock()
	defer p.mu.Unlock()

	p.done += done
	p.total += total
	p.report(p.done, p.total)
}

// CheckFiles scans every content group to the add-time confidence and returns
// the groups that lost a segment. A group whose probes errored is not returned:
// dropping it on a transient server failure would hide files that may be fine.
func (c *DefaultChecker) CheckFiles(ctx context.Context, nzbData *nzbparser.NzbData, report VerdictReport, progress ProgressFunc) []FailedGroup {
	return c.Scan(ctx, nzbData, c.config.AddConfidence, nil, report, progress)
}

// Scan probes every content group of the nzb to the given confidence and
// returns the groups that lost a segment. confidence <= 0 scans nothing. known
// holds the flattened positions that already count as present per group name
// (as GroupPartFilenames names it), so a rescan re-asks only what is unknown; a
// position at or past the group's segment count is ignored. report may be nil.
func (c *DefaultChecker) Scan(ctx context.Context, nzbData *nzbparser.NzbData, confidence float64, known map[string][]int, report VerdictReport, progress ProgressFunc) []FailedGroup {
	if confidence <= 0 {
		return nil
	}
	started := time.Now()
	var (
		failed        []FailedGroup
		totalSegments int
	)
	for _, g := range Groups(nzbData) {
		totalSegments += g.Segments
		if _, err := c.scan(ctx, g, known[g.Name], probeTarget(confidence, g.Segments), report, progress); err != nil {
			if !errors.Is(err, ErrSegmentsMissing) {
				slog.Warn("Segment probe failed, group left unchecked", "group", g.Name, "err", err)
				continue
			}
			files := make([]string, len(g.Files))
			for i, f := range g.Files {
				files[i] = f.Filename
			}
			failed = append(failed, FailedGroup{Name: g.Name, Files: files})
		}
	}
	recordCheck(ctx, started, totalSegments)
	return failed
}

// probeTarget is how many of a population a scan to confidence covers: at zero
// tolerance being c sure that nothing is missing means probing c of the
// segments. Never below one and never above the population.
func probeTarget(confidence float64, segments int) int {
	target := int(math.Round(confidence * float64(segments)))
	if target < 1 {
		target = 1
	}
	if target > segments {
		target = segments
	}
	return target
}

// PlannedProbes is the work a check of this nzb would be, in segments it would
// ask the server about, worked out without asking about any of them.
func (c *DefaultChecker) PlannedProbes(nzbData *nzbparser.NzbData) int {
	if c.config.AddConfidence <= 0 {
		return 0
	}
	planned := 0
	for _, g := range Groups(nzbData) {
		planned += probeTarget(c.config.AddConfidence, g.Segments)
	}
	return planned
}

// scan probes the order over g's segments until target of them count as present
// or one is missing. known holds the positions that already count and are
// skipped rather than re-asked; the walk extends past them, so the probes land
// on positions nothing knows about yet and coverage reaches target. It returns
// how many count as present and stops early on a missing segment, because at
// zero tolerance there is nothing further to learn. Every answered probe is
// handed to report, present or missing, so a caller can persist the verdicts. A
// probe that errors is returned as ErrProbeFailed and reported to nobody: an
// unanswered probe is no verdict about the segments.
func (c *DefaultChecker) scan(ctx context.Context, g ContentGroup, known []int, target int, report VerdictReport, progress ProgressFunc) (covered int, err error) {
	if g.Segments == 0 || target <= 0 {
		return 0, nil
	}
	if report == nil {
		report = func(*nzbparser.File, int, bool) {}
	}
	if target > g.Segments {
		target = g.Segments
	}

	order := probeOrder(g.Segments)

	knownSet := make(map[int]bool, len(known))
	for _, pos := range known {
		// Out-of-range and repeated entries say nothing extra; counting them
		// would credit coverage no probe stands behind.
		if pos < 0 || pos >= g.Segments || knownSet[pos] {
			continue
		}
		knownSet[pos] = true
		covered++
	}

	// Positions known to be present already count, so only what is left of the
	// target is asked of the server.
	need := target - covered
	if need <= 0 {
		return covered, nil
	}

	reporter := &progressReporter{report: progress}
	reporter.plan(need)

	// Flatten the files' segments into one index space, so the order walks the
	// group end to end.
	var (
		mu      sync.Mutex
		wg      sync.WaitGroup
		sem     = make(chan struct{}, c.config.MaxParallel)
		missing bool
		errored bool
	)
	gctx, cancel := context.WithCancel(ctx)
	defer cancel()

	// The walk extends past the known positions instead of stopping at the
	// target-th order entry, so a rescan widens the sample rather than
	// re-walking the prefix a previous pass covered.
	scheduled := 0
	for i := 0; i < len(order) && scheduled < need; i++ {
		pos := order[i]
		if knownSet[pos] {
			continue
		}
		scheduled++

		file, id := segmentAt(g, pos)
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()

			if gctx.Err() != nil {
				return
			}

			exists, err := c.exists(id)

			reporter.step()

			mu.Lock()
			defer mu.Unlock()
			switch {
			case err != nil:
				recordProbe(gctx, "error")
				errored = true
			case !exists:
				recordProbe(gctx, "missing")
				missing = true
				cancel()
				report(file, pos, false)
			default:
				recordProbe(gctx, "present")
				covered++
				report(file, pos, true)
			}
		}()
	}
	wg.Wait()

	// A confirmed miss decides the group even if another probe errored; only
	// when nothing was confirmed does an errored probe surface as no verdict.
	switch {
	case missing:
		return covered, ErrSegmentsMissing
	case errored:
		return covered, ErrProbeFailed
	}
	return covered, nil
}

// segmentAt resolves a group-index position to the file holding it and the
// message-id of the segment that sits there.
func segmentAt(g ContentGroup, pos int) (*nzbparser.File, string) {
	for _, file := range g.Files {
		if pos < len(file.Segments) {
			return file, file.Segments[pos].ID
		}
		pos -= len(file.Segments)
	}
	return nil, ""
}

// contentFiles picks the files whose loss would make the release unusable. They
// are the only ones grouped and probed: a missing par2 or nfo costs nothing
// being measured here.
func contentFiles(nzbData *nzbparser.NzbData) []*nzbparser.File {
	var files []*nzbparser.File
	for i := range nzbData.Files {
		if filenameops.Classify(nzbData.Files[i].Filename) == filenameops.ClassContent && len(nzbData.Files[i].Segments) > 0 {
			files = append(files, &nzbData.Files[i])
		}
	}
	return files
}
