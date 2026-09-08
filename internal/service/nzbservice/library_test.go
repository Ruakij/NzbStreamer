package nzbservice_test

import (
	"errors"
	"testing"

	"git.ruekov.eu/ruakij/nzbStreamer/internal/service/nzbservice"
	"git.ruekov.eu/ruakij/nzbStreamer/pkg/nzbparser"
)

func sizedNzb(name string) *nzbparser.NzbData {
	return &nzbparser.NzbData{
		MetaName: name,
		Files: []nzbparser.File{{
			Filename: "some.release.rar",
			Groups:   []string{"alt.binaries.test"},
			Segments: []nzbparser.Segment{{ID: name + "@example.com", Index: 1, BytesHint: 716800}},
		}},
	}
}

func TestAFullLibraryRefusesFurtherAdds(t *testing.T) {
	service := nzbservice.NewService(newFakeStore(), &fakeFactory{}, nil, nil, healthyChecker{})

	if _, err := service.Add(sizedNzb("First.Release"), "tv"); err != nil {
		t.Fatalf("Add: %v", err)
	}

	library := service.Library()
	if library.Nzbs != 1 || library.Bytes == 0 {
		t.Fatalf("library after one add: %+v", library)
	}

	// Exactly what is already there, so anything further is refused
	service.SetMaxLibraryBytes(library.Bytes)

	_, err := service.Add(sizedNzb("Second.Release"), "tv")
	if !errors.Is(err, nzbservice.ErrLibraryFull) {
		t.Fatalf("the add past the limit returned %v, want ErrLibraryFull", err)
	}
	if library = service.Library(); library.Nzbs != 1 || !library.Full() {
		t.Errorf("library after the refused add: %+v", library)
	}

	// Room again, and the same nzb goes in
	service.SetMaxLibraryBytes(0)
	if _, err := service.Add(sizedNzb("Second.Release"), "tv"); err != nil {
		t.Fatalf("Add with the limit lifted: %v", err)
	}
}
