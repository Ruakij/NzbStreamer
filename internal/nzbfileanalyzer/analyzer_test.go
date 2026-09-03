package nzbfileanalyzer

import (
	"errors"
	"testing"

	"git.ruekov.eu/ruakij/nzbStreamer/pkg/nzbparser"
)

// nzbWith builds an nzb of one file whose segments carry the given hints.
func nzbWith(hints ...int) *nzbparser.NzbData {
	segments := make([]nzbparser.Segment, len(hints))
	for i, hint := range hints {
		segments[i] = nzbparser.Segment{Index: i + 1, BytesHint: hint}
	}
	return &nzbparser.NzbData{Files: []nzbparser.File{{Segments: segments}}}
}

func TestContentConventionIsExact(t *testing.T) {
	sizer := NewSegmentSizer(nzbWith(768000, 768000, 768000, 400000))

	if sizer.Convention() != ConventionContent {
		t.Fatalf("convention = %v, want ConventionContent", sizer.Convention())
	}
	size, exact := sizer.Size(768000)
	if size != 768000 || !exact {
		t.Errorf("Size(768000) = %d, %v; want 768000, true", size, exact)
	}
}

func TestWireConventionResolvesToKnownSize(t *testing.T) {
	// 768000 content bytes posted as yEnc is a few percent larger on the wire
	const wireHint = 791000
	sizer := NewSegmentSizer(nzbWith(wireHint, wireHint, wireHint, 412000))

	if sizer.Convention() != ConventionWire {
		t.Fatalf("convention = %v, want ConventionWire", sizer.Convention())
	}
	size, exact := sizer.Size(wireHint)
	if size != 768000 || !exact {
		t.Errorf("Size(%d) = %d, %v; want 768000, true", wireHint, size, exact)
	}
}

// Escape overhead depends on the bytes being escaped, so full segments of one
// nzb carry hints that differ from each other. Every one of them still resolves
// to the same known size. The hints are from a real nzb of 716800-byte segments.
func TestWireConventionResolvesEveryFullSegment(t *testing.T) {
	hints := []int{739565, 739557, 739490, 739351, 739345, 739445}
	sizer := NewSegmentSizer(nzbWith(hints...))

	if sizer.Convention() != ConventionWire {
		t.Fatalf("convention = %v, want ConventionWire", sizer.Convention())
	}
	for _, hint := range hints {
		size, exact := sizer.Size(hint)
		if size != 716800 || !exact {
			t.Errorf("Size(%d) = %d, %v; want 716800, true", hint, size, exact)
		}
	}
}

// A short tail segment carries a hint of its own, which no convention can turn
// into an exact size.
func TestTailSegmentIsEstimated(t *testing.T) {
	const wireHint = 791000
	sizer := NewSegmentSizer(nzbWith(wireHint, wireHint, 412000))

	size, exact := sizer.Size(412000)
	if exact {
		t.Error("tail segment reported as exact")
	}
	if size >= 412000 {
		t.Errorf("Size(412000) = %d, want less than the hint", size)
	}
}

func TestUnknownConventionEstimates(t *testing.T) {
	sizer := NewSegmentSizer(nzbWith(500000, 500000, 500000))

	if sizer.Convention() != ConventionUnknown {
		t.Fatalf("convention = %v, want ConventionUnknown", sizer.Convention())
	}
	size, exact := sizer.Size(500000)
	if exact {
		t.Error("unknown convention reported as exact")
	}
	if size >= 500000 {
		t.Errorf("Size(500000) = %d, want less than the hint", size)
	}
}

// The convention is a property of the whole nzb, so every file in it has to be
// counted when picking the hint it is decided from.
func TestConventionSpansAllFiles(t *testing.T) {
	nzbData := nzbWith(400000)
	nzbData.Files = append(nzbData.Files, nzbparser.File{Segments: []nzbparser.Segment{
		{Index: 1, BytesHint: 768000},
		{Index: 2, BytesHint: 768000},
	}})

	sizer := NewSegmentSizer(nzbData)
	if sizer.Convention() != ConventionContent {
		t.Errorf("convention = %v, want ConventionContent", sizer.Convention())
	}
}

// An nzb whose segment size is not one of the known ones stays unknown until a
// decoded length says what its hint counted.
func TestSettlingAnUnknownConvention(t *testing.T) {
	const hint = 500000

	sizer := NewSegmentSizer(nzbWith(hint, hint, hint, 120000))
	if sizer.Convention() != ConventionUnknown {
		t.Fatalf("convention = %v, want ConventionUnknown", sizer.Convention())
	}

	content := sizer.SettleWith(hint, hint)
	if content.Convention() != ConventionContent {
		t.Errorf("a decoded length equal to the hint gave %v", content.Convention())
	}
	if size, exact := content.Size(hint); size != hint || !exact {
		t.Errorf("Size(%d) = %d, %v; want %d, true", hint, size, exact, hint)
	}

	// The same hint standing for 480000 decoded bytes plus yEnc overhead
	wire := sizer.SettleWith(hint, 480000)
	if wire.Convention() != ConventionWire {
		t.Errorf("a decoded length below the hint gave %v", wire.Convention())
	}
	if size, exact := wire.Size(hint); size != 480000 || !exact {
		t.Errorf("Size(%d) = %d, %v; want 480000, true", hint, size, exact)
	}
}

// Settling on a tail segment would take its decoded length for a full one, so
// only the hint the sizer identified as full is allowed to settle it.
func TestOnlyAFullSegmentSettlesTheConvention(t *testing.T) {
	sizer := NewSegmentSizer(nzbWith(500000, 500000, 120000))

	if got := sizer.SettleWith(120000, 120000); got.Convention() != ConventionUnknown {
		t.Errorf("a tail segment settled the convention as %v", got.Convention())
	}
}

// A pair that fits neither convention - a gap far wider than yEnc overhead -
// says the hint means something else entirely, so it settles nothing.
func TestAnImplausiblePairSettlesNothing(t *testing.T) {
	sizer := NewSegmentSizer(nzbWith(500000, 500000, 120000))

	if got := sizer.SettleWith(500000, 250000); got.Convention() != ConventionUnknown {
		t.Errorf("an implausible pair settled the convention as %v", got.Convention())
	}
}

// A probe carries on past a candidate that answers nothing, since one dead
// article says nothing about the nzb, and gives up once it has spent what an
// unknown convention is worth.
func TestProbingTriesPastACandidateThatSettlesNothing(t *testing.T) {
	const hint = 500000

	nzbData := nzbWith(hint, hint, hint, hint, 120000)
	nzbData.Files[0].Groups = []string{"alt.binaries.test"}
	for i := range nzbData.Files[0].Segments {
		nzbData.Files[0].Segments[i].ID = string(rune('a' + i))
	}

	fetched := []string{}
	fetch := func(_, id string) (int, error) {
		fetched = append(fetched, id)
		switch id {
		case "a":
			return 0, errors.New("article not found")
		case "b":
			// A length that fits neither convention
			return 250000, nil
		default:
			return hint, nil
		}
	}

	settled, err := NewSegmentSizer(nzbData).SettleByProbing(nzbData, fetch, 3)
	if err != nil {
		t.Fatalf("SettleByProbing: %v", err)
	}
	if settled.Convention() != ConventionContent {
		t.Errorf("convention = %v, want ConventionContent", settled.Convention())
	}
	if len(fetched) != 3 {
		t.Errorf("fetched %v, want three attempts", fetched)
	}

	// Nothing settled in those three attempts leaves the sizer as it was, and
	// reports why rather than looking like a success
	stubborn := func(_, _ string) (int, error) { return 0, errors.New("article not found") }
	unsettled, err := NewSegmentSizer(nzbData).SettleByProbing(nzbData, stubborn, 3)
	if err == nil {
		t.Errorf("a probe that answered nothing reported no error")
	}
	if unsettled.Convention() != ConventionUnknown {
		t.Errorf("convention = %v, want ConventionUnknown", unsettled.Convention())
	}

	// No attempts at all is what switches probing off, and it costs no article
	refuse := func(_, _ string) (int, error) {
		t.Error("probing fetched an article when it was allowed no attempts")
		return 0, nil
	}
	if _, err := NewSegmentSizer(nzbData).SettleByProbing(nzbData, refuse, 0); err != nil {
		t.Errorf("probing with no attempts reported %v", err)
	}
}

// An nzb that already knows its convention is not open to being told otherwise.
func TestSettlingLeavesAKnownConventionAlone(t *testing.T) {
	sizer := NewSegmentSizer(nzbWith(768000, 768000, 400000))

	if got := sizer.SettleWith(768000, 750000); got.Convention() != ConventionContent {
		t.Errorf("convention = %v, want ConventionContent", got.Convention())
	}
}
