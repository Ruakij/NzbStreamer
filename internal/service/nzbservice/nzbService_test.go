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
	err error
}

func (f *fakeFactory) BuildSegmentStackFromNzbData(_ *nzbparser.NzbData) (map[string]presentation.Openable, error) {
	if f.err != nil {
		return nil, f.err
	}
	return map[string]presentation.Openable{"file.mkv": nil}, nil
}

type healthyChecker struct{}

func (healthyChecker) CheckFiles(_ *nzbparser.NzbData) []error { return nil }

func TestFailedAddLeavesTheNzbAddable(t *testing.T) {
	factory := &fakeFactory{err: errBuildFailed}
	service := nzbservice.NewService(nil, factory, nil, nil, healthyChecker{})

	nzbData := &nzbparser.NzbData{
		MetaName: "Some.Release",
		Files:    []nzbparser.File{{Filename: "some.release.rar"}},
	}

	if err := service.AddNzb(nzbData); !errors.Is(err, errBuildFailed) {
		t.Fatalf("first add returned %v, expected the build error", err)
	}

	factory.err = nil
	if err := service.AddNzb(nzbData); err != nil {
		t.Fatalf("re-adding after a failed add returned %v", err)
	}

	if err := service.AddNzb(nzbData); !errors.Is(err, nzbservice.ErrNzbAlreadyExists) {
		t.Errorf("adding an nzb twice returned %v, expected it to be rejected", err)
	}
}
