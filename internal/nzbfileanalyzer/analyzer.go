package nzbfileanalyzer

import (
	"git.ruekov.eu/ruakij/nzbStreamer/pkg/nzbparser"
)

// SizeConvention says what a segments bytes-attribute counts. Nzb producers
// disagree about it, and the attribute itself carries no indication of which
// they used.
type SizeConvention int

const (
	// ConventionUnknown means it could not be determined from the nzb alone.
	ConventionUnknown SizeConvention = iota
	// ConventionContent counts the bytes of the decoded payload, which is the
	// length the segment contributes to the file.
	ConventionContent
	// ConventionWire counts the bytes of the posted article: the yEnc-encoded
	// payload plus its header and trailer lines and the escape overhead.
	ConventionWire
)

// Segment sizes commonly chosen by posting tools. They are exact multiples of
// 1024, which a yEnc-encoded length essentially never is, so a hint landing on
// one of them identifies the convention.
var knownSizes = []int{
	716800,
	768000,
	3584000,
}

const (
	yEncOverheadMin float32 = 0.0203435
	yEncOverheadMax float32 = 0.0453969
)

// SegmentSizer converts a segments bytes-hint into a decoded-payload size.
//
// A single tool builds a whole nzb, so the convention is uniform within it and
// is decided once, from the hint that occurs most often - the size of a full
// segment. Deciding per segment instead would let one unlucky hint disagree with
// its neighbours about what the same attribute means.
type SegmentSizer struct {
	convention SizeConvention
	// fullHint is the hint a full segment carries, fullSize its decoded size
	fullHint int
	fullSize int
}

// NewSegmentSizer determines the convention of an nzb.
func NewSegmentSizer(nzbData *nzbparser.NzbData) SegmentSizer {
	hint := mostCommonHint(nzbData)
	sizer := SegmentSizer{fullHint: hint, fullSize: hint}

	for _, known := range knownSizes {
		switch {
		case hint == known:
			sizer.convention = ConventionContent
			return sizer
		case hint > known && hint <= int(float32(known)*(1+yEncOverheadMax)):
			// A full segment holds known bytes, and the hint is that plus yEnc
			// overhead, so the whole nzb counts wire bytes
			sizer.convention = ConventionWire
			sizer.fullSize = known
			return sizer
		}
	}

	sizer.convention = ConventionUnknown
	return sizer
}

// Convention reports what the nzbs bytes-attribute was found to count.
func (s SegmentSizer) Convention() SizeConvention {
	return s.convention
}

// Size returns the decoded size a segment contributes to its file, and whether
// that is exact rather than an upper-bounded estimate.
func (s SegmentSizer) Size(hint int) (int, bool) {
	switch {
	case s.convention == ConventionContent:
		return hint, true
	case s.convention == ConventionWire && hint == s.fullHint:
		return s.fullSize, true
	}
	// A short tail segment, or an nzb whose convention stayed unknown. Content is
	// never larger than wire, so the low end of the overhead range is the
	// smallest size the hint can stand for.
	return int(float32(hint) * (1 - yEncOverheadMax)), false
}

// mostCommonHint returns the bytes-hint shared by the most segments in the nzb,
// which is the hint of a full segment: every file has at most one short segment,
// its last.
func mostCommonHint(nzbData *nzbparser.NzbData) int {
	counts := make(map[int]int)
	for i := range nzbData.Files {
		for _, segment := range nzbData.Files[i].Segments {
			if segment.BytesHint > 0 {
				counts[segment.BytesHint]++
			}
		}
	}

	var hint, best int
	for size, count := range counts {
		// Prefer the larger hint on a tie, so a two-segment nzb does not settle
		// on its short tail
		if count > best || (count == best && size > hint) {
			hint, best = size, count
		}
	}
	return hint
}
