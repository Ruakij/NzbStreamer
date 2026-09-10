package nzbrecordfactory

import (
	"io"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"git.ruekov.eu/ruakij/nzbStreamer/internal/nzbstore"
	"git.ruekov.eu/ruakij/nzbStreamer/internal/nzbstore/sqlstore"
	"git.ruekov.eu/ruakij/nzbStreamer/pkg/diskcache"
	"git.ruekov.eu/ruakij/nzbStreamer/pkg/nzbparser"
)

// mergeNzbXML is one content file of three segments, whose number attributes
// count from one as an nzb names them. What the store keys its segment rows by
// is not that number but the segment's position in the file, counted from zero,
// which is the space the scan writes in.
const mergeNzbXML = `<?xml version="1.0" encoding="utf-8" ?>
<nzb>
	<file poster="p@example.com" date="1700000000" subject="Release &#34;file.bin&#34; yEnc (3/3)">
		<groups><group>alt.binaries.test</group></groups>
		<segments>
			<segment bytes="500000" number="1">a@example.com</segment>
			<segment bytes="500000" number="2">b@example.com</segment>
			<segment bytes="500000" number="3">c@example.com</segment>
		</segments>
	</file>
</nzb>`

// What the read path reports and what the scan writes have to land on the same
// rows: the store is one key space, and a report keyed by the number attribute
// would sit beside the scan's rows, rewrite their message-ids where they
// overlap, and leave the last segment without the row its verdict hangs off.
func TestReadReportsLandOnThePositionsTheScanWrites(t *testing.T) {
	nzbData, err := nzbparser.ParseNzb(strings.NewReader(mergeNzbXML), "Some.Release.nzb")
	if err != nil {
		t.Fatalf("ParseNzb: %v", err)
	}

	store, err := sqlstore.New(filepath.Join(t.TempDir(), "metadata.db"))
	if err != nil {
		t.Fatalf("sqlstore.New: %v", err)
	}
	defer store.Close()
	if err := store.Add(nzbData, "completed", ""); err != nil {
		t.Fatalf("Add: %v", err)
	}

	cache, err := diskcache.NewCache(&diskcache.CacheOptions{
		CacheDir: filepath.Join(t.TempDir(), "cache"),
		MaxSize:  10 << 20,
	})
	if err != nil {
		t.Fatalf("NewCache: %v", err)
	}

	// Every fetch answers with bytes, so every read-path report is a present
	getSegment := func(_, _ string) ([]byte, error) {
		return make([]byte, 4242), nil
	}
	factory := NewNzbFileFactory(cache, getSegment, store, 0, 2)
	result, err := factory.BuildSegmentStackFromNzbData(nzbData, nil)
	if err != nil {
		t.Fatalf("BuildSegmentStackFromNzbData: %v", err)
	}

	// The scan side, which defines the space: 0-based positions over the
	// file's segments, every one but the last answered, as the periodic pass
	// writes them
	file := &nzbData.Files[0]
	last := len(file.Segments) - 1
	probes := make([]nzbstore.ProbeResult, 0, last)
	for i := range file.Segments[:last] {
		probes = append(probes, nzbstore.ProbeResult{
			Filename: file.Filename, Index: i, MessageID: file.Segments[i].ID, Present: true,
		})
	}
	if err := store.RecordProbes(nzbData.MetaName, probes); err != nil {
		t.Fatalf("RecordProbes: %v", err)
	}

	// A client reads the presented file end to end, which fetches and reports
	// every segment, the last one included
	reader, err := result.Presented[file.Filename].Open()
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if _, err := io.ReadAll(reader); err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if err := reader.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	verdicts := waitSegments(t, store, nzbData.MetaName, file.Filename, last+1)

	want := map[int]string{0: "a@example.com", 1: "b@example.com", 2: "c@example.com"}
	if len(verdicts) != len(want) {
		t.Fatalf("segment rows: got %v, want one per position %v", verdicts, want)
	}
	for index, id := range want {
		if verdict := verdicts[index]; verdict.MessageID != id || !verdict.Present {
			t.Errorf("position %d: got message %q present %v, want %q present",
				index, verdict.MessageID, verdict.Present, id)
		}
	}
}

// waitSegments waits for the segment rows of one source file to be complete,
// and lets the asynchronous reports settle before the keyspace is judged: a
// report that would land beside the rows instead of on them does so late.
func waitSegments(t *testing.T, store *sqlstore.Store, nzbName, filename string, count int) map[int]nzbstore.SegmentVerdict {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)
	for {
		perFile, err := store.SegmentVerdicts(nzbName, []string{filename})
		if err != nil {
			t.Fatalf("SegmentVerdicts: %v", err)
		}
		if len(perFile[filename]) >= count {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("segment rows of %s: got %v, want %d", filename, perFile[filename], count)
		}
		time.Sleep(10 * time.Millisecond)
	}

	time.Sleep(100 * time.Millisecond)
	perFile, err := store.SegmentVerdicts(nzbName, []string{filename})
	if err != nil {
		t.Fatalf("SegmentVerdicts: %v", err)
	}
	return perFile[filename]
}
