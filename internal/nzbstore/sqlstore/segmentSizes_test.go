package sqlstore

import (
	"fmt"
	"testing"
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
