package nzbservice_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"git.ruekov.eu/ruakij/nzbStreamer/internal/filehealth"
	"git.ruekov.eu/ruakij/nzbStreamer/internal/nzbrecordfactory"
	"git.ruekov.eu/ruakij/nzbStreamer/internal/nzbstore"
	"git.ruekov.eu/ruakij/nzbStreamer/internal/presentation"
	"git.ruekov.eu/ruakij/nzbStreamer/internal/service/nzbservice"
	"git.ruekov.eu/ruakij/nzbStreamer/pkg/nzbparser"
)

var (
	errBuildFailed = errors.New("build failed")
	errNoBytes     = errors.New("nothing behind this file")
)

type fakeFactory struct {
	err error
	// Reported alongside a usable tree, the way an archive left packed is
	packedErr error
	discarded []string
	// How often a tree was built, which is what a restore from the store is
	// supposed not to do
	builds atomic.Int64
	// Optional, to hold an add inside the build the way blockingChecker holds it
	// inside the health check
	entered chan struct{}
	release chan struct{}
}

func (f *fakeFactory) DiscardSegmentStackFromNzbData(nzbData *nzbparser.NzbData) {
	f.discarded = append(f.discarded, nzbData.MetaName)
}

func (f *fakeFactory) BuildSegmentStackFromNzbData(_ *nzbparser.NzbData, _ nzbrecordfactory.ProgressFunc) (nzbrecordfactory.BuildResult, error) {
	if f.entered != nil {
		close(f.entered)
		<-f.release
	}
	if f.err != nil {
		return nzbrecordfactory.BuildResult{}, f.err
	}
	f.builds.Add(1)
	return nzbrecordfactory.BuildResult{
		Presented: map[string]presentation.Openable{"file.mkv": fakeFile{}},
		SourceOf:  map[string]string{"file.mkv": "some.release.rar"},
	}, f.packedErr
}

// fakeFile is a file with nothing behind it: a tree is what these tests look at,
// never the bytes.
type fakeFile struct{}

func (fakeFile) SizeHint() (int64, error) { return 42, nil }

func (fakeFile) Open() (io.ReadSeekCloser, error) { return nil, errNoBytes }

type healthyChecker struct{}

func (healthyChecker) CheckFiles(_ context.Context, _ *nzbparser.NzbData, _ filehealth.VerdictReport, _ filehealth.ProgressFunc) []filehealth.FailedGroup {
	return nil
}

func (healthyChecker) Scan(_ context.Context, _ *nzbparser.NzbData, _ float64, _ map[string][]int, _ filehealth.VerdictReport, _ filehealth.ProgressFunc) []filehealth.FailedGroup {
	return nil
}

func (healthyChecker) PlannedProbes(_ *nzbparser.NzbData) int { return 0 }

// unhealthyChecker reports the groups the test says failed, so the add drops
// exactly those files and carries on with the rest.
type unhealthyChecker struct {
	groups []filehealth.FailedGroup
}

func (c unhealthyChecker) CheckFiles(_ context.Context, _ *nzbparser.NzbData, _ filehealth.VerdictReport, _ filehealth.ProgressFunc) []filehealth.FailedGroup {
	return c.groups
}

func (c unhealthyChecker) Scan(_ context.Context, _ *nzbparser.NzbData, _ float64, _ map[string][]int, _ filehealth.VerdictReport, _ filehealth.ProgressFunc) []filehealth.FailedGroup {
	return c.groups
}

func (unhealthyChecker) PlannedProbes(_ *nzbparser.NzbData) int { return 0 }

// verdictChecker answers a check by reporting the verdicts the test says, then
// failing the groups the test says, which is the shape a real check's answer
// takes: verdicts on the way, failed groups at the end.
type verdictChecker struct {
	report func(report filehealth.VerdictReport)
	groups []filehealth.FailedGroup
}

func (c verdictChecker) CheckFiles(_ context.Context, _ *nzbparser.NzbData, report filehealth.VerdictReport, _ filehealth.ProgressFunc) []filehealth.FailedGroup {
	if c.report != nil {
		c.report(report)
	}
	return c.groups
}

func (c verdictChecker) Scan(_ context.Context, _ *nzbparser.NzbData, _ float64, _ map[string][]int, report filehealth.VerdictReport, _ filehealth.ProgressFunc) []filehealth.FailedGroup {
	if c.report != nil {
		c.report(report)
	}
	return c.groups
}

func (verdictChecker) PlannedProbes(_ *nzbparser.NzbData) int { return 0 }

// filesFactory presents every file the nzb still names, by its own filename, so
// a health-drop test can tell which of them made it into the tree.
type filesFactory struct {
	discarded []string
}

func (f *filesFactory) DiscardSegmentStackFromNzbData(nzbData *nzbparser.NzbData) {
	f.discarded = append(f.discarded, nzbData.MetaName)
}

func (f *filesFactory) BuildSegmentStackFromNzbData(nzbData *nzbparser.NzbData, _ nzbrecordfactory.ProgressFunc) (nzbrecordfactory.BuildResult, error) {
	result := nzbrecordfactory.BuildResult{
		Presented: map[string]presentation.Openable{},
		SourceOf:  map[string]string{},
	}
	for _, file := range nzbData.Files {
		result.Presented[file.Filename] = fakeFile{}
		result.SourceOf[file.Filename] = file.Filename
	}
	return result, nil
}

// fakeStore keeps what the real one keeps, in a map. Locked because an add runs
// in the background and the test reads the store while it does.
type fakeStore struct {
	mutex     sync.Mutex
	records   map[string]nzbstore.Record
	presented map[string][]nzbstore.File
	order     []string
	// What the health rows hold, so a verdict recorded by the add path and a
	// rescan the test triggers read back through the same interface
	sourceFiles map[string]map[string]nzbstore.SourceFile
	retryAfter  map[string]map[string]time.Time
	verdicts    map[string]map[string]map[int]nzbstore.SegmentVerdict
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		records:     map[string]nzbstore.Record{},
		presented:   map[string][]nzbstore.File{},
		sourceFiles: map[string]map[string]nzbstore.SourceFile{},
		retryAfter:  map[string]map[string]time.Time{},
		verdicts:    map[string]map[string]map[int]nzbstore.SegmentVerdict{},
	}
}

func (s *fakeStore) List() ([]nzbstore.Record, error) {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	list := make([]nzbstore.Record, 0, len(s.records))
	for _, name := range s.order {
		if record, ok := s.records[name]; ok {
			list = append(list, record)
		}
	}
	return list, nil
}

func (s *fakeStore) Raw(name string) ([]byte, error) {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	record, ok := s.records[name]
	if !ok {
		return nil, fmt.Errorf("%w: %s", nzbstore.ErrNotFound, name)
	}
	return record.Data.Raw, nil
}

func (s *fakeStore) Add(data *nzbparser.NzbData, stage, category string) error {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	if _, ok := s.records[data.MetaName]; !ok {
		s.order = append(s.order, data.MetaName)
	}
	s.records[data.MetaName] = nzbstore.Record{Data: data, Stage: stage, Category: category}
	return nil
}

func (s *fakeStore) SetArchived(name string, archived bool) error {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	record, ok := s.records[name]
	if !ok {
		return nil
	}
	record.Archived = archived
	s.records[name] = record
	return nil
}

func (s *fakeStore) SetStage(name, stage, errMessage string) error {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	record, ok := s.records[name]
	if !ok {
		return nil
	}
	record.Stage, record.Err = stage, errMessage
	s.records[name] = record
	return nil
}

func (s *fakeStore) SetFiles(name, treeKey string, files []nzbstore.File) error {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	record, ok := s.records[name]
	if !ok {
		return nil
	}
	record.TreeKey = treeKey
	s.records[name] = record
	s.presented[name] = files
	return nil
}

func (s *fakeStore) Files(name string) ([]nzbstore.File, error) {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	return s.presented[name], nil
}

func (s *fakeStore) Delete(name string) error {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	delete(s.records, name)
	delete(s.presented, name)
	return nil
}

func (s *fakeStore) EnsureSourceFiles(nzbName string, files []nzbstore.SourceFile) error {
	s.mutex.Lock()
	defer s.mutex.Unlock()

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

func (s *fakeStore) SetRetryAfter(nzbName, filename string, after time.Time) error {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	if s.retryAfter[nzbName] == nil {
		s.retryAfter[nzbName] = map[string]time.Time{}
	}
	s.retryAfter[nzbName][filename] = after
	return nil
}

func (s *fakeStore) SegmentVerdicts(nzbName string, filenames []string) (map[string]map[int]nzbstore.SegmentVerdict, error) {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	verdicts := make(map[string]map[int]nzbstore.SegmentVerdict, len(filenames))
	for _, filename := range filenames {
		if index := s.verdicts[nzbName][filename]; len(index) > 0 {
			verdicts[filename] = index
		}
	}
	return verdicts, nil
}

func (s *fakeStore) RecordProbes(nzbName string, probes []nzbstore.ProbeResult) error {
	s.mutex.Lock()
	defer s.mutex.Unlock()

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

func (s *fakeStore) RemoveFiles(name string, paths []string) error {
	s.mutex.Lock()
	defer s.mutex.Unlock()

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

// aVerdict writes what a scan would have, so a test can stage store state the
// service did not produce in this run
func (s *fakeStore) aVerdict(nzbName, filename string, index int, present bool) {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	if s.verdicts[nzbName] == nil {
		s.verdicts[nzbName] = map[string]map[int]nzbstore.SegmentVerdict{}
	}
	if s.verdicts[nzbName][filename] == nil {
		s.verdicts[nzbName][filename] = map[int]nzbstore.SegmentVerdict{}
	}
	s.verdicts[nzbName][filename][index] = nzbstore.SegmentVerdict{
		Filename: filename, Index: index, Present: present, CheckedAt: time.Now(),
	}
}

func (s *fakeStore) stage(name string) string {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	return s.records[name].Stage
}

// blockingChecker holds an add inside the health check until it is released, so
// a test can look at the queue while something is in it
type blockingChecker struct {
	entered chan struct{}
	release chan struct{}
}

func (c blockingChecker) CheckFiles(_ context.Context, _ *nzbparser.NzbData, _ filehealth.VerdictReport, progress filehealth.ProgressFunc) []filehealth.FailedGroup {
	progress(1, 2)
	close(c.entered)
	<-c.release
	return nil
}

func (c blockingChecker) Scan(_ context.Context, _ *nzbparser.NzbData, _ float64, _ map[string][]int, _ filehealth.VerdictReport, progress filehealth.ProgressFunc) []filehealth.FailedGroup {
	progress(1, 2)
	close(c.entered)
	<-c.release
	return nil
}

func (blockingChecker) PlannedProbes(_ *nzbparser.NzbData) int { return 2 }

func TestAnAddIsVisibleWhileItRunsAndAfterItFinishes(t *testing.T) {
	checker := blockingChecker{entered: make(chan struct{}), release: make(chan struct{})}
	store := newFakeStore()
	service := nzbservice.NewService(store, &fakeFactory{}, nil, nil, checker)
	service.SetRate(func() float64 { return 10 })

	nzbData := &nzbparser.NzbData{
		MetaName: "Some.Release",
		Files:    []nzbparser.File{{Filename: "some.release.rar", Segments: []nzbparser.Segment{{ID: "a", BytesHint: 716800}}}},
	}

	id, err := service.Add(nzbData, "tv")
	if err != nil {
		t.Fatalf("Add: %v", err)
	}

	<-checker.entered
	queue := service.Queue()
	if len(queue) != 1 || queue[0].ID != id || queue[0].Stage != nzbservice.StageChecking {
		t.Fatalf("queue during the health check was %+v", queue)
	}
	// The hint is a known segment size, so it counts decoded bytes and the size
	// owes nothing to an estimate
	if queue[0].Bytes != 716800 || !queue[0].BytesExact {
		t.Errorf("queued item reported %d bytes, exact %v", queue[0].Bytes, queue[0].BytesExact)
	}
	// The check has reported one of its probes, and the build it has not reached
	// is still ahead of it, so the add is started but nowhere near done
	if queue[0].Progress <= 0 || queue[0].Progress >= 1 || queue[0].Eta <= 0 {
		t.Errorf("queued item reported progress %v, eta %v", queue[0].Progress, queue[0].Eta)
	}
	if len(service.History()) != 0 {
		t.Errorf("an unfinished add is already in the history")
	}

	if _, err := service.Add(nzbData, "tv"); !errors.Is(err, nzbservice.ErrNzbAlreadyExists) {
		t.Errorf("adding an nzb already in flight returned %v", err)
	}

	close(checker.release)

	history := waitForHistory(t, service)
	if history[0].ID != id || history[0].Stage != nzbservice.StageCompleted {
		t.Errorf("finished add was recorded as %+v", history[0])
	}
	if len(service.Queue()) != 0 {
		t.Errorf("finished add is still in the queue")
	}
	files := service.Files()
	if got := files[id]; len(got) != 1 || got[0].Path != "Some.Release/file.mkv" {
		t.Errorf("files are %v, want [Some.Release/file.mkv]", got)
	}

	if err := service.RemoveNzb(nzbData); err != nil {
		t.Fatalf("RemoveNzb: %v", err)
	}
	if len(service.History()) != 0 {
		t.Errorf("removed nzb is still in the history")
	}
}

// A cancel is answered at once, whatever the add is in the middle of, and the
// add tears down what it had got as far as building when it unwinds.
func TestCancellingAnAddIsAnsweredAtOnceAndLeavesNothingBehind(t *testing.T) {
	for _, test := range []struct {
		name string
		// Where the add is held while the cancel arrives
		hold func(*fakeFactory) (chan struct{}, chan struct{})
	}{
		{
			name: "before it builds anything",
			hold: nil,
		},
		{
			name: "after it has built",
			hold: func(f *fakeFactory) (chan struct{}, chan struct{}) {
				f.entered, f.release = make(chan struct{}), make(chan struct{})
				return f.entered, f.release
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			checker := blockingChecker{entered: make(chan struct{}), release: make(chan struct{})}
			factory := &fakeFactory{}
			store := newFakeStore()
			service := nzbservice.NewService(store, factory, nil, nil, checker)

			entered, release := checker.entered, checker.release
			if test.hold != nil {
				entered, release = test.hold(factory)
				close(checker.release)
			}

			nzbData := &nzbparser.NzbData{
				MetaName: "Some.Release",
				Files:    []nzbparser.File{{Filename: "some.release.rar"}},
			}

			id, err := service.Add(nzbData, "tv")
			if err != nil {
				t.Fatalf("Add: %v", err)
			}
			<-entered

			cancelled := make(chan error, 1)
			go func() { cancelled <- service.Cancel(id) }()

			// The add is held in a call nothing can interrupt, and the cancel is
			// answered anyway rather than waiting for it
			select {
			case err := <-cancelled:
				if err != nil {
					t.Fatalf("Cancel: %v", err)
				}
			case <-time.After(time.Second):
				t.Fatal("Cancel waited for an add that was still running")
			}

			// The store says cancelled before the add has unwound, so a restart
			// in between does not resume it
			if got := store.stage(id); got != string(nzbservice.StageCancelled) {
				t.Errorf("a cancelled add is recorded in the store as %q", got)
			}
			if queue := service.Queue(); len(queue) != 1 || queue[0].Stage != nzbservice.StageCancelling {
				t.Fatalf("an add still unwinding was reported as %+v", queue)
			}

			close(release)

			history := waitForHistory(t, service)
			if len(history) != 1 || history[0].Stage != nzbservice.StageCancelled {
				t.Fatalf("cancelled add was recorded as %+v", history)
			}
			// Once, however many hands the teardown passes through
			if len(factory.discarded) != 1 {
				t.Errorf("a cancelled add discarded its segment data %d times, want once", len(factory.discarded))
			}
			if files := service.Files(); len(files) != 0 {
				t.Errorf("a cancelled add is still presenting %v", files)
			}
		})
	}
}

func TestAFailedAddIsHistoryWithItsError(t *testing.T) {
	service := nzbservice.NewService(newFakeStore(), &fakeFactory{err: errBuildFailed}, nil, nil, healthyChecker{})

	nzbData := &nzbparser.NzbData{
		MetaName: "Some.Release",
		Files:    []nzbparser.File{{Filename: "some.release.rar"}},
	}

	if err := service.AddNzb(nzbData); !errors.Is(err, errBuildFailed) {
		t.Fatalf("AddNzb returned %v", err)
	}

	history := service.History()
	if len(history) != 1 || history[0].Stage != nzbservice.StageFailed {
		t.Fatalf("failed add was recorded as %+v", history)
	}
	if !strings.Contains(history[0].Err, errBuildFailed.Error()) {
		t.Errorf("failed add reported %q as its error", history[0].Err)
	}
}

// Wait is what a client api holds a request open on: it answers an add that
// ends inside the window with how it ended, and one that outlasts it with
// nothing, leaving it running to be polled for.
func TestWaitAnswersAnAddThatEndsAndGivesUpOnOneThatRuns(t *testing.T) {
	checker := blockingChecker{entered: make(chan struct{}), release: make(chan struct{})}
	service := nzbservice.NewService(newFakeStore(), &fakeFactory{err: errBuildFailed}, nil, nil, checker)

	nzbData := &nzbparser.NzbData{
		MetaName: "Some.Release",
		Files:    []nzbparser.File{{Filename: "some.release.rar"}},
	}

	if _, err := service.Add(nzbData, "tv"); err != nil {
		t.Fatalf("Add returned %v", err)
	}
	<-checker.entered

	if _, done := service.Wait("Some.Release", 10*time.Millisecond); done {
		t.Errorf("Wait answered an add that was still checking")
	}

	close(checker.release)

	item, done := service.Wait("Some.Release", time.Second)
	if !done {
		t.Fatalf("Wait gave up on an add that had ended")
	}
	if item.Stage != nzbservice.StageFailed || !strings.Contains(item.Err, errBuildFailed.Error()) {
		t.Errorf("Wait answered %+v", item)
	}

	if _, done := service.Wait("Never.Added", time.Second); done {
		t.Errorf("Wait answered an id nothing is tracking")
	}
}

// A release nothing could unpack is not one a client can import, so the add
// ends failed - with what it did build presented, for whoever wants to look at
// what was posted.
func TestAnArchiveLeftPackedFailsTheAddAndStillPresentsIt(t *testing.T) {
	presenter := &fakePresenter{files: map[string]presentation.Openable{}}
	packed := fmt.Errorf("%w: some.release.part.rar", nzbrecordfactory.ErrArchiveLeftPacked)
	service := nzbservice.NewService(newFakeStore(),
		&fakeFactory{packedErr: packed},
		[]presentation.Presenter{presenter}, nil, healthyChecker{})

	nzbData := &nzbparser.NzbData{
		MetaName: "Some.Release",
		Files:    []nzbparser.File{{Filename: "some.release.part01.rar"}},
	}

	if err := service.AddNzb(nzbData); !errors.Is(err, nzbrecordfactory.ErrArchiveLeftPacked) {
		t.Fatalf("AddNzb returned %v", err)
	}

	history := service.History()
	if len(history) != 1 || history[0].Stage != nzbservice.StageFailed {
		t.Fatalf("the add was recorded as %+v", history)
	}
	if !strings.Contains(history[0].Err, packed.Error()) {
		t.Errorf("the add reported %q as its error", history[0].Err)
	}

	if _, presented := presenter.files["Some.Release/file.mkv"]; !presented {
		t.Errorf("what the add did build is not presented, got %v", presenter.files)
	}
}

// A restart keeps every add reportable: the ones that ended come back as
// history, and the one the process died in the middle of runs again.
func TestARestartRestoresHistoryAndResumesAnInterruptedAdd(t *testing.T) {
	store := newFakeStore()
	for name, stage := range map[string]string{
		"Completed.Release":   string(nzbservice.StageCompleted),
		"Failed.Release":      string(nzbservice.StageFailed),
		"Interrupted.Release": string(nzbservice.StageChecking),
	} {
		data := &nzbparser.NzbData{
			MetaName: name,
			Files:    []nzbparser.File{{Filename: "some.release.rar"}},
		}
		if err := store.Add(data, stage, "tv"); err != nil {
			t.Fatalf("Add: %v", err)
		}
	}

	service := nzbservice.NewService(store, &fakeFactory{}, nil, nil, healthyChecker{})
	if err := service.Init(context.Background()); err != nil {
		t.Fatalf("Init: %v", err)
	}

	// A restored tree that has to be rebuilt is in the history as rebuilding
	// while it runs, so the stages are only settled once nothing is
	for range 100 {
		if len(service.History()) == 3 && service.Restoring() == 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	stages := map[string]nzbservice.Stage{}
	for _, item := range service.History() {
		stages[item.ID] = item.Stage
	}
	want := map[string]nzbservice.Stage{
		"Completed.Release":   nzbservice.StageCompleted,
		"Failed.Release":      nzbservice.StageFailed,
		"Interrupted.Release": nzbservice.StageCompleted,
	}
	for id, stage := range want {
		if stages[id] != stage {
			t.Errorf("%s came back as %q, want %q", id, stages[id], stage)
		}
	}
}

func waitForHistory(t *testing.T, service *nzbservice.Service) []nzbservice.QueueItem {
	t.Helper()

	for range 100 {
		if history := service.History(); len(history) > 0 {
			return history
		}
		time.Sleep(10 * time.Millisecond)
	}

	t.Fatal("add never reached the history")
	return nil
}

func TestFailedAddLeavesTheNzbAddable(t *testing.T) {
	factory := &fakeFactory{err: errBuildFailed}
	store := newFakeStore()
	service := nzbservice.NewService(store, factory, nil, nil, healthyChecker{})

	nzbData := &nzbparser.NzbData{
		MetaName: "Some.Release",
		Files:    []nzbparser.File{{Filename: "some.release.rar"}},
	}

	if err := service.AddNzb(nzbData); !errors.Is(err, errBuildFailed) {
		t.Fatalf("first add returned %v, expected the build error", err)
	}

	if got := store.stage(nzbData.MetaName); got != string(nzbservice.StageFailed) {
		t.Errorf("a failed add is recorded in the store as %q", got)
	}

	factory.err = nil
	if err := service.AddNzb(nzbData); err != nil {
		t.Fatalf("re-adding after a failed add returned %v", err)
	}

	if got := store.stage(nzbData.MetaName); got != string(nzbservice.StageCompleted) {
		t.Errorf("a successful add is recorded in the store as %q", got)
	}

	if err := service.AddNzb(nzbData); !errors.Is(err, nzbservice.ErrNzbAlreadyExists) {
		t.Errorf("adding an nzb twice returned %v, expected it to be rejected", err)
	}
	if got := store.stage(nzbData.MetaName); got != string(nzbservice.StageCompleted) {
		t.Errorf("the rejected add changed the record of the one that succeeded to %q", got)
	}
}

// A check that fails one group drops that group's files and presents the rest.
// The add is still an add: AddNzb swallows the sentinel so the caller is not
// refused the file, but the record ends failed with the count in Err.
func TestABeyondRepairGroupIsDroppedAndTheRestPresented(t *testing.T) {
	presenter := &fakePresenter{files: map[string]presentation.Openable{}}
	store := newFakeStore()
	checker := unhealthyChecker{groups: []filehealth.FailedGroup{{Name: "a.rar", Files: []string{"a.rar"}}}}
	service := nzbservice.NewService(store, &filesFactory{}, []presentation.Presenter{presenter}, nil, checker)

	nzbData := &nzbparser.NzbData{
		MetaName: "Some.Release",
		Files: []nzbparser.File{
			{Filename: "a.rar", Segments: []nzbparser.Segment{{ID: "a1"}}},
			{Filename: "b.mkv", Segments: []nzbparser.Segment{{ID: "b1"}}},
		},
	}

	if err := service.AddNzb(nzbData); err != nil {
		t.Fatalf("AddNzb returned %v, a partial add is still an add", err)
	}

	history := service.History()
	if len(history) != 1 || history[0].Stage != nzbservice.StageFailed {
		t.Fatalf("the partial add was recorded as %+v", history)
	}
	if !strings.Contains(history[0].Err, "beyond repair") {
		t.Errorf("the partial add reported %q, want it to mention beyond repair", history[0].Err)
	}
	if !history[0].HealthCheckFailed {
		t.Errorf("the partial add did not mark itself as health-check-failed")
	}
	if got := store.stage(nzbData.MetaName); got != string(nzbservice.StageFailed) {
		t.Errorf("the partial add is recorded in the store as %q", got)
	}

	if len(presenter.files) != 1 {
		t.Fatalf("presented %v, want only the surviving file", presenter.files)
	}
	for fullPath := range presenter.files {
		if strings.HasSuffix(fullPath, ".rar") {
			t.Errorf("the dropped group is still presented as %s", fullPath)
		}
	}
}

// When every file is beyond repair nothing is presented, the name is freed, and
// a later add of the same name succeeds - the same re-add pattern a failed build
// leaves behind.
func TestOnlyFailingFilesLeaveNothingPresentedAndTheNameFree(t *testing.T) {
	presenter := &fakePresenter{files: map[string]presentation.Openable{}}
	store := newFakeStore()
	checker := unhealthyChecker{groups: []filehealth.FailedGroup{{Name: "a.rar", Files: []string{"a.rar"}}}}
	service := nzbservice.NewService(store, &filesFactory{}, []presentation.Presenter{presenter}, nil, checker)

	nzbData := &nzbparser.NzbData{
		MetaName: "Some.Release",
		Files:    []nzbparser.File{{Filename: "a.rar", Segments: []nzbparser.Segment{{ID: "a1"}}}},
	}

	if err := service.AddNzb(nzbData); err != nil {
		t.Fatalf("AddNzb returned %v, the sentinel is swallowed", err)
	}

	if got := store.stage(nzbData.MetaName); got != string(nzbservice.StageFailed) {
		t.Errorf("the failed add is recorded in the store as %q", got)
	}
	if len(service.Files()) != 0 || len(presenter.files) != 0 {
		t.Errorf("nothing should have been presented, got %v", presenter.files)
	}
	history := service.History()
	if len(history) != 1 || history[0].Stage != nzbservice.StageFailed {
		t.Fatalf("the failed add was recorded as %+v", history)
	}

	// The name was freed, so the same add succeeds
	checker.groups = nil
	if err := service.AddNzb(nzbData); err != nil {
		t.Fatalf("re-adding after nothing was presented returned %v", err)
	}
	if got := store.stage(nzbData.MetaName); got != string(nzbservice.StageCompleted) {
		t.Errorf("a successful re-add is recorded in the store as %q", got)
	}
}

func TestRemovingAnNzbDiscardsWhatItAccumulated(t *testing.T) {
	factory := &fakeFactory{}
	store := newFakeStore()
	service := nzbservice.NewService(store, factory, nil, nil, healthyChecker{})

	nzbData := &nzbparser.NzbData{
		MetaName: "Some.Release",
		Files:    []nzbparser.File{{Filename: "some.release.rar"}},
	}

	if err := service.AddNzb(nzbData); err != nil {
		t.Fatalf("AddNzb: %v", err)
	}
	if err := service.RemoveNzb(nzbData); err != nil {
		t.Fatalf("RemoveNzb: %v", err)
	}

	if len(factory.discarded) != 1 || factory.discarded[0] != nzbData.MetaName {
		t.Errorf("removal left the segment data behind: %v", factory.discarded)
	}
	if got := store.stage(nzbData.MetaName); got != "" {
		t.Errorf("removal left the nzb in the store as %q", got)
	}
}

// fakePresenter keeps what it was handed, so a test can open a file the way a
// mount would.
type fakePresenter struct {
	mutex sync.Mutex
	files map[string]presentation.Openable
}

func (p *fakePresenter) AddFile(fullpath string, _ time.Time, openable presentation.Openable) error {
	p.mutex.Lock()
	defer p.mutex.Unlock()

	p.files[fullpath] = openable
	return nil
}

func (p *fakePresenter) RemoveFile(fullpath string) error {
	p.mutex.Lock()
	defer p.mutex.Unlock()

	delete(p.files, fullpath)
	return nil
}

// A restart lists what the store recorded an nzb presenting and builds nothing
// for it; the read that wants bytes is what walks the archive.
func TestARestoredTreeIsListedFromTheStoreAndBuiltOnTheFirstRead(t *testing.T) {
	for _, test := range []struct {
		name string
		// What the process is configured with, against the "settings" the tree
		// was stored under
		key             string
		buildsOnRestore int64
	}{
		{name: "under the settings it was stored with", key: "settings", buildsOnRestore: 0},
		{name: "under changed settings", key: "other", buildsOnRestore: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			store := newFakeStore()
			factory := &fakeFactory{}
			presenter := &fakePresenter{files: map[string]presentation.Openable{}}
			service := nzbservice.NewService(store, factory, []presentation.Presenter{presenter}, nil, healthyChecker{})
			service.SetTreeKey(test.key)

			nzbData := &nzbparser.NzbData{
				MetaName: "Some.Release",
				Files:    []nzbparser.File{{Filename: "some.release.rar"}},
			}
			if err := store.Add(nzbData, string(nzbservice.StageCompleted), "tv"); err != nil {
				t.Fatalf("Add: %v", err)
			}
			if err := store.SetFiles(nzbData.MetaName, "settings", []nzbstore.File{
				{Path: "Some.Release/file.mkv", Size: 42, Exact: true},
			}); err != nil {
				t.Fatalf("SetFiles: %v", err)
			}

			if err := service.Init(context.Background()); err != nil {
				t.Fatalf("Init: %v", err)
			}

			if got := factory.builds.Load(); got != test.buildsOnRestore {
				t.Errorf("the restore built %d trees, want %d", got, test.buildsOnRestore)
			}

			file, listed := presenter.files["Some.Release/file.mkv"]
			if !listed {
				t.Fatalf("the restore listed %v", presenter.files)
			}
			if size, err := file.SizeHint(); err != nil || size != 42 {
				t.Errorf("the listed size is %d (%v), want 42", size, err)
			}

			// Opening reaches the built file either way; on a restored tree it is
			// what builds it
			if _, err := file.Open(); !errors.Is(err, errNoBytes) {
				t.Fatalf("opening the file returned %v", err)
			}
			if got := factory.builds.Load(); got != 1 {
				t.Errorf("after a read %d trees were built, want 1", got)
			}
		})
	}
}

// Archiving is what a download client means by removing a finished download: it
// has imported what it wanted and the record is in its way. Here the tree it
// imported from is the library, so the files stay.
func TestArchivingKeepsTheFilesPresentedAndSurvivesARestart(t *testing.T) {
	store := newFakeStore()
	factory := &fakeFactory{}
	presenter := &fakePresenter{files: map[string]presentation.Openable{}}
	service := nzbservice.NewService(store, factory, []presentation.Presenter{presenter}, nil, healthyChecker{})

	nzbData := &nzbparser.NzbData{
		MetaName: "Some.Release",
		Files:    []nzbparser.File{{Filename: "some.release.rar"}},
	}
	if _, err := service.Add(nzbData, "tv"); err != nil {
		t.Fatalf("Add: %v", err)
	}
	waitForHistory(t, service)

	if err := service.Archive(nzbData.MetaName, true); err != nil {
		t.Fatalf("Archive: %v", err)
	}

	history := service.History()
	if len(history) != 1 || !history[0].Archived {
		t.Fatalf("history after archiving was %+v", history)
	}
	if len(presenter.files) == 0 {
		t.Error("archiving took the files out of the presenter")
	}
	if len(factory.discarded) != 0 {
		t.Errorf("archiving discarded the segment data: %v", factory.discarded)
	}

	restarted := nzbservice.NewService(store, &fakeFactory{}, []presentation.Presenter{presenter}, nil, healthyChecker{})
	if err := restarted.Init(context.Background()); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if history := restarted.History(); len(history) != 1 || !history[0].Archived {
		t.Errorf("history after a restart was %+v", history)
	}

	if err := service.Archive(nzbData.MetaName, false); err != nil {
		t.Fatalf("Archive back: %v", err)
	}
	if history := service.History(); history[0].Archived {
		t.Error("restoring left the item archived")
	}
}

// A miss on a post younger than the minimum age still fails the group and
// drops it, but settles nothing: it becomes a retry, which is what leaves the
// background pass able to re-ask once the post is old enough.
func TestAMissOnAYoungPostIsARetryNotAVerdict(t *testing.T) {
	presenter := &fakePresenter{files: map[string]presentation.Openable{}}
	store := newFakeStore()
	young := time.Now().Add(-time.Hour)
	nzbData := &nzbparser.NzbData{
		MetaName: "Some.Release",
		Files: []nzbparser.File{
			{Filename: "a.rar", ParsedDate: young, Segments: []nzbparser.Segment{{ID: "a1"}, {ID: "a2"}}},
			{Filename: "b.mkv", ParsedDate: young, Segments: []nzbparser.Segment{{ID: "b1"}}},
		},
	}
	checker := verdictChecker{
		report: func(report filehealth.VerdictReport) {
			// One position of the a.rar group is present, one is missing
			report(&nzbData.Files[0], 0, true)
			report(&nzbData.Files[0], 1, false)
		},
		groups: []filehealth.FailedGroup{{Name: "a.rar", Files: []string{"a.rar"}}},
	}
	service := nzbservice.NewService(store, &filesFactory{}, []presentation.Presenter{presenter}, nil, checker)
	service.SetPeriodicScan(checker, nzbservice.PeriodicScanConfig{Interval: time.Hour, MinAge: 24 * time.Hour, Confidence: 0.99})

	if err := service.AddNzb(nzbData); err != nil {
		t.Fatalf("AddNzb: %v", err)
	}

	history := service.History()
	if len(history) != 1 || history[0].Stage != nzbservice.StageFailed || !history[0].HealthCheckFailed {
		t.Fatalf("the partial add was recorded as %+v", history)
	}
	if _, presented := presenter.files["Some.Release/b.mkv"]; !presented {
		t.Errorf("the surviving file was not presented, got %v", presenter.files)
	}
	if len(presenter.files) != 1 {
		t.Errorf("the failed group was presented: %v", presenter.files)
	}

	// The miss on the young post is no verdict: it left a retry instead
	if _, ok := store.verdicts["Some.Release"]["a.rar"][1]; ok {
		t.Errorf("the young miss was recorded as a verdict")
	}
	if after := store.retryAfter["Some.Release"]["a.rar"]; after.IsZero() {
		t.Errorf("the young miss recorded no retry")
	} else if want := young.Add(24 * time.Hour); !after.Equal(want) {
		t.Errorf("the young miss may be re-asked at %v, want %v", after, want)
	}
	// The present verdict stands whatever the post's age
	if got, ok := store.verdicts["Some.Release"]["a.rar"][0]; !ok || !got.Present {
		t.Errorf("the confirmed segment was recorded as %+v, want present=true", got)
	}
}

// A miss on a post old enough is a verdict, so the loss is durable.
func TestAMissOnAnOldPostIsAVerdict(t *testing.T) {
	store := newFakeStore()
	old := time.Now().Add(-48 * time.Hour)
	nzbData := &nzbparser.NzbData{
		MetaName: "Some.Release",
		Files:    []nzbparser.File{{Filename: "a.rar", ParsedDate: old, Segments: []nzbparser.Segment{{ID: "a1"}}}},
	}
	checker := verdictChecker{
		report: func(report filehealth.VerdictReport) { report(&nzbData.Files[0], 0, false) },
		groups: []filehealth.FailedGroup{{Name: "a.rar", Files: []string{"a.rar"}}},
	}
	service := nzbservice.NewService(store, &filesFactory{}, nil, nil, checker)
	service.SetPeriodicScan(checker, nzbservice.PeriodicScanConfig{Interval: time.Hour, MinAge: 24 * time.Hour, Confidence: 0.99})

	if err := service.AddNzb(nzbData); err != nil {
		t.Fatalf("AddNzb: %v", err)
	}

	verdict, ok := store.verdicts["Some.Release"]["a.rar"][0]
	if !ok || verdict.Present {
		t.Errorf("the settled miss was recorded as %+v, want a present=false verdict", verdict)
	}
	if after := store.retryAfter["Some.Release"]["a.rar"]; !after.IsZero() {
		t.Errorf("a settled miss left a retry at %v", after)
	}
	if got := store.stage("Some.Release"); got != string(nzbservice.StageFailed) {
		t.Errorf("the add is recorded as %q", got)
	}
}

// A verdict that failed a group outlives the process that made it: a build
// over the raw nzb drops the group's files again, so a restart or a rebuild
// does not re-present what a scan pulled back.
func TestARebuildDropsWhatAVerdictFailed(t *testing.T) {
	presenter := &fakePresenter{files: map[string]presentation.Openable{}}
	store := newFakeStore()
	nzbData := &nzbparser.NzbData{
		MetaName: "Some.Release",
		Files: []nzbparser.File{
			{Filename: "a.rar", Segments: []nzbparser.Segment{{ID: "a1"}}},
			{Filename: "b.mkv", Segments: []nzbparser.Segment{{ID: "b1"}}},
		},
	}
	if err := store.Add(nzbData, string(nzbservice.StageCompleted), "tv"); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := store.SetFiles("Some.Release", "settings", []nzbstore.File{
		{Path: "Some.Release/b.mkv", Size: 42, Exact: true, Source: "b.mkv"},
	}); err != nil {
		t.Fatalf("SetFiles: %v", err)
	}
	// A scan found a.rar's segment missing and pulled its path back; the
	// verdict is what remains of it
	store.aVerdict("Some.Release", "a.rar", 0, false)

	service := nzbservice.NewService(store, &filesFactory{}, []presentation.Presenter{presenter}, nil, healthyChecker{})
	service.SetTreeKey("settings")
	if err := service.Init(context.Background()); err != nil {
		t.Fatalf("Init: %v", err)
	}

	// Opening the surviving file builds the tree from the raw nzb, which the
	// verdict keeps to the survivors
	file, listed := presenter.files["Some.Release/b.mkv"]
	if !listed {
		t.Fatalf("the restored tree lists %v", presenter.files)
	}
	if _, err := file.Open(); !errors.Is(err, errNoBytes) {
		t.Fatalf("opening the file returned %v", err)
	}
	if len(presenter.files) != 1 {
		t.Errorf("the rebuild re-presented the failed group: %v", presenter.files)
	}
}
