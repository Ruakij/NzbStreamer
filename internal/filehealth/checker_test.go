package filehealth_test

import (
	"errors"
	"slices"
	"sync"
	"testing"

	"git.ruekov.eu/ruakij/nzbStreamer/internal/filehealth"
	"git.ruekov.eu/ruakij/nzbStreamer/pkg/nzbparser"
)

func nzbWith(files ...nzbparser.File) *nzbparser.NzbData {
	return &nzbparser.NzbData{Files: files}
}

func fileWith(name string, segmentIDs ...string) nzbparser.File {
	segments := make([]nzbparser.Segment, len(segmentIDs))
	for i, id := range segmentIDs {
		segments[i] = nzbparser.Segment{ID: id, Index: i + 1}
	}
	return nzbparser.File{Filename: name, Segments: segments}
}

// recorder tracks which segment-ids were checked, and reports the given ones missing
func recorder(missing ...string) (filehealth.SegmentExistsFunc, *[]string) {
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

func TestChecksOnlyFirstAndLastSegment(t *testing.T) {
	exists, checked := recorder()
	checker := filehealth.NewDefaultChecker(filehealth.CheckerConfig{SegmentsPerFile: 2, MaxParallel: 4}, exists)

	errs := checker.CheckFiles(nzbWith(fileWith("a.rar", "s1", "s2", "s3", "s4")))
	if len(errs) != 0 {
		t.Fatalf("got errors %v, want none", errs)
	}

	slices.Sort(*checked)
	if want := []string{"s1", "s4"}; !slices.Equal(*checked, want) {
		t.Errorf("checked %v, want %v", *checked, want)
	}
}

func TestMissingSegmentReportsFile(t *testing.T) {
	exists, _ := recorder("b1")
	checker := filehealth.NewDefaultChecker(filehealth.CheckerConfig{SegmentsPerFile: -1, MaxParallel: 4}, exists)

	errs := checker.CheckFiles(nzbWith(
		fileWith("a.rar", "a1", "a2"),
		fileWith("b.rar", "b1", "b2"),
	))
	if len(errs) != 1 {
		t.Fatalf("got %d errors, want 1: %v", len(errs), errs)
	}
	if !errors.Is(errs[0], filehealth.ErrSegmentsMissing) {
		t.Errorf("got %v, want ErrSegmentsMissing", errs[0])
	}

	var healthErr *filehealth.FileHealthError
	if !errors.As(errs[0], &healthErr) || healthErr.Path != "b.rar" {
		t.Errorf("got error for %v, want b.rar", errs[0])
	}
}

func TestDisabledCheckDoesNothing(t *testing.T) {
	exists, checked := recorder("s1")
	checker := filehealth.NewDefaultChecker(filehealth.CheckerConfig{SegmentsPerFile: 0}, exists)

	if errs := checker.CheckFiles(nzbWith(fileWith("a.rar", "s1"))); errs != nil {
		t.Fatalf("got errors %v, want none", errs)
	}
	if len(*checked) != 0 {
		t.Errorf("checked %v, want nothing", *checked)
	}
}
