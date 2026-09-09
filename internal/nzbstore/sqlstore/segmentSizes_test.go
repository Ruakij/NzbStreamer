package sqlstore

import (
	"fmt"
	"testing"
	"time"
)

func TestSegmentSizesSurviveReopening(t *testing.T) {
	dir := t.TempDir()

	store := storeAt(t, dir)
	store.RecordSegmentSize("a@example.com", 716800)
	store.RecordSegmentSize("b@example.com", 12345)
	// The later value wins, so a re-measurement is not a conflict
	store.RecordSegmentSize("b@example.com", 54321)
	store.Close()

	sizes, err := storeAt(t, dir).SegmentSizes([]string{"a@example.com", "b@example.com", "missing@example.com"})
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

func TestForgetSegments(t *testing.T) {
	store := storeAt(t, t.TempDir())

	store.RecordSegmentSize("a@example.com", 716800)
	store.flushSegmentSizes()
	// Still buffered, so forgetting has to reach the buffer as well as the table
	store.RecordSegmentSize("b@example.com", 12345)

	if err := store.ForgetSegments([]string{"a@example.com", "b@example.com"}); err != nil {
		t.Fatalf("ForgetSegments: %v", err)
	}
	store.flushSegmentSizes()

	sizes, err := store.SegmentSizes([]string{"a@example.com", "b@example.com"})
	if err != nil {
		t.Fatalf("SegmentSizes: %v", err)
	}
	if len(sizes) != 0 {
		t.Errorf("forgotten sizes came back: %v", sizes)
	}
}

func TestSegmentActivityCountsWhatWasReadAndWhatWasFetchedTwice(t *testing.T) {
	store := storeAt(t, t.TempDir())

	store.RecordSegmentSize("a@example.com", 700)
	store.RecordSegmentRead("a@example.com")
	// Read from the cache, so it belongs to the working set without a fetch
	store.RecordSegmentRead("b@example.com")
	store.RecordSegmentSize("b@example.com", 300)
	store.flushSegmentSizes()

	// Evicted and read again, which is what the working set outgrowing the cache
	// looks like
	store.RecordSegmentSize("a@example.com", 700)
	store.RecordSegmentRead("a@example.com")
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

// More ids than fit in one statement, which is what the chunking is for
func TestSegmentSizesBeyondOneStatement(t *testing.T) {
	store := storeAt(t, t.TempDir())

	ids := make([]string, 2000)
	for i := range ids {
		ids[i] = fmt.Sprintf("%d@example.com", i)
		store.RecordSegmentSize(ids[i], int64(i))
	}
	store.flushSegmentSizes()

	sizes, err := store.SegmentSizes(ids)
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
