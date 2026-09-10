package nzbrecordfactory

import (
	"io"
	"testing"
	"time"

	"git.ruekov.eu/ruakij/nzbStreamer/internal/nzbfileanalyzer"
	"git.ruekov.eu/ruakij/nzbStreamer/internal/nzbstore"
	"git.ruekov.eu/ruakij/nzbStreamer/pkg/nzbparser"
	"git.ruekov.eu/ruakij/nzbStreamer/pkg/resource"
)

// segmentRef is where the factory keys its reports: the nzb's own file, and
// the position within it.
type segmentRef struct {
	filename string
	index    int
}

type fakeSizeStore struct {
	known    map[string]int64
	recorded map[segmentRef]int64
	reads    map[segmentRef]int
	ensured  map[string][]string

	probes   chan nzbstore.ProbeResult
	probeErr error
}

func newFakeSizeStore() *fakeSizeStore {
	return &fakeSizeStore{
		known:    map[string]int64{},
		recorded: map[segmentRef]int64{},
		reads:    map[segmentRef]int{},
		ensured:  map[string][]string{},
		probes:   make(chan nzbstore.ProbeResult, 16),
	}
}

func (s *fakeSizeStore) SegmentSizes(_ string, ids []string) (map[string]int64, error) {
	sizes := make(map[string]int64)
	for _, id := range ids {
		if size, ok := s.known[id]; ok {
			sizes[id] = size
		}
	}
	return sizes, nil
}

func (s *fakeSizeStore) RecordSegmentSize(_, filename, _ string, index int, size int64) {
	s.recorded[segmentRef{filename: filename, index: index}] = size
}

func (s *fakeSizeStore) RecordSegmentRead(_, filename string, index int) {
	s.reads[segmentRef{filename: filename, index: index}]++
}

func (s *fakeSizeStore) RecordProbes(_ string, probes []nzbstore.ProbeResult) error {
	for _, probe := range probes {
		select {
		case s.probes <- probe:
		default:
			s.probes <- probe // the buffer is sized for the tests that use it
		}
	}
	return s.probeErr
}

func (s *fakeSizeStore) EnsureSourceFiles(nzbName string, files []nzbstore.SourceFile) error {
	for _, file := range files {
		s.ensured[nzbName] = append(s.ensured[nzbName], file.Filename)
	}
	return nil
}

func waitProbe(t *testing.T, store *fakeSizeStore) nzbstore.ProbeResult {
	t.Helper()

	// The report is asynchronous, so it is waited for rather than raced on
	select {
	case probe := <-store.probes:
		return probe
	case <-time.After(5 * time.Second):
		t.Fatal("no probe was reported")
		return nzbstore.ProbeResult{}
	}
}

func TestAKnownSizeIsExactWithoutFetching(t *testing.T) {
	store := newFakeSizeStore()
	store.known["a@example.com"] = 700000
	getSegment := func(string, string) ([]byte, error) {
		t.Fatal("a known size must not cost a fetch")
		return nil, nil
	}

	factory := NewNzbFileFactory(nil, getSegment, store, 3, 2)
	nzbData := &nzbparser.NzbData{Files: []nzbparser.File{{
		Filename: "file.rar",
		Groups:   []string{"alt.binaries.test"},
		Segments: []nzbparser.Segment{{ID: "a@example.com", BytesHint: 999999}},
	}}}

	segment := factory.BuildResourceFromNzbSegment(
		nzbData.MetaName, &nzbData.Files[0], &nzbData.Files[0].Segments[0], 0,
		nzbfileanalyzer.NewSegmentSizer(nzbData).FileSizes(&nzbData.Files[0])[0], factory.knownSizes(nzbData),
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
	store := newFakeSizeStore()
	getSegment := func(string, string) ([]byte, error) {
		return make([]byte, 4242), nil
	}

	factory := NewNzbFileFactory(nil, getSegment, store, 3, 2)
	nzbData := &nzbparser.NzbData{Files: []nzbparser.File{{
		Filename: "file.rar",
		Groups:   []string{"alt.binaries.test"},
		Segments: []nzbparser.Segment{{Index: 1, ID: "a@example.com", BytesHint: 999999}},
	}}}

	segment := factory.BuildResourceFromNzbSegment(
		nzbData.MetaName, &nzbData.Files[0], &nzbData.Files[0].Segments[0], 0,
		nzbfileanalyzer.NewSegmentSizer(nzbData).FileSizes(&nzbData.Files[0])[0], nil,
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

	if store.recorded[segmentRef{filename: "file.rar", index: 0}] != 4242 {
		t.Errorf("recorded: got %v, want the decoded 4242 at the segment's position", store.recorded)
	}
}

// A fetch that delivered bytes is a present probe as well as a size: the scan
// must not re-ask about a segment a reader just pulled.
func TestFetchingCountsAsAPresentProbe(t *testing.T) {
	store := newFakeSizeStore()
	getSegment := func(string, string) ([]byte, error) {
		return make([]byte, 4242), nil
	}

	factory := NewNzbFileFactory(nil, getSegment, store, 0, 2)
	nzbData := &nzbparser.NzbData{Files: []nzbparser.File{{
		Filename: "file.rar",
		Groups:   []string{"alt.binaries.test"},
		Segments: []nzbparser.Segment{{Index: 1, ID: "a@example.com", BytesHint: 999999}},
	}}}

	segment := factory.BuildResourceFromNzbSegment(
		nzbData.MetaName, &nzbData.Files[0], &nzbData.Files[0].Segments[0], 0,
		nzbfileanalyzer.NewSegmentSizer(nzbData).FileSizes(&nzbData.Files[0])[0], nil,
	)
	reader, err := segment.Open()
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer reader.Close()
	if _, err := io.ReadAll(reader); err != nil {
		t.Fatalf("ReadAll: %v", err)
	}

	probe := waitProbe(t, store)
	if !probe.Present || probe.Filename != "file.rar" || probe.Index != 0 ||
		probe.MessageID != "a@example.com" || probe.Server != "" {
		t.Errorf("probe: got %+v, want a present probe of file.rar at the segment's position", probe)
	}
}
