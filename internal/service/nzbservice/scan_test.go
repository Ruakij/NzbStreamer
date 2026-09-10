package nzbservice

import (
	"context"
	"errors"
	"io"
	"regexp"
	"strings"
	"testing"
	"time"

	"git.ruekov.eu/ruakij/nzbStreamer/internal/filehealth"
	"git.ruekov.eu/ruakij/nzbStreamer/internal/nzbrecordfactory"
	"git.ruekov.eu/ruakij/nzbStreamer/internal/nzbstore"
	"git.ruekov.eu/ruakij/nzbStreamer/internal/presentation"
	"git.ruekov.eu/ruakij/nzbStreamer/pkg/nzbparser"
)

// The pass is driven directly rather than through the ticker, so every test
// below runs in one goroutine and the store fakes need no locks.

// scanStore is a store in a map, enough of one for the pass: the rows a scan
// reads and writes, the presented files, and the record stages.
type scanStore struct {
	records     map[string]nzbstore.Record
	order       []string
	presented   map[string][]nzbstore.File
	sourceFiles map[string]map[string]nzbstore.SourceFile
	retryAfter  map[string]map[string]time.Time
	verdicts    map[string]map[string]map[int]nzbstore.SegmentVerdict
}

func newScanStore() *scanStore {
	return &scanStore{
		records:     map[string]nzbstore.Record{},
		presented:   map[string][]nzbstore.File{},
		sourceFiles: map[string]map[string]nzbstore.SourceFile{},
		retryAfter:  map[string]map[string]time.Time{},
		verdicts:    map[string]map[string]map[int]nzbstore.SegmentVerdict{},
	}
}

func (s *scanStore) put(data *nzbparser.NzbData, stage string) {
	if _, ok := s.records[data.MetaName]; !ok {
		s.order = append(s.order, data.MetaName)
	}
	s.records[data.MetaName] = nzbstore.Record{Data: data, Stage: stage}
}

func (s *scanStore) List() ([]nzbstore.Record, error) {
	list := make([]nzbstore.Record, 0, len(s.order))
	for _, name := range s.order {
		if record, ok := s.records[name]; ok {
			list = append(list, record)
		}
	}
	return list, nil
}

func (s *scanStore) Raw(name string) ([]byte, error) {
	if record, ok := s.records[name]; ok {
		return record.Data.Raw, nil
	}
	return nil, nzbstore.ErrNotFound
}

func (s *scanStore) Add(data *nzbparser.NzbData, stage, _ string) error {
	s.put(data, stage)
	return nil
}

func (s *scanStore) SetStage(name, stage, errMessage string) error {
	record, ok := s.records[name]
	if !ok {
		return nil
	}
	record.Stage, record.Err = stage, errMessage
	s.records[name] = record
	return nil
}

func (s *scanStore) SetArchived(_ string, _ bool) error { return nil }

func (s *scanStore) SetFiles(name, _ string, files []nzbstore.File) error {
	s.presented[name] = files
	return nil
}

func (s *scanStore) Files(name string) ([]nzbstore.File, error) {
	return s.presented[name], nil
}

func (s *scanStore) RemoveFiles(name string, paths []string) error {
	removed := make(map[string]bool, len(paths))
	for _, path := range paths {
		removed[path] = true
	}
	kept := s.presented[name][:0]
	for _, file := range s.presented[name] {
		if !removed[file.Path] {
			kept = append(kept, file)
		}
	}
	s.presented[name] = kept
	return nil
}

func (s *scanStore) EnsureSourceFiles(nzbName string, files []nzbstore.SourceFile) error {
	if s.sourceFiles[nzbName] == nil {
		s.sourceFiles[nzbName] = map[string]nzbstore.SourceFile{}
	}
	for _, file := range files {
		if _, ok := s.sourceFiles[nzbName][file.Filename]; !ok {
			s.sourceFiles[nzbName][file.Filename] = file
		}
	}
	return nil
}

func (s *scanStore) SetRetryAfter(nzbName, filename string, after time.Time) error {
	if s.retryAfter[nzbName] == nil {
		s.retryAfter[nzbName] = map[string]time.Time{}
	}
	s.retryAfter[nzbName][filename] = after
	return nil
}

func (s *scanStore) SegmentVerdicts(nzbName string, filenames []string) (map[string]map[int]nzbstore.SegmentVerdict, error) {
	verdicts := make(map[string]map[int]nzbstore.SegmentVerdict, len(filenames))
	for _, filename := range filenames {
		if index := s.verdicts[nzbName][filename]; len(index) > 0 {
			verdicts[filename] = index
		}
	}
	return verdicts, nil
}

func (s *scanStore) RecordProbes(nzbName string, probes []nzbstore.ProbeResult) error {
	for _, probe := range probes {
		if s.verdicts[nzbName] == nil {
			s.verdicts[nzbName] = map[string]map[int]nzbstore.SegmentVerdict{}
		}
		if s.verdicts[nzbName][probe.Filename] == nil {
			s.verdicts[nzbName][probe.Filename] = map[int]nzbstore.SegmentVerdict{}
		}
		s.verdicts[nzbName][probe.Filename][probe.Index] = nzbstore.SegmentVerdict{
			Filename: probe.Filename, Index: probe.Index,
			MessageID: probe.MessageID, Present: probe.Present,
			CheckedAt: time.Now(),
		}
	}
	return nil
}

func (s *scanStore) Delete(name string) error {
	delete(s.records, name)
	delete(s.presented, name)
	return nil
}

// verdict stages what a scan would have learned, so a test can set the state
// the pass resumes from
func (s *scanStore) verdict(nzbName, filename string, index int, present bool, checkedAt time.Time) {
	if s.verdicts[nzbName] == nil {
		s.verdicts[nzbName] = map[string]map[int]nzbstore.SegmentVerdict{}
	}
	if s.verdicts[nzbName][filename] == nil {
		s.verdicts[nzbName][filename] = map[int]nzbstore.SegmentVerdict{}
	}
	s.verdicts[nzbName][filename][index] = nzbstore.SegmentVerdict{
		Filename: filename, Index: index, Present: present, CheckedAt: checkedAt,
	}
}

// scanChecker answers what the test says and records what it was asked.
type scanChecker struct {
	failed []filehealth.FailedGroup
	// ScanHook answers the scan when set, reporting verdicts on the way
	ScanHook func(data *nzbparser.NzbData, report filehealth.VerdictReport) []filehealth.FailedGroup
	// scans holds one entry per Scan call
	scans []scanCall
}

type scanCall struct {
	nzb        string
	confidence float64
	known      map[string][]int
}

func (c *scanChecker) CheckFiles(context.Context, *nzbparser.NzbData, filehealth.VerdictReport, filehealth.ProgressFunc) []filehealth.FailedGroup {
	return nil
}

func (c *scanChecker) Scan(_ context.Context, nzbData *nzbparser.NzbData, confidence float64, known map[string][]int, report filehealth.VerdictReport, _ filehealth.ProgressFunc) []filehealth.FailedGroup {
	c.scans = append(c.scans, scanCall{nzb: nzbData.MetaName, confidence: confidence, known: known})
	if c.ScanHook != nil {
		return c.ScanHook(nzbData, report)
	}
	return c.failed
}

func (c *scanChecker) PlannedProbes(*nzbparser.NzbData) int { return 0 }

func (c *scanChecker) scanned(name string) (scanCall, bool) {
	for _, call := range c.scans {
		if call.nzb == name {
			return call, true
		}
	}
	return scanCall{}, false
}

// scanFactory builds nothing; the pass never opens an archive
type scanFactory struct{}

func (scanFactory) BuildSegmentStackFromNzbData(*nzbparser.NzbData, nzbrecordfactory.ProgressFunc) (nzbrecordfactory.BuildResult, error) {
	return nzbrecordfactory.BuildResult{}, nil
}

func (scanFactory) DiscardSegmentStackFromNzbData(*nzbparser.NzbData) {}

type scanPresenter struct {
	removed []string
}

func (p *scanPresenter) AddFile(string, time.Time, presentation.Openable) error { return nil }

func (p *scanPresenter) RemoveFile(fullpath string) error {
	p.removed = append(p.removed, fullpath)
	return nil
}

// scanFile answers with a size, which is all a listing needs of it
type scanFile struct{}

func (scanFile) SizeHint() (int64, error)         { return 42, nil }
func (scanFile) Open() (io.ReadSeekCloser, error) { return nil, errors.New("no bytes") }

func scanNzbData(name string, files ...nzbparser.File) *nzbparser.NzbData {
	return &nzbparser.NzbData{MetaName: name, Files: files}
}

func twoSegmentFile(name string, date time.Time) nzbparser.File {
	return nzbparser.File{
		Filename:   name,
		ParsedDate: date,
		Segments:   []nzbparser.Segment{{ID: name + "-1"}, {ID: name + "-2"}},
	}
}

// scanService wires a service over the fakes with the pass configured but not
// running: the tests trigger it directly.
func scanService(t *testing.T, store *scanStore, checker *scanChecker, config PeriodicScanConfig, presenters ...presentation.Presenter) *Service {
	t.Helper()
	service := NewService(store, &scanFactory{}, presenters, nil, checker)
	service.SetPeriodicScan(checker, config)
	return service
}

// The pass lists the completed records and leaves the rest alone: a failed
// release has had its verdict, and probing a healthy group inside it buys
// nobody anything.
func TestPeriodicPassScansCompletedRecordsOnly(t *testing.T) {
	store := newScanStore()
	old := time.Now().Add(-72 * time.Hour)
	done := scanNzbData("Done.Release", twoSegmentFile("a.rar", old))
	failed := scanNzbData("Failed.Release", twoSegmentFile("a.rar", old))
	cancelled := scanNzbData("Cancelled.Release", twoSegmentFile("a.rar", old))
	store.put(done, string(StageCompleted))
	store.put(failed, string(StageFailed))
	store.put(cancelled, string(StageCancelled))

	checker := &scanChecker{}
	config := PeriodicScanConfig{Interval: time.Hour, Confidence: 0.99}
	service := scanService(t, store, checker, config)

	// The queue items a restart restored, the same way Init lays them down
	for _, record := range mustList(t, store) {
		service.restore(record)
	}

	service.periodicPass(context.Background(), config)

	if len(checker.scans) != 1 || checker.scans[0].nzb != "Done.Release" {
		t.Fatalf("the pass scanned %v, want only Done.Release", checker.scans)
	}
	if got := checker.scans[0].confidence; got != 0.99 {
		t.Errorf("the scan ran to confidence %v, want the periodic one", got)
	}

	// The item came back out of StageScanning once the scan was over
	if item := service.find("Done.Release"); item.Stage != StageCompleted {
		t.Errorf("a scanned record sat at %q after the pass", item.Stage)
	}
}

func mustList(t *testing.T, store *scanStore) []nzbstore.Record {
	t.Helper()
	records, err := store.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	return records
}

// A group with a missing segment is settled: the scan is told every position
// counts, so it spends no probe on it. Fresh coverage of a healthy group works
// the same way from the other side: what the add and earlier passes confirmed
// within the interval is skipped rather than re-asked.
func TestPeriodicPassSkipsSettledGroupsAndStaleCoverageCounts(t *testing.T) {
	store := newScanStore()
	now := time.Now()
	data := scanNzbData("Done.Release", twoSegmentFile("a.rar", now.Add(-72*time.Hour)))
	store.put(data, string(StageCompleted))

	checker := &scanChecker{}
	config := PeriodicScanConfig{Interval: time.Hour, Confidence: 0.99}
	service := scanService(t, store, checker, config)
	service.restore(store.records["Done.Release"])

	// One segment failed and stays failed; one is present but its answer is
	// older than the interval, so it does not count as fresh either
	store.verdict("Done.Release", "a.rar", 0, false, now.Add(-time.Hour))
	store.verdict("Done.Release", "a.rar", 1, true, now.Add(-2*24*time.Hour))

	service.periodicPass(context.Background(), config)

	call, ok := checker.scanned("Done.Release")
	if !ok {
		t.Fatal("the pass did not scan the record at all")
	}
	if got := call.known["a.rar"]; len(got) != 2 || got[0] != 0 || got[1] != 1 {
		t.Errorf("a settled group was told known is %v, want every position so nothing is probed", got)
	}
}

// What a read or a probe confirmed inside the interval is coverage, and the
// pass does not spend a probe on it.
func TestPeriodicPassSkipsFreshCoverage(t *testing.T) {
	store := newScanStore()
	now := time.Now()
	data := scanNzbData("Done.Release", twoSegmentFile("a.rar", now.Add(-72*time.Hour)))
	store.put(data, string(StageCompleted))

	checker := &scanChecker{}
	config := PeriodicScanConfig{Interval: 24 * time.Hour, Confidence: 0.5}
	service := scanService(t, store, checker, config)
	service.restore(store.records["Done.Release"])

	store.verdict("Done.Release", "a.rar", 0, true, now.Add(-time.Hour))
	store.verdict("Done.Release", "a.rar", 1, true, now.Add(-2*24*time.Hour))

	service.periodicPass(context.Background(), config)

	call, ok := checker.scanned("Done.Release")
	if !ok {
		t.Fatal("the pass did not scan the record")
	}
	// One fresh position covers the target of one; the stale one is expired
	if got := call.known["a.rar"]; len(got) != 1 || got[0] != 0 {
		t.Errorf("fresh coverage was reported as %v, want position 0 only", got)
	}
}

// A scan that fails a group pulls the presented files it built back and moves
// the record to failed, with the group named in the error.
func TestPeriodicPassPullsAFailedGroupBack(t *testing.T) {
	store := newScanStore()
	now := time.Now()
	data := scanNzbData("Done.Release",
		twoSegmentFile("a.rar", now.Add(-72*time.Hour)),
		twoSegmentFile("b.rar", now.Add(-72*time.Hour)))
	store.put(data, string(StageCompleted))

	presenter := &scanPresenter{removed: []string{}}
	checker := &scanChecker{failed: []filehealth.FailedGroup{{Name: "a.rar", Files: []string{"a.rar"}}}}
	config := PeriodicScanConfig{Interval: time.Hour, Confidence: 0.99}
	service := scanService(t, store, checker, config, presenter)
	service.restore(store.records["Done.Release"])

	tree := map[string]presentation.Openable{
		"Done.Release/a.mkv": scanFile{},
		"Done.Release/b.mkv": scanFile{},
	}
	service.register(data, tree)
	store.presented["Done.Release"] = []nzbstore.File{
		{Path: "Done.Release/a.mkv", Size: 42, Source: "a.rar"},
		{Path: "Done.Release/b.mkv", Size: 42, Source: "b.rar"},
	}

	service.periodicPass(context.Background(), config)

	if len(presenter.removed) != 1 || presenter.removed[0] != "Done.Release/a.mkv" {
		t.Errorf("the pass removed %v, want only the failed group's file", presenter.removed)
	}
	if got := store.presented["Done.Release"]; len(got) != 1 || got[0].Path != "Done.Release/b.mkv" {
		t.Errorf("the store holds %v, want only the surviving file", got)
	}
	if files := service.Files()["Done.Release"]; len(files) != 1 || files[0].Path != "Done.Release/b.mkv" {
		t.Errorf("the service still reports %v", files)
	}

	item := service.find("Done.Release")
	if item.Stage != StageFailed {
		t.Errorf("a scanned record that lost a group sits at %q", item.Stage)
	}
	if !item.HealthCheckFailed || !strings.Contains(item.Err, "a.rar") {
		t.Errorf("the record reported err %q, health-check-failed %v", item.Err, item.HealthCheckFailed)
	}
	if got := store.records["Done.Release"].Stage; got != string(StageFailed) {
		t.Errorf("the store holds %q for a record whose scan failed a group", got)
	}
}

// Rows that do not say which source file they were built from cannot be
// attributed to a group, so the whole tree goes rather than a damaged file
// staying presented.
func TestPeriodicPassPullsTheWholeTreeBackWhenRowsAreUnattributed(t *testing.T) {
	store := newScanStore()
	now := time.Now()
	data := scanNzbData("Done.Release", twoSegmentFile("a.rar", now.Add(-72*time.Hour)))
	store.put(data, string(StageCompleted))

	presenter := &scanPresenter{removed: []string{}}
	checker := &scanChecker{failed: []filehealth.FailedGroup{{Name: "a.rar", Files: []string{"a.rar"}}}}
	config := PeriodicScanConfig{Interval: time.Hour, Confidence: 0.99}
	service := scanService(t, store, checker, config, presenter)
	service.restore(store.records["Done.Release"])

	tree := map[string]presentation.Openable{
		"Done.Release/a.mkv": scanFile{},
		"Done.Release/b.mkv": scanFile{},
	}
	service.register(data, tree)
	store.presented["Done.Release"] = []nzbstore.File{
		{Path: "Done.Release/a.mkv", Size: 42},
		{Path: "Done.Release/b.mkv", Size: 42},
	}

	service.periodicPass(context.Background(), config)

	if len(presenter.removed) != 2 {
		t.Errorf("the pass removed %v, want the whole unattributed tree", presenter.removed)
	}
	if item := service.find("Done.Release"); item.Stage != StageFailed {
		t.Errorf("the record sits at %q", item.Stage)
	}
}

// A miss made before the post was old enough to be final left a retry instead
// of a verdict. The pass re-asks the group once that retry has come round, and
// what it finds settles the group for good: healthy keeps the record, another
// miss fails it durably.
func TestPeriodicPassReasksAFailedGroupOnceItsRetryHasPassed(t *testing.T) {
	for _, test := range []struct {
		name          string
		reportPresent bool
		failed        []filehealth.FailedGroup
		wantStage     Stage
	}{
		{
			name:          "the post propagated and the group survives",
			reportPresent: true,
			wantStage:     StageCompleted,
		},
		{
			name:          "the post is gone for good",
			reportPresent: false,
			failed:        []filehealth.FailedGroup{{Name: "a.rar", Files: []string{"a.rar"}}},
			wantStage:     StageFailed,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			store := newScanStore()
			now := time.Now()
			data := scanNzbData("Done.Release", twoSegmentFile("a.rar", now.Add(-72*time.Hour)))
			store.put(data, string(StageCompleted))

			checker := &scanChecker{failed: test.failed}
			config := PeriodicScanConfig{Interval: time.Hour, Confidence: 0.99}
			service := scanService(t, store, checker, config)
			service.restore(store.records["Done.Release"])

			// A previous pass found the miss while the post was young, so it
			// recorded a retry rather than a verdict; the retry has come round
			service.recordRetryAfter("Done.Release", "a.rar", now.Add(-time.Minute))

			checker.ScanHook = func(data *nzbparser.NzbData, report filehealth.VerdictReport) []filehealth.FailedGroup {
				report(&data.Files[0], 0, test.reportPresent)
				return test.failed
			}

			service.periodicPass(context.Background(), config)

			if item := service.find("Done.Release"); item.Stage != test.wantStage {
				t.Errorf("the record sits at %q, want %q", item.Stage, test.wantStage)
			}
			verdict, ok := store.verdicts["Done.Release"]["a.rar"][0]
			if !ok {
				t.Fatal("the re-ask recorded no verdict")
			}
			if verdict.Present != test.reportPresent {
				t.Errorf("the re-asked segment is present=%v, want %v", verdict.Present, test.reportPresent)
			}
		})
	}
}

// Without a retry, a failed group is never re-probed, whatever its age.
func TestPeriodicPassNeverReprobesAFailedGroupWithoutARetry(t *testing.T) {
	store := newScanStore()
	now := time.Now()
	data := scanNzbData("Done.Release", twoSegmentFile("a.rar", now.Add(-72*time.Hour)))
	store.put(data, string(StageCompleted))

	checker := &scanChecker{}
	config := PeriodicScanConfig{Interval: time.Hour, Confidence: 0.99}
	service := scanService(t, store, checker, config)
	service.restore(store.records["Done.Release"])

	store.verdict("Done.Release", "a.rar", 0, false, now.Add(-time.Hour))

	service.periodicPass(context.Background(), config)

	call, ok := checker.scanned("Done.Release")
	if !ok {
		t.Fatal("the pass did not scan the record")
	}
	if got := call.known["a.rar"]; len(got) != 2 {
		t.Errorf("a settled group was told %v counts, want every position, so no probe is spent", got)
	}
	if item := service.find("Done.Release"); item.Stage != StageCompleted {
		t.Errorf("a scanned record that lost nothing new sits at %q", item.Stage)
	}
}

// A re-add of the same name while its scan runs is not the scan's record to
// mark, so the pass leaves it alone rather than moving a live add to failed.
func TestPeriodicPassLeavesAnItemThatIsNotTheRecordAlone(t *testing.T) {
	store := newScanStore()
	now := time.Now()
	data := scanNzbData("Done.Release", twoSegmentFile("a.rar", now.Add(-72*time.Hour)))
	store.put(data, string(StageCompleted))

	checker := &scanChecker{failed: []filehealth.FailedGroup{{Name: "a.rar", Files: []string{"a.rar"}}}}
	config := PeriodicScanConfig{Interval: time.Hour, Confidence: 0.99}
	service := scanService(t, store, checker, config)

	// No queue item holds the record, which is what an add that replaced the
	// record looks like to a scan still running
	service.periodicPass(context.Background(), config)

	if item := service.find("Done.Release"); item != nil {
		t.Errorf("an unknown item was marked: %+v", item)
	}
	if got := store.records["Done.Release"].Stage; got != string(StageCompleted) {
		t.Errorf("the pass moved a record it could not take ownership of to %q", got)
	}
}

// A service being torn down cancels the context the pass holds, and the tick
// answers by returning rather than scanning on.
func TestPeriodicPassStopsWhenTheContextIsCancelled(t *testing.T) {
	store := newScanStore()
	data := scanNzbData("Done.Release", twoSegmentFile("a.rar", time.Now().Add(-72*time.Hour)))
	store.put(data, string(StageCompleted))

	checker := &scanChecker{}
	config := PeriodicScanConfig{Interval: time.Hour, Confidence: 0.99}
	service := scanService(t, store, checker, config)
	service.restore(store.records["Done.Release"])

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	service.periodicPass(ctx, config)

	if len(checker.scans) != 0 {
		t.Errorf("a cancelled pass scanned %v", checker.scans)
	}
}

// The verdicts a pass reports are persisted, so a restart resumes at them, and
// a miss on a post younger than the minimum age is a retry rather than one.
func TestPeriodicPassRecordsItsVerdicts(t *testing.T) {
	store := newScanStore()
	now := time.Now()
	old := now.Add(-72 * time.Hour)
	young := now.Add(-time.Hour)
	data := scanNzbData("Done.Release",
		nzbparser.File{Filename: "a.rar", ParsedDate: old, Segments: []nzbparser.Segment{{ID: "a-1"}}},
		nzbparser.File{Filename: "b.rar", ParsedDate: young, Segments: []nzbparser.Segment{{ID: "b-1"}}},
	)
	store.put(data, string(StageCompleted))

	// A checker that reports one present and one missing verdict, the missing
	// one on the young post
	checker := &scanChecker{}
	config := PeriodicScanConfig{Interval: time.Hour, Confidence: 0.99, MinAge: 24 * time.Hour}
	service := scanService(t, store, checker, config)
	service.restore(store.records["Done.Release"])

	checker.ScanHook = func(data *nzbparser.NzbData, report filehealth.VerdictReport) []filehealth.FailedGroup {
		// The old post's miss is final; the young one is a retry, not a verdict
		report(&data.Files[0], 0, false)
		report(&data.Files[1], 0, false)
		return []filehealth.FailedGroup{
			{Name: "a.rar", Files: []string{"a.rar"}},
			{Name: "b.rar", Files: []string{"b.rar"}},
		}
	}

	service.periodicPass(context.Background(), config)

	if got, ok := store.verdicts["Done.Release"]["a.rar"][0]; !ok || got.Present {
		t.Errorf("the settled miss was recorded as %+v, want a present=false verdict", got)
	}
	if _, ok := store.verdicts["Done.Release"]["b.rar"][0]; ok {
		t.Errorf("the young miss was recorded as a verdict, want a retry only")
	}
	if after := store.retryAfter["Done.Release"]["b.rar"]; after.IsZero() {
		t.Errorf("the young miss recorded no retry")
	} else if want := young.Add(24 * time.Hour); !after.Equal(want) {
		t.Errorf("the young miss may be re-asked at %v, want %v", after, want)
	}
	if item := service.find("Done.Release"); item.Stage != StageFailed {
		t.Errorf("the record of a scan that lost a group sits at %q", item.Stage)
	}
}

// A failed record is revisited once a group's retry has come round, and only
// that group is asked anything: its re-ask settles the group for good, a
// propagated post leaves coverage behind, and the retry is spent either way.
// The record stays failed, with the error the add ended with.
func TestPeriodicPassReasksOnlyTheRetryGroupOfAFailedRecord(t *testing.T) {
	for _, test := range []struct {
		name          string
		reportPresent bool
		failed        []filehealth.FailedGroup
	}{
		{
			name:          "the post is gone for good",
			reportPresent: false,
			failed:        []filehealth.FailedGroup{{Name: "a.rar", Files: []string{"a.rar"}}},
		},
		{
			name:          "the post propagated",
			reportPresent: true,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			store := newScanStore()
			now := time.Now()
			data := scanNzbData("Done.Release",
				twoSegmentFile("a.rar", now.Add(-72*time.Hour)),
				twoSegmentFile("b.rar", now.Add(-72*time.Hour)))
			store.put(data, string(StageFailed))
			const addErr = "health check failed: 2 files beyond repair"
			record := store.records["Done.Release"]
			record.Err = addErr
			store.records["Done.Release"] = record

			checker := &scanChecker{}
			config := PeriodicScanConfig{Interval: time.Hour, Confidence: 0.99}
			service := scanService(t, store, checker, config)
			service.restore(store.records["Done.Release"])

			// A previous pass found the miss while the post was young and
			// recorded a retry rather than a verdict; it has come round. b.rar
			// was failed for an unrelated reason and its verdict is settled.
			service.recordRetryAfter("Done.Release", "a.rar", now.Add(-time.Minute))
			store.verdict("Done.Release", "b.rar", 0, false, now.Add(-time.Hour))

			checker.ScanHook = func(data *nzbparser.NzbData, report filehealth.VerdictReport) []filehealth.FailedGroup {
				report(&data.Files[0], 0, test.reportPresent)
				return test.failed
			}

			service.periodicPass(context.Background(), config)

			call, ok := checker.scanned("Done.Release")
			if !ok {
				t.Fatal("the pass did not re-ask the retry group")
			}
			if got := call.known["a.rar"]; len(got) != 0 {
				t.Errorf("the retry group was told %v counts, want nothing, so it is asked again", got)
			}
			if got := call.known["b.rar"]; len(got) != 2 {
				t.Errorf("a settled group of a failed record was told %v, want every position, so no probe is spent", got)
			}

			verdict, ok := store.verdicts["Done.Release"]["a.rar"][0]
			if !ok {
				t.Fatal("the re-ask recorded no verdict")
			}
			if verdict.Present != test.reportPresent {
				t.Errorf("the re-asked segment is present=%v, want %v", verdict.Present, test.reportPresent)
			}
			if _, ok := service.retryAfter["Done.Release"]["a.rar"]; ok {
				t.Errorf("the re-ask left the retry standing")
			}

			item := service.find("Done.Release")
			if item.Stage != StageFailed {
				t.Errorf("the record sits at %q, want the failed one the add ended with", item.Stage)
			}
			if item.Err != addErr {
				t.Errorf("the record reports %q, want the add's own error", item.Err)
			}
			if got := store.records["Done.Release"]; got.Stage != string(StageFailed) || got.Err != addErr {
				t.Errorf("the store holds %q/%q, want the add's own failure", got.Stage, got.Err)
			}
		})
	}
}

// A failed group whose verdict is already settled but whose files are still
// presented - a crash between the presenter removal and the store delete, or
// a verdict a read fed back - has its removal run again, which is idempotent,
// and the record says why.
func TestPeriodicPassRepullsASettledGroupStillPresented(t *testing.T) {
	store := newScanStore()
	now := time.Now()
	data := scanNzbData("Done.Release",
		twoSegmentFile("a.rar", now.Add(-72*time.Hour)),
		twoSegmentFile("b.rar", now.Add(-72*time.Hour)))
	store.put(data, string(StageCompleted))

	presenter := &scanPresenter{removed: []string{}}
	checker := &scanChecker{}
	config := PeriodicScanConfig{Interval: time.Hour, Confidence: 0.99}
	service := scanService(t, store, checker, config, presenter)
	service.restore(store.records["Done.Release"])

	tree := map[string]presentation.Openable{
		"Done.Release/a.mkv": scanFile{},
		"Done.Release/b.mkv": scanFile{},
	}
	service.register(data, tree)
	store.presented["Done.Release"] = []nzbstore.File{
		{Path: "Done.Release/a.mkv", Size: 42, Source: "a.rar"},
		{Path: "Done.Release/b.mkv", Size: 42, Source: "b.rar"},
	}

	// The verdict is settled; only the removal it earned was lost
	store.verdict("Done.Release", "a.rar", 0, false, now.Add(-time.Hour))

	service.periodicPass(context.Background(), config)

	if len(presenter.removed) != 1 || presenter.removed[0] != "Done.Release/a.mkv" {
		t.Errorf("the pass removed %v, want the settled group's still registered file", presenter.removed)
	}
	if got := store.presented["Done.Release"]; len(got) != 1 || got[0].Path != "Done.Release/b.mkv" {
		t.Errorf("the store holds %v, want only the surviving file", got)
	}
	if files := service.Files()["Done.Release"]; len(files) != 1 || files[0].Path != "Done.Release/b.mkv" {
		t.Errorf("the service still reports %v", files)
	}
	item := service.find("Done.Release")
	if item.Stage != StageFailed || !strings.Contains(item.Err, "a.rar") {
		t.Errorf("the record sits at %q with err %q, want failed with the settled group named", item.Stage, item.Err)
	}

	// A second pass has nothing left to pull and nothing left to say
	service.periodicPass(context.Background(), config)
	if len(presenter.removed) != 1 {
		t.Errorf("the pass removed %v again", presenter.removed)
	}
}

// The pass probes what the add would have presented: the nzb file blacklist is
// the add's decision, so a blacklisted file is never probed and never pulls
// presented files back over blacklisted junk.
func TestPeriodicPassProbesOnlyWhatTheBlacklistLeaves(t *testing.T) {
	store := newScanStore()
	old := time.Now().Add(-72 * time.Hour)
	data := scanNzbData("Done.Release",
		twoSegmentFile("a.rar", old),
		twoSegmentFile("sample.mkv", old))
	store.put(data, string(StageCompleted))

	var probed []string
	checker := &scanChecker{}
	checker.ScanHook = func(data *nzbparser.NzbData, _ filehealth.VerdictReport) []filehealth.FailedGroup {
		for _, file := range data.Files {
			probed = append(probed, file.Filename)
		}
		return nil
	}
	config := PeriodicScanConfig{Interval: time.Hour, Confidence: 0.99}
	service := scanService(t, store, checker, config)
	service.SetNzbFileBlacklist([]regexp.Regexp{*regexp.MustCompile(`^sample`)})
	service.restore(store.records["Done.Release"])

	service.periodicPass(context.Background(), config)

	if len(probed) != 1 || probed[0] != "a.rar" {
		t.Errorf("the pass probed %v, want only what the blacklist leaves", probed)
	}
	call, ok := checker.scanned("Done.Release")
	if !ok {
		t.Fatal("the pass did not scan the record")
	}
	if _, ok := call.known["sample.mkv"]; ok {
		t.Errorf("a blacklisted file was grouped for probing")
	}
	if _, ok := store.sourceFiles["Done.Release"]["sample.mkv"]; ok {
		t.Errorf("a blacklisted file was recorded as a source file")
	}
	if item := service.find("Done.Release"); item.Stage != StageCompleted {
		t.Errorf("a record whose scan probed nothing sits at %q", item.Stage)
	}
}
