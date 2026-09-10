package filehealth

import (
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"

	"git.ruekov.eu/ruakij/nzbStreamer/pkg/nzbparser"
)

// config is the default probe setup, plus the confidence a test cares about
func config(confidence float64) CheckerConfig {
	return CheckerConfig{
		AddConfidence:     confidence,
		MaxMissingPercent: 100,
		Par2Safety:        0.9,
		UndecidedAccept:   true,
		MaxParallel:       4,
	}
}

func nzbWith(files ...nzbparser.File) *nzbparser.NzbData {
	return &nzbparser.NzbData{Files: files}
}

func fileWith(name string, segmentIDs ...string) nzbparser.File {
	segments := make([]nzbparser.Segment, len(segmentIDs))
	for i, id := range segmentIDs {
		segments[i] = nzbparser.Segment{ID: id, Index: i + 1, BytesHint: 1}
	}
	return nzbparser.File{Filename: name, Segments: segments}
}

// fileOf builds a file of count segments named prefix1..prefixN
func fileOf(name, prefix string, count int) nzbparser.File {
	ids := make([]string, count)
	for i := range ids {
		ids[i] = fmt.Sprintf("%s%d", prefix, i+1)
	}
	return fileWith(name, ids...)
}

// recorder tracks which segment-ids were checked, and reports the given ones missing
func recorder(missing ...string) (SegmentExistsFunc, *[]string) {
	var (
		mu      sync.Mutex
		checked []string
	)
	return func(id string) (bool, error) {
		mu.Lock()
		checked = append(checked, id)
		mu.Unlock()
		return !slices.Contains(missing, id), nil
	}, &checked
}

func failedNames(groups []FailedGroup) [][]string {
	names := make([][]string, len(groups))
	for i, g := range groups {
		names[i] = g.Files
	}
	return names
}

func TestGroupedVolumeSetIsProbedAsOneUnit(t *testing.T) {
	exists, _ := recorder("s8") // a segment of a.r00 is lost
	checker := NewDefaultChecker(config(1), exists)

	failed := checker.CheckFiles(t.Context(), nzbWith(
		fileWith("a.rar", "s1", "s2", "s3", "s4"),
		fileWith("a.r00", "s5", "s6", "s7", "s8"),
		fileWith("a.r01", "s9", "s10", "s11", "s12"),
		fileWith("b.mkv", "m1", "m2", "m3", "m4"),
	), nil)

	if len(failed) != 1 {
		t.Fatalf("got %d failed groups, want 1: %v", len(failed), failedNames(failed))
	}
	if failed[0].Name != "a.rar" {
		t.Errorf("failed group name = %q, want a.rar", failed[0].Name)
	}
	if want := []string{"a.rar", "a.r00", "a.r01"}; !slices.Equal(failed[0].Files, want) {
		t.Errorf("failed group files = %v, want %v", failed[0].Files, want)
	}
}

func TestMissingSegmentFailsTheGroupAndStopsEarly(t *testing.T) {
	// probeOrder(8) = [0,4,2,6,1,5,3,7]; position 1 is segment index 4 (s5)
	exists, checked := recorder("s5")
	checker := NewDefaultChecker(config(1), exists)

	checker.config.MaxParallel = 1
	failed := checker.CheckFiles(t.Context(), nzbWith(
		fileWith("a.rar", "s1", "s2", "s3", "s4", "s5", "s6", "s7", "s8"),
	), nil)

	if len(failed) != 1 {
		t.Fatalf("got %d failed groups, want 1", len(failed))
	}
	// One present probe, then the miss ends the scan; nothing else is asked.
	if want := []string{"s1", "s5"}; !slices.Equal(*checked, want) {
		t.Errorf("checked %v, want %v (stopped at the miss)", *checked, want)
	}
}

func TestCleanGroupIsProbedToTheConfidence(t *testing.T) {
	exists, checked := recorder()
	checker := NewDefaultChecker(config(0.5), exists)

	failed := checker.CheckFiles(t.Context(), nzbWith(
		fileWith("a.rar", "s1", "s2", "s3", "s4", "s5", "s6", "s7", "s8"),
	), nil)

	if len(failed) != 0 {
		t.Fatalf("got failed groups %v, want none", failedNames(failed))
	}
	// 0.5 * 8 = 4 distinct probes, none repeated.
	if want := 4; len(*checked) != want {
		t.Errorf("probed %d segments, want %d", len(*checked), want)
	}
}

func TestDisabledCheckDoesNothing(t *testing.T) {
	exists, checked := recorder("s1")
	checker := NewDefaultChecker(config(0), exists)

	if failed := checker.CheckFiles(t.Context(), nzbWith(fileWith("a.rar", "s1")), nil); len(failed) != 0 {
		t.Fatalf("got failed groups %v, want none", failedNames(failed))
	}
	if len(*checked) != 0 {
		t.Errorf("checked %v, want nothing", *checked)
	}
}

func TestFailedGroupCarriesItsFiles(t *testing.T) {
	// The whole a.r01 volume is lost; every member filename must be reported.
	exists, _ := recorder("s9", "s10", "s11", "s12")
	checker := NewDefaultChecker(config(0.25), exists)

	failed := checker.CheckFiles(t.Context(), nzbWith(
		fileWith("a.rar", "s1", "s2", "s3", "s4"),
		fileWith("a.r00", "s5", "s6", "s7", "s8"),
		fileWith("a.r01", "s9", "s10", "s11", "s12"),
	), nil)

	if len(failed) != 1 {
		t.Fatalf("got %d failed groups, want 1: %v", len(failed), failedNames(failed))
	}
	if want := []string{"a.rar", "a.r00", "a.r01"}; !slices.Equal(failed[0].Files, want) {
		t.Errorf("failed group files = %v, want %v", failed[0].Files, want)
	}
}

func TestProbeOrderIsABitReversalPermutation(t *testing.T) {
	order := probeOrder(16)
	if len(order) != 16 {
		t.Fatalf("probeOrder(16) has %d entries, want 16", len(order))
	}
	seen := make([]bool, 16)
	for _, v := range order {
		if v < 0 || v >= 16 || seen[v] {
			t.Fatalf("probeOrder(16) is not a permutation of 0..15: %v", order)
		}
		seen[v] = true
	}
	if want := []int{0, 8, 4, 12}; !slices.Equal(order[:4], want) {
		t.Errorf("first 4 = %v, want %v", order[:4], want)
	}
	if want := []int{0, 8, 4, 12, 2, 10, 6, 14}; !slices.Equal(order[:8], want) {
		t.Errorf("first 8 = %v, want %v", order[:8], want)
	}
}

func TestScanCountsKnownPositionsWithoutProbing(t *testing.T) {
	// probeOrder(8) = [0,4,2,6,1,5,3,7]; position 0 is already known.
	exists, checked := recorder()
	checker := NewDefaultChecker(config(1), exists)

	file := fileWith("a.rar", "s1", "s2", "s3", "s4", "s5", "s6", "s7", "s8")
	g := group{Name: "a.rar", Files: []*nzbparser.File{&file}, Segments: 8}

	covered, err := checker.scan(t.Context(), g, []int{0}, 4, nil)
	if err != nil {
		t.Fatalf("scan error: %v", err)
	}
	// order[:4] = [0,4,2,6]; position 0 is skipped, so 3 are probed plus the
	// known one counts -> 4 covered.
	if covered != 4 {
		t.Errorf("covered = %d, want 4", covered)
	}
	// s1 (position 0) must not be asked of the exists func.
	if slices.Contains(*checked, "s1") {
		t.Errorf("known segment s1 was probed: %v", *checked)
	}
	if want := 3; len(*checked) != want {
		t.Errorf("probed %d segments, want %d", len(*checked), want)
	}
}

func TestProgressEndsAtEverythingItProbed(t *testing.T) {
	exists, checked := recorder()
	checker := NewDefaultChecker(config(0.5), exists)

	var (
		mu           sync.Mutex
		last         [2]int
		reports      int
		wentBackward bool
	)
	progress := func(done, total int) {
		mu.Lock()
		defer mu.Unlock()
		if done < last[0] || total < last[1] {
			wentBackward = true
		}
		last = [2]int{done, total}
		reports++
	}

	failed := checker.CheckFiles(t.Context(), nzbWith(
		fileOf("a.rar", "s", 8),
	), progress)
	if len(failed) != 0 {
		t.Fatalf("got failed groups %v, want none", failedNames(failed))
	}

	if wentBackward {
		t.Error("progress went backward")
	}
	if last[0] != last[1] || last[0] != len(*checked) {
		t.Errorf("ended at %d of %d, want %d of the same", last[0], last[1], len(*checked))
	}
	if reports <= 2 {
		t.Errorf("got %d reports, want the planning plus one per probe", reports)
	}
}

func TestUnhealthyGroup(t *testing.T) {
	// b.mkv lost a segment, a.rar is healthy: only b.mkv is a failed group.
	exists, _ := recorder("m1")
	checker := NewDefaultChecker(config(1), exists)

	failed := checker.CheckFiles(t.Context(), nzbWith(
		fileWith("a.rar", "s1", "s2", "s3", "s4"),
		fileWith("b.mkv", "m1", "m2", "m3", "m4"),
	), nil)

	if len(failed) != 1 {
		t.Fatalf("got %d failed groups, want 1: %v", len(failed), failedNames(failed))
	}
	if failed[0].Name != "b.mkv" || !slices.Equal(failed[0].Files, []string{"b.mkv"}) {
		t.Errorf("failed group = %+v, want b.mkv", failed[0])
	}
}

func TestScanStopsEarlyOnMissing(t *testing.T) {
	file := fileWith("a.rar", "s1", "s2", "s3", "s4", "s5", "s6", "s7", "s8")
	g := group{Name: "a.rar", Files: []*nzbparser.File{&file}, Segments: 8}

	exists, _ := recorder("s5")
	checker := NewDefaultChecker(config(1), exists)
	checker.config.MaxParallel = 1

	_, err := checker.scan(t.Context(), g, nil, 8, nil)
	if !errors.Is(err, ErrSegmentsMissing) {
		t.Errorf("scan err = %v, want ErrSegmentsMissing", err)
	}
}
