package sqlstore

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"git.ruekov.eu/ruakij/nzbStreamer/internal/nzbstore"
	"git.ruekov.eu/ruakij/nzbStreamer/pkg/nzbparser"
)

var posted = time.Unix(1700000000, 0)

// The nzb is named after the file it was read from, with the extension
// resolved away, so the record key and the parsed name are the same constant.
const (
	nzbFilename = "Some.Release.nzb"
	nzbName     = "Some.Release"
)

// withSourceFiles adds the nzb record the source files hang off, and records
// the named files under it. A source file row references the nzb row, so
// activity and verdicts have nothing to attach to until both exist.
func withSourceFiles(t *testing.T, dir string, filenames ...string) *Store {
	t.Helper()

	store := storeAt(t, dir)
	data, err := nzbparser.ParseNzb(strings.NewReader(nzbXML), nzbFilename)
	if err != nil {
		t.Fatalf("ParseNzb: %v", err)
	}
	if data.MetaName != nzbName {
		t.Fatalf("parsed nzb is named %q, want %q", data.MetaName, nzbName)
	}
	if err := store.Add(data, "completed", ""); err != nil {
		t.Fatalf("Add: %v", err)
	}

	files := make([]nzbstore.SourceFile, len(filenames))
	for i, filename := range filenames {
		files[i] = nzbstore.SourceFile{Filename: filename, PostedAt: posted}
	}
	if err := store.EnsureSourceFiles(nzbName, files); err != nil {
		t.Fatalf("EnsureSourceFiles: %v", err)
	}

	return store
}

func TestSegmentSizesSurviveReopening(t *testing.T) {
	dir := t.TempDir()

	store := withSourceFiles(t, dir, "file.rar")
	store.RecordSegmentSize(nzbName, "file.rar", "a@example.com", 1, 716800)
	store.RecordSegmentSize(nzbName, "file.rar", "b@example.com", 2, 12345)
	// The later value wins, so a re-measurement is not a conflict
	store.RecordSegmentSize(nzbName, "file.rar", "b@example.com", 2, 54321)
	store.Close()

	sizes, err := storeAt(t, dir).SegmentSizes(nzbName,
		[]string{"a@example.com", "b@example.com", "missing@example.com"})
	if err != nil {
		t.Fatalf("SegmentSizes: %v", err)
	}

	if sizes["a@example.com"] != 716800 || sizes["b@example.com"] != 54321 {
		t.Errorf("sizes: got %v", sizes)
	}
	if _, ok := sizes["missing@example.com"]; ok {
		t.Errorf("an unknown segment came back with a size: %v", sizes)
	}
}

// Sizes are learned per nzb now, so a message-id another nzb also names is not
// answered out of that one's rows.
func TestSegmentSizesOfOneNzbDoNotLeakIntoAnother(t *testing.T) {
	store := withSourceFiles(t, t.TempDir(), "other.rar")
	data, err := nzbparser.ParseNzb(strings.NewReader(nzbXML), "Other.Release.nzb")
	if err != nil {
		t.Fatalf("ParseNzb: %v", err)
	}
	if err := store.Add(data, "completed", ""); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := store.EnsureSourceFiles(data.MetaName, []nzbstore.SourceFile{
		{Filename: "other.rar", PostedAt: posted},
	}); err != nil {
		t.Fatalf("EnsureSourceFiles: %v", err)
	}

	store.RecordSegmentSize(data.MetaName, "other.rar", "a@example.com", 1, 716800)
	store.flushSegmentSizes()

	sizes, err := store.SegmentSizes(nzbName, []string{"a@example.com"})
	if err != nil {
		t.Fatalf("SegmentSizes: %v", err)
	}
	if len(sizes) != 0 {
		t.Errorf("another nzb's size leaked: %v", sizes)
	}
}

func TestSegmentActivityCountsWhatWasReadAndWhatWasFetchedTwice(t *testing.T) {
	store := withSourceFiles(t, t.TempDir(), "file.rar")

	store.RecordSegmentSize(nzbName, "file.rar", "a@example.com", 1, 700)
	store.RecordSegmentRead(nzbName, "file.rar", 1)
	// Read from the cache, so it belongs to the working set without a fetch
	store.RecordSegmentRead(nzbName, "file.rar", 2)
	store.RecordSegmentSize(nzbName, "file.rar", "b@example.com", 2, 300)
	store.flushSegmentSizes()

	// Evicted and read again, which is what the working set outgrowing the cache
	// looks like
	store.RecordSegmentSize(nzbName, "file.rar", "a@example.com", 1, 700)
	store.RecordSegmentRead(nzbName, "file.rar", 1)
	store.flushSegmentSizes()

	// Every window is answered off one scan, and one whose cutoff is in the
	// future has nothing read within it
	activity, err := store.SegmentActivitySince([]time.Time{
		time.Now().Add(-time.Hour), time.Now().Add(time.Hour),
	})
	if err != nil {
		t.Fatalf("SegmentActivitySince: %v", err)
	}
	if activity[0] != (SegmentActivity{WorkingSetBytes: 1000, WorkingSetSegments: 2}) {
		t.Errorf("activity: got %+v, want a working set of 1000 bytes over 2 segments", activity[0])
	}
	if activity[1] != (SegmentActivity{}) {
		t.Errorf("activity outside the window: got %+v", activity[1])
	}
	if bytes, segments := store.Refetches(); bytes != 700 || segments != 1 {
		t.Errorf("refetches: got %d bytes over %d segments, want 700 over 1", bytes, segments)
	}
}

// The same post can sit under two files of one nzb; it is one segment of the
// working set and its bytes count once.
func TestSegmentActivityCountsAPostOnceWhateverTheFilesSay(t *testing.T) {
	store := withSourceFiles(t, t.TempDir(), "a.rar", "b.rar")

	store.RecordSegmentSize(nzbName, "a.rar", "same@example.com", 1, 700)
	store.RecordSegmentRead(nzbName, "a.rar", 1)
	// Learned separately, and at a different length, by the file that names the
	// same post again
	store.RecordSegmentSize(nzbName, "b.rar", "same@example.com", 1, 300)
	store.RecordSegmentRead(nzbName, "b.rar", 1)
	store.flushSegmentSizes()

	activity, err := store.SegmentActivitySince([]time.Time{time.Now().Add(-time.Hour)})
	if err != nil {
		t.Fatalf("SegmentActivitySince: %v", err)
	}
	if activity[0] != (SegmentActivity{WorkingSetBytes: 700, WorkingSetSegments: 1}) {
		t.Errorf("activity: got %+v, want one segment of 700 bytes", activity[0])
	}
}

// More segments than fit in one statement, which is what the chunking is for
func TestSegmentSizesBeyondOneStatement(t *testing.T) {
	store := withSourceFiles(t, t.TempDir(), "file.rar")

	ids := make([]string, 2000)
	for i := range ids {
		ids[i] = fmt.Sprintf("%d@example.com", i)
		store.RecordSegmentSize(nzbName, "file.rar", ids[i], i, int64(i))
	}
	store.flushSegmentSizes()

	sizes, err := store.SegmentSizes(nzbName, ids)
	if err != nil {
		t.Fatalf("SegmentSizes: %v", err)
	}
	if len(sizes) != len(ids) {
		t.Fatalf("expected %d sizes, got %d", len(ids), len(sizes))
	}
	if sizes[ids[1999]] != 1999 {
		t.Errorf("last size: got %d", sizes[ids[1999]])
	}
}

// Activity whose source file has gone - an nzb deleted while its reads were
// still buffered - is dropped at flush rather than written against nothing.
func TestSegmentActivityForAForgottenFileIsDropped(t *testing.T) {
	store := withSourceFiles(t, t.TempDir(), "file.rar")
	store.RecordSegmentRead(nzbName, "gone.rar", 1)

	if err := store.Delete(nzbName); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	store.flushSegmentSizes()

	activity, err := store.SegmentActivitySince([]time.Time{time.Now().Add(-time.Hour)})
	if err != nil {
		t.Fatalf("SegmentActivitySince: %v", err)
	}
	if activity[0] != (SegmentActivity{}) {
		t.Errorf("activity of a deleted nzb survived: %+v", activity[0])
	}
}
