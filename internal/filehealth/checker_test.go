package filehealth_test

import (
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"

	"git.ruekov.eu/ruakij/nzbStreamer/internal/filehealth"
	"git.ruekov.eu/ruakij/nzbStreamer/pkg/nzbparser"
)

// config is the default probe setup, minus the percentages a test cares about
func config(initialPercent, extensivePercent float64) filehealth.CheckerConfig {
	return filehealth.CheckerConfig{
		InitialFilePercent:       initialPercent,
		InitialFileMinSegments:   2,
		InitialFileMaxSegments:   8,
		ExtensiveFilePercent:     extensivePercent,
		ExtensiveFileMaxSegments: 512,
		MaxMissingPercent:        100,
		Par2Safety:               0.9,
		UndecidedAccept:          true,
		Confidence:               0.95,
		MaxParallel:              4,
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

func TestChecksOnlyFirstAndLastSegmentOfContent(t *testing.T) {
	exists, checked := recorder()
	checker := filehealth.NewDefaultChecker(config(0.5, 1), exists)

	errs := checker.CheckFiles(nzbWith(
		fileWith("a.rar", "s1", "s2", "s3", "s4"),
		fileWith("a.vol00+01.par2", "p1", "p2"),
		fileWith("a.nfo", "n1"),
	))
	if len(errs) != 0 {
		t.Fatalf("got errors %v, want none", errs)
	}

	slices.Sort(*checked)
	if want := []string{"s1", "s4"}; !slices.Equal(*checked, want) {
		t.Errorf("checked %v, want %v", *checked, want)
	}
}

func TestMissingSegmentWithoutPar2ReportsFile(t *testing.T) {
	exists, _ := recorder("b1")
	checker := filehealth.NewDefaultChecker(config(100, 100), exists)

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

func TestDamageWithinPar2CapacityIsAccepted(t *testing.T) {
	exists, _ := recorder("a7")
	checker := filehealth.NewDefaultChecker(filehealth.CheckerConfig{
		InitialFilePercent:     100,
		InitialFileMinSegments: 2,
		InitialFileMaxSegments: 40,
		MaxMissingPercent:      100,
		Par2Safety:             0.9,
		Confidence:             0.95,
		MaxParallel:            4,
	}, exists)

	// Recovery as large as the content, so the limit is 90% missing
	errs := checker.CheckFiles(nzbWith(
		fileOf("a.rar", "a", 40),
		fileOf("a.vol00+39.par2", "p", 40),
	))
	if len(errs) != 0 {
		t.Fatalf("got errors %v, want none", errs)
	}
}

// halfGone reports the ids of every second segment of a 100-segment file, so
// any evenly spread sample of it comes back about half missing
func halfGone() []string {
	var missing []string
	for i := 2; i <= 100; i += 2 {
		missing = append(missing, fmt.Sprintf("a%d", i))
	}
	return missing
}

// A sample of two, half of it missing, cannot tell 50% damage from 90% damage,
// and 90% is what the par2 here could repair
func TestUndecidedFileIsProbedAgain(t *testing.T) {
	exists, checked := recorder(halfGone()...)
	checker := filehealth.NewDefaultChecker(config(0.5, 100), exists)

	errs := checker.CheckFiles(nzbWith(
		fileOf("a.rar", "a", 100),
		fileOf("a.vol00+99.par2", "p", 100),
	))
	if len(errs) != 0 {
		t.Fatalf("got errors %v, want none", errs)
	}
	if len(*checked) <= 2 {
		t.Errorf("checked %d segments, want the initial sample plus a widened one", len(*checked))
	}
}

func TestUndecidedFileIsReportedWhenNotAccepted(t *testing.T) {
	exists, _ := recorder(halfGone()...)
	// A cap too small for the widened sample to settle it either
	config := config(0.5, 100)
	config.ExtensiveFileMaxSegments = 3
	config.UndecidedAccept = false
	checker := filehealth.NewDefaultChecker(config, exists)

	errs := checker.CheckFiles(nzbWith(
		fileOf("a.rar", "a", 100),
		fileOf("a.vol00+99.par2", "p", 100),
	))
	if len(errs) != 1 {
		t.Fatalf("got %d errors, want 1: %v", len(errs), errs)
	}
}

func TestDisabledCheckDoesNothing(t *testing.T) {
	exists, checked := recorder("s1")
	checker := filehealth.NewDefaultChecker(config(0, 0), exists)

	if errs := checker.CheckFiles(nzbWith(fileWith("a.rar", "s1"))); errs != nil {
		t.Fatalf("got errors %v, want none", errs)
	}
	if len(*checked) != 0 {
		t.Errorf("checked %v, want nothing", *checked)
	}
}
