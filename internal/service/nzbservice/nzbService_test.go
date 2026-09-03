package nzbservice_test

import (
	"errors"
	"testing"

	"git.ruekov.eu/ruakij/nzbStreamer/internal/presentation"
	"git.ruekov.eu/ruakij/nzbStreamer/internal/service/nzbservice"
	"git.ruekov.eu/ruakij/nzbStreamer/pkg/nzbparser"
)

var errBuildFailed = errors.New("build failed")

type fakeFactory struct {
	err       error
	discarded []string
}

func (f *fakeFactory) DiscardSegmentStackFromNzbData(nzbData *nzbparser.NzbData) {
	f.discarded = append(f.discarded, nzbData.MetaName)
}

func (f *fakeFactory) BuildSegmentStackFromNzbData(_ *nzbparser.NzbData) (map[string]presentation.Openable, error) {
	if f.err != nil {
		return nil, f.err
	}
	return map[string]presentation.Openable{"file.mkv": nil}, nil
}

type healthyChecker struct{}

func (healthyChecker) CheckFiles(_ *nzbparser.NzbData) []error { return nil }

type fakeStore struct {
	stored map[string]bool
}

func (s *fakeStore) List() ([]nzbparser.NzbData, error) { return nil, nil }

func (s *fakeStore) Set(data *nzbparser.NzbData) error {
	s.stored[data.MetaName] = true
	return nil
}

func (s *fakeStore) Delete(data *nzbparser.NzbData) error {
	delete(s.stored, data.MetaName)
	return nil
}

func TestFailedAddLeavesTheNzbAddable(t *testing.T) {
	factory := &fakeFactory{err: errBuildFailed}
	store := &fakeStore{stored: map[string]bool{}}
	service := nzbservice.NewService(store, factory, nil, nil, healthyChecker{})

	nzbData := &nzbparser.NzbData{
		MetaName: "Some.Release",
		Files:    []nzbparser.File{{Filename: "some.release.rar"}},
	}

	if err := service.AddNzb(nzbData); !errors.Is(err, errBuildFailed) {
		t.Fatalf("first add returned %v, expected the build error", err)
	}

	if store.stored[nzbData.MetaName] {
		t.Errorf("a failed add was persisted")
	}

	factory.err = nil
	if err := service.AddNzb(nzbData); err != nil {
		t.Fatalf("re-adding after a failed add returned %v", err)
	}

	if !store.stored[nzbData.MetaName] {
		t.Errorf("a successful add was not persisted")
	}

	if err := service.AddNzb(nzbData); !errors.Is(err, nzbservice.ErrNzbAlreadyExists) {
		t.Errorf("adding an nzb twice returned %v, expected it to be rejected", err)
	}
}

func TestRemovingAnNzbDiscardsWhatItAccumulated(t *testing.T) {
	factory := &fakeFactory{}
	store := &fakeStore{stored: map[string]bool{}}
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
	if store.stored[nzbData.MetaName] {
		t.Errorf("removal left the nzb in the store")
	}
}
