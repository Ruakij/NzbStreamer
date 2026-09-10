package nzbrecordfactory

import (
	"errors"
	"fmt"
	"io"
	"testing"

	"git.ruekov.eu/ruakij/nzbStreamer/internal/nntpclient"
	"git.ruekov.eu/ruakij/nzbStreamer/internal/nzbfileanalyzer"
	"git.ruekov.eu/ruakij/nzbStreamer/pkg/nzbparser"
	"git.ruekov.eu/ruakij/nzbStreamer/pkg/resource/nzbpostresource"
)

// observedNzb is a content file of two segments, one of which the tests make
// the fetch fail for. The number attributes are 1-based, as an nzb names them;
// what the reports carry is the slice position, which is 0-based.
func observedNzb() *nzbparser.NzbData {
	return &nzbparser.NzbData{
		MetaName: "Some.Release",
		Files: []nzbparser.File{{
			Filename: "file.bin",
			Groups:   []string{"alt.binaries.test"},
			Segments: []nzbparser.Segment{
				{Index: 1, ID: "a@example.com", BytesHint: 500000},
				{Index: 2, ID: "b@example.com", BytesHint: 500000},
			},
		}},
	}
}

func observedResource(t *testing.T, factory *NzbFileFactory, nzbData *nzbparser.NzbData, index int) *nzbpostresource.NzbPostResource {
	t.Helper()
	segment := &nzbData.Files[0].Segments[index]
	return factory.BuildResourceFromNzbSegment(nzbData.MetaName, &nzbData.Files[0], segment, index,
		nzbfileanalyzer.NewSegmentSizer(nzbData).FileSizes(&nzbData.Files[0])[index], nil)
}

// A fetch the server answers with not-found is a verdict, not a log line: the
// read still reports the failure it hit, and the miss reaches the store.
func TestANotFoundFetchIsRecordedAsAMiss(t *testing.T) {
	store := newFakeSizeStore()
	getSegment := func(_, id string) ([]byte, error) {
		return nil, fmt.Errorf("%w: '%s'", nntpclient.ErrArticleNotFound, id)
	}
	factory := NewNzbFileFactory(nil, getSegment, store, 0, 2)
	nzbData := observedNzb()

	segment := observedResource(t, factory, nzbData, 0)
	reader, err := segment.Open()
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer reader.Close()

	if _, err := io.ReadAll(reader); err == nil {
		t.Fatal("a read of a missing segment succeeded")
	} else if !errors.Is(err, nntpclient.ErrArticleNotFound) {
		t.Fatalf("ReadAll: %v, want the not-found carried through", err)
	}

	probe := waitProbe(t, store)
	if probe.Present || probe.Filename != "file.bin" || probe.Index != 0 ||
		probe.MessageID != "a@example.com" || probe.Server != "" {
		t.Errorf("probe: got %+v, want a miss of file.bin at position 0 with no server named", probe)
	}
}

// A transport error has not answered the question, so it records nothing.
func TestATransportErrorRecordsNothing(t *testing.T) {
	store := newFakeSizeStore()
	getSegment := func(_, _ string) ([]byte, error) {
		return nil, errors.New("connection reset by peer")
	}
	factory := NewNzbFileFactory(nil, getSegment, store, 0, 2)
	nzbData := observedNzb()

	segment := observedResource(t, factory, nzbData, 0)
	reader, err := segment.Open()
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer reader.Close()

	if _, err := io.ReadAll(reader); err == nil {
		t.Fatal("a read of a failed fetch succeeded")
	}
	// Nothing was reported: the probe channel staying empty is the assertion,
	// since a report would have been sent by now
	select {
	case probe := <-store.probes:
		t.Errorf("a transport error was reported as a probe: %+v", probe)
	default:
	}
}

// A report that cannot be stored costs a log line, never the read.
func TestAFailedReportDoesNotFailTheRead(t *testing.T) {
	store := newFakeSizeStore()
	store.probeErr = errors.New("database is closed")
	getSegment := func(string, string) ([]byte, error) {
		return make([]byte, 4242), nil
	}

	factory := NewNzbFileFactory(nil, getSegment, store, 0, 2)
	nzbData := observedNzb()

	segment := observedResource(t, factory, nzbData, 1)
	reader, err := segment.Open()
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer reader.Close()

	body, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if len(body) != 4242 {
		t.Errorf("read %d bytes, want 4242", len(body))
	}
	waitProbe(t, store)
}

// The build records the nzb's own files, so the reports of what it built have
// rows to land in even when the caller has not ensured them yet.
func TestTheBuildEnsuresTheSourceFilesItReportsAgainst(t *testing.T) {
	store := newFakeSizeStore()
	factory := NewNzbFileFactory(nil, nil, store, 0, 2)
	nzbData := observedNzb()

	if _, err := factory.BuildSegmentStackFromNzbData(nzbData, nil); err != nil {
		t.Fatalf("BuildSegmentStackFromNzbData: %v", err)
	}
	if got := store.ensured["Some.Release"]; len(got) != 1 || got[0] != "file.bin" {
		t.Errorf("ensured: got %v, want [file.bin]", got)
	}
}

// Every presented path is attributed to the nzb's own file it was built from.
func TestTheBuildAttributesWhatItPresents(t *testing.T) {
	nzbData := &nzbparser.NzbData{
		MetaName: "Some.Release",
		Files: []nzbparser.File{
			{
				Filename: "Movie.mkv",
				Groups:   []string{"alt.binaries.test"},
				Segments: []nzbparser.Segment{{Index: 1, ID: "a@example.com", BytesHint: 100}},
			},
			{
				Filename: "Sample.mkv",
				Groups:   []string{"alt.binaries.test"},
				Segments: []nzbparser.Segment{{Index: 1, ID: "b@example.com", BytesHint: 100}},
			},
		},
	}
	getSegment := func(string, string) ([]byte, error) {
		t.Fatal("building must not fetch")
		return nil, nil
	}

	factory := NewNzbFileFactory(nil, getSegment, nil, 0, 2)
	result, err := factory.BuildSegmentStackFromNzbData(nzbData, nil)
	if err != nil {
		t.Fatalf("BuildSegmentStackFromNzbData: %v", err)
	}

	if len(result.Presented) != 2 {
		t.Fatalf("presented: got %v", result.Presented)
	}
	for _, filename := range []string{"Movie.mkv", "Sample.mkv"} {
		if _, ok := result.Presented[filename]; !ok {
			t.Errorf("%s is not presented", filename)
		}
		if result.SourceOf[filename] != filename {
			t.Errorf("%s is attributed to %q, want itself", filename, result.SourceOf[filename])
		}
	}
}
