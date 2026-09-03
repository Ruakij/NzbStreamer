package nzbservice_test

import (
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"git.ruekov.eu/ruakij/nzbStreamer/internal/nzbstore"
	"git.ruekov.eu/ruakij/nzbStreamer/internal/presentation"
	"git.ruekov.eu/ruakij/nzbStreamer/internal/service/nzbservice"
	"git.ruekov.eu/ruakij/nzbStreamer/pkg/nzbparser"
)

var errBuildFailed = errors.New("build failed")

type fakeFactory struct {
	err       error
	discarded []string
	// Optional, to hold an add inside the build the way blockingChecker holds it
	// inside the health check
	entered chan struct{}
	release chan struct{}
}

func (f *fakeFactory) DiscardSegmentStackFromNzbData(nzbData *nzbparser.NzbData) {
	f.discarded = append(f.discarded, nzbData.MetaName)
}

func (f *fakeFactory) BuildSegmentStackFromNzbData(_ *nzbparser.NzbData) (map[string]presentation.Openable, error) {
	if f.entered != nil {
		close(f.entered)
		<-f.release
	}
	if f.err != nil {
		return nil, f.err
	}
	return map[string]presentation.Openable{"file.mkv": nil}, nil
}

type healthyChecker struct{}

func (healthyChecker) CheckFiles(_ *nzbparser.NzbData) []error { return nil }

// fakeStore keeps what the real one keeps, in a map. Locked because an add runs
// in the background and the test reads the store while it does.
type fakeStore struct {
	mutex   sync.Mutex
	records map[string]nzbstore.Record
	order   []string
}

func newFakeStore() *fakeStore {
	return &fakeStore{records: map[string]nzbstore.Record{}}
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

func (s *fakeStore) Add(data *nzbparser.NzbData, stage string) error {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	if _, ok := s.records[data.MetaName]; !ok {
		s.order = append(s.order, data.MetaName)
	}
	s.records[data.MetaName] = nzbstore.Record{Data: data, Stage: stage}
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

func (s *fakeStore) Delete(name string) error {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	delete(s.records, name)
	return nil
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

func (c blockingChecker) CheckFiles(_ *nzbparser.NzbData) []error {
	close(c.entered)
	<-c.release
	return nil
}

func TestAnAddIsVisibleWhileItRunsAndAfterItFinishes(t *testing.T) {
	checker := blockingChecker{entered: make(chan struct{}), release: make(chan struct{})}
	store := newFakeStore()
	service := nzbservice.NewService(store, &fakeFactory{}, nil, nil, checker)

	nzbData := &nzbparser.NzbData{
		MetaName: "Some.Release",
		Files:    []nzbparser.File{{Filename: "some.release.rar", Segments: []nzbparser.Segment{{ID: "a", BytesHint: 700000}}}},
	}

	id, err := service.Add(nzbData)
	if err != nil {
		t.Fatalf("Add: %v", err)
	}

	<-checker.entered
	queue := service.Queue()
	if len(queue) != 1 || queue[0].ID != id || queue[0].Stage != nzbservice.StageChecking {
		t.Fatalf("queue during the health check was %+v", queue)
	}
	if queue[0].Bytes != 700000 {
		t.Errorf("queued item reported %d bytes", queue[0].Bytes)
	}
	if len(service.History()) != 0 {
		t.Errorf("an unfinished add is already in the history")
	}

	if _, err := service.Add(nzbData); !errors.Is(err, nzbservice.ErrNzbAlreadyExists) {
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

	if err := service.RemoveNzb(nzbData); err != nil {
		t.Fatalf("RemoveNzb: %v", err)
	}
	if len(service.History()) != 0 {
		t.Errorf("removed nzb is still in the history")
	}
}

// A cancel is answered at the next stage boundary if there is one, and by
// removing the finished add if there is not. Both end in the same place.
func TestCancellingAnAddWaitsForItAndLeavesNothingBehind(t *testing.T) {
	for _, test := range []struct {
		name string
		// Where the add is held while the cancel arrives, and whether it gets
		// far enough to build anything
		hold  func(*fakeFactory) (chan struct{}, chan struct{})
		built bool
	}{
		{
			name:  "before it builds anything",
			hold:  nil,
			built: false,
		},
		{
			name: "after it has built",
			hold: func(f *fakeFactory) (chan struct{}, chan struct{}) {
				f.entered, f.release = make(chan struct{}), make(chan struct{})
				return f.entered, f.release
			},
			built: true,
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

			id, err := service.Add(nzbData)
			if err != nil {
				t.Fatalf("Add: %v", err)
			}
			<-entered

			cancelled := make(chan error, 1)
			go func() { cancelled <- service.Cancel(id) }()

			select {
			case err := <-cancelled:
				t.Fatalf("Cancel returned %v while the add was still running", err)
			case <-time.After(20 * time.Millisecond):
			}

			close(release)

			if err := <-cancelled; err != nil {
				t.Fatalf("Cancel: %v", err)
			}

			history := service.History()
			if len(history) != 1 || history[0].Stage != nzbservice.StageCancelled {
				t.Fatalf("cancelled add was recorded as %+v", history)
			}
			if got := store.stage(id); got != string(nzbservice.StageCancelled) {
				t.Errorf("a cancelled add is recorded in the store as %q", got)
			}
			if built := len(factory.discarded) == 1; test.built && !built {
				t.Errorf("a cancelled add left its segment data behind")
			} else if !test.built && built {
				t.Errorf("a cancel caught before the build discarded something anyway")
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
		if err := store.Add(data, stage); err != nil {
			t.Fatalf("Add: %v", err)
		}
	}

	service := nzbservice.NewService(store, &fakeFactory{}, nil, nil, healthyChecker{})
	if err := service.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}

	for range 100 {
		if len(service.History()) == 3 {
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
