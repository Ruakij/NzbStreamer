package nzbfileanalyzer

import (
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
