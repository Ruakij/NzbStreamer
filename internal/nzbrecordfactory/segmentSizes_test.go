package nzbrecordfactory

import (
	"io"
	"testing"

	"git.ruekov.eu/ruakij/nzbStreamer/internal/nzbfileanalyzer"
	"git.ruekov.eu/ruakij/nzbStreamer/pkg/nzbparser"
	"git.ruekov.eu/ruakij/nzbStreamer/pkg/resource"
)

type fakeSizeStore struct {
	known    map[string]int64
	recorded map[string]int64
}

func (s *fakeSizeStore) SegmentSizes(ids []string) (map[string]int64, error) {
	sizes := make(map[string]int64)
	for _, id := range ids {
		if size, ok := s.known[id]; ok {
			sizes[id] = size
		}
	}
	return sizes, nil
}

func (s *fakeSizeStore) RecordSegmentSize(messageID string, size int64) {
	s.recorded[messageID] = size
}

func (s *fakeSizeStore) ForgetSegments(ids []string) error {
	for _, id := range ids {
		delete(s.known, id)
		delete(s.recorded, id)
	}
	return nil
}

func TestAKnownSizeIsExactWithoutFetching(t *testing.T) {
	store := &fakeSizeStore{
		known:    map[string]int64{"a@example.com": 700000},
		recorded: map[string]int64{},
	}
	getSegment := func(string, string) ([]byte, error) {
		t.Fatal("a known size must not cost a fetch")
		return nil, nil
	}

	factory := NewNzbFileFactory(nil, getSegment, store)
	nzbData := &nzbparser.NzbData{Files: []nzbparser.File{{
		Groups:   []string{"alt.binaries.test"},
		Segments: []nzbparser.Segment{{ID: "a@example.com", BytesHint: 999999}},
	}}}

	segment := factory.BuildResourceFromNzbSegment(
		&nzbData.Files[0].Segments[0], "alt.binaries.test",
		nzbfileanalyzer.NewSegmentSizer(nzbData), factory.knownSizes(nzbData),
	)

	size, err := segment.Size()
	if err != nil {
		t.Fatalf("Size: %v", err)
	}
	if size != 700000 {
		t.Errorf("size: got %d, want the stored 700000", size)
	}
}

func TestFetchingRecordsTheDecodedLength(t *testing.T) {
	store := &fakeSizeStore{known: map[string]int64{}, recorded: map[string]int64{}}
	getSegment := func(string, string) ([]byte, error) {
		return make([]byte, 4242), nil
	}

	factory := NewNzbFileFactory(nil, getSegment, store)
	nzbData := &nzbparser.NzbData{Files: []nzbparser.File{{
		Groups:   []string{"alt.binaries.test"},
		Segments: []nzbparser.Segment{{ID: "a@example.com", BytesHint: 999999}},
	}}}

	segment := factory.BuildResourceFromNzbSegment(
		&nzbData.Files[0].Segments[0], "alt.binaries.test",
		nzbfileanalyzer.NewSegmentSizer(nzbData), nil,
	)

	if _, err := segment.Size(); err == nil {
		t.Error("an unmeasured segment of an unknown-convention nzb reported an exact size")
	} else if err != resource.ErrSizeNotExact { //nolint:errorlint // the sentinel is returned unwrapped
		t.Fatalf("Size: %v", err)
	}

	reader, err := segment.Open()
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer reader.Close()
	if _, err := io.ReadAll(reader); err != nil {
		t.Fatalf("ReadAll: %v", err)
	}

	if store.recorded["a@example.com"] != 4242 {
		t.Errorf("recorded: got %v, want the decoded 4242", store.recorded)
	}
}
