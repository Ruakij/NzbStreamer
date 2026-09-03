package nzbrecordfactory

import (
	"testing"

	"git.ruekov.eu/ruakij/nzbStreamer/internal/nzbfileanalyzer"
	"git.ruekov.eu/ruakij/nzbStreamer/pkg/nzbparser"
)

// unknownConventionNzb posts a segment size no producer table lists, so nothing
// in the nzb says whether the hints count wire or content bytes.
func unknownConventionNzb() *nzbparser.NzbData {
	return &nzbparser.NzbData{Files: []nzbparser.File{{
		Groups: []string{"alt.binaries.test"},
		Segments: []nzbparser.Segment{
			{Index: 1, ID: "a@example.com", BytesHint: 500000},
			{Index: 2, ID: "b@example.com", BytesHint: 500000},
			{Index: 3, ID: "c@example.com", BytesHint: 120000},
		},
	}}}
}

// One decoded segment settles the convention for the ones never fetched.
func TestADecodedSegmentSettlesTheRestOfTheNzb(t *testing.T) {
	nzbData := unknownConventionNzb()
	known := map[string]int64{"a@example.com": 480000}

	sizer := settleConvention(nzbData, nzbfileanalyzer.NewSegmentSizer(nzbData), known)
	if sizer.Convention() != nzbfileanalyzer.ConventionWire {
		t.Fatalf("convention = %v, want ConventionWire", sizer.Convention())
	}

	// b was never fetched, so only the settled convention can size it exactly
	size, exact := sizer.Size(500000)
	if size != 480000 || !exact {
		t.Errorf("Size(500000) = %d, %v; want 480000, true", size, exact)
	}
}

func TestNothingDecodedLeavesTheConventionUnknown(t *testing.T) {
	nzbData := unknownConventionNzb()

	sizer := settleConvention(nzbData, nzbfileanalyzer.NewSegmentSizer(nzbData), nil)
	if sizer.Convention() != nzbfileanalyzer.ConventionUnknown {
		t.Errorf("convention = %v, want ConventionUnknown", sizer.Convention())
	}
}

// Only a full segment can settle it, so the tail alone is not enough.
func TestOnlyATailDecodedLeavesTheConventionUnknown(t *testing.T) {
	nzbData := unknownConventionNzb()
	known := map[string]int64{"c@example.com": 115000}

	sizer := settleConvention(nzbData, nzbfileanalyzer.NewSegmentSizer(nzbData), known)
	if sizer.Convention() != nzbfileanalyzer.ConventionUnknown {
		t.Errorf("convention = %v, want ConventionUnknown", sizer.Convention())
	}
}

// With nothing decoded yet, one segment is fetched to settle the convention,
// and its length is kept so no later build repeats the probe.
func TestProbingSettlesTheConvention(t *testing.T) {
	store := &fakeSizeStore{known: map[string]int64{}, recorded: map[string]int64{}}

	var fetched []string
	getSegment := func(_, id string) ([]byte, error) {
		fetched = append(fetched, id)
		return make([]byte, 480000), nil
	}

	factory := NewNzbFileFactory(nil, getSegment, store, true)
	nzbData := unknownConventionNzb()

	sizer := factory.sizer(nzbData, nil)
	if sizer.Convention() != nzbfileanalyzer.ConventionWire {
		t.Fatalf("convention = %v, want ConventionWire", sizer.Convention())
	}

	// The tail carries a hint of its own, so probing it would settle nothing
	if len(fetched) != 1 || fetched[0] != "a@example.com" {
		t.Errorf("probe fetched %v, want one full segment", fetched)
	}
	if store.recorded["a@example.com"] != 480000 {
		t.Errorf("the probed length was not kept: %v", store.recorded)
	}
}

// A convention the hints already identify is worth no traffic at all.
func TestAKnownConventionIsNotProbed(t *testing.T) {
	getSegment := func(string, string) ([]byte, error) {
		t.Fatal("a known convention must not cost a fetch")
		return nil, nil
	}

	factory := NewNzbFileFactory(nil, getSegment, nil, true)
	nzbData := unknownConventionNzb()
	for i := range nzbData.Files[0].Segments[:2] {
		nzbData.Files[0].Segments[i].BytesHint = 768000
	}

	if got := factory.sizer(nzbData, nil).Convention(); got != nzbfileanalyzer.ConventionContent {
		t.Errorf("convention = %v, want ConventionContent", got)
	}
}

// A store that already holds a decoded length settles it without traffic.
func TestAStoredLengthIsNotProbed(t *testing.T) {
	getSegment := func(string, string) ([]byte, error) {
		t.Fatal("a stored length must not cost a fetch")
		return nil, nil
	}

	factory := NewNzbFileFactory(nil, getSegment, nil, true)
	nzbData := unknownConventionNzb()

	sizer := factory.sizer(nzbData, map[string]int64{"a@example.com": 480000})
	if sizer.Convention() != nzbfileanalyzer.ConventionWire {
		t.Errorf("convention = %v, want ConventionWire", sizer.Convention())
	}
}

// With probing off the nzb costs no traffic and keeps its estimates.
func TestProbingCanBeDisabled(t *testing.T) {
	getSegment := func(string, string) ([]byte, error) {
		t.Fatal("probing is off, so nothing may be fetched")
		return nil, nil
	}

	factory := NewNzbFileFactory(nil, getSegment, nil, false)
	nzbData := unknownConventionNzb()

	if got := factory.sizer(nzbData, nil).Convention(); got != nzbfileanalyzer.ConventionUnknown {
		t.Errorf("convention = %v, want ConventionUnknown", got)
	}
}

// A length a read has already measured settles it even with probing off, which
// is what makes the nzb exact from the next build on.
func TestAStoredLengthSettlesItWithProbingOff(t *testing.T) {
	factory := NewNzbFileFactory(nil, nil, nil, false)
	nzbData := unknownConventionNzb()

	sizer := factory.sizer(nzbData, map[string]int64{"a@example.com": 480000})
	if sizer.Convention() != nzbfileanalyzer.ConventionWire {
		t.Errorf("convention = %v, want ConventionWire", sizer.Convention())
	}
}
