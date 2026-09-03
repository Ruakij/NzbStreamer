package nzbfileanalyzer

import (
	"fmt"

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

func (c SizeConvention) String() string {
	switch c {
	case ConventionContent:
		return "content"
	case ConventionWire:
		return "wire"
	default:
		return "unknown"
	}
}

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
	// fullSize is the decoded size of a full segment
	fullSize int
}

// NewSegmentSizer determines the convention of an nzb.
func NewSegmentSizer(nzbData *nzbparser.NzbData) SegmentSizer {
	hint := mostCommonHint(nzbData)
	sizer := SegmentSizer{fullSize: hint}

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
//
// yEnc escaping depends on the bytes being escaped, so every full segment of a
// wire-counted nzb carries a slightly different hint. What identifies one is the
// hint landing in the overhead range above the known size, not matching any
// particular other hint.
func (s SegmentSizer) Size(hint int) (int, bool) {
	switch {
	case s.convention == ConventionContent:
		return hint, true
	case s.convention == ConventionWire && s.isFullWireHint(hint):
		return s.fullSize, true
	}
	// A short tail segment, or an nzb whose convention stayed unknown. Content is
	// never larger than wire, so the low end of the overhead range is the
	// smallest size the hint can stand for.
	return int(float32(hint) * (1 - yEncOverheadMax)), false
}

// SettleWith resolves a convention the nzb alone could not identify, from one
// segment whose decoded length is known: comparing that length against its own
// hint says directly which of the two the producer counted.
//
// Only the hint of a full segment can settle it, since the wire case needs the
// decoded size of a full segment to be exact about the rest. That is the hint
// the sizer already holds - the most common one, which belongs to a full segment
// because every file has at most one short one. A hint that is not it, or a pair
// that fits neither convention, leaves the sizer unknown.
func (s SegmentSizer) SettleWith(hint, size int) SegmentSizer {
	if s.convention != ConventionUnknown || hint != s.fullSize || size <= 0 {
		return s
	}

	switch {
	case size == hint:
		s.convention = ConventionContent
	case hint > size && hint >= int(float32(size)*(1+yEncOverheadMin)) && hint <= int(float32(size)*(1+yEncOverheadMax)):
		s.convention = ConventionWire
		s.fullSize = size
	}

	return s
}

// DecodeFunc returns the decoded length of one article.
type DecodeFunc func(group, messageID string) (int, error)

// SettleByProbing resolves an unknown convention by decoding a single full
// segment, for an nzb where nothing already known could settle it. It picks a
// segment carrying the hint the sizer took for a full one, so the length it
// learns is the decoded size of a full segment.
//
// This is the one thing in the add path that reads a body rather than checking
// that one exists. It costs a single article, once per nzb ever, against every
// full segment in it becoming exact.
func (s SegmentSizer) SettleByProbing(nzbData *nzbparser.NzbData, decode DecodeFunc) (SegmentSizer, error) {
	if s.convention != ConventionUnknown {
		return s, nil
	}

	for i := range nzbData.Files {
		file := &nzbData.Files[i]
		if len(file.Groups) == 0 {
			continue
		}

		for _, segment := range file.Segments {
			if segment.BytesHint != s.fullSize {
				continue
			}

			size, err := decode(file.Groups[0], segment.ID)
			if err != nil {
				return s, fmt.Errorf("failed decoding segment %s: %w", segment.ID, err)
			}
			return s.SettleWith(segment.BytesHint, size), nil
		}
	}

	return s, nil
}

// isFullWireHint reports whether a hint is the wire size of a full segment, which
// is fullSize plus an escape overhead that never exceeds yEncOverheadMax.
func (s SegmentSizer) isFullWireHint(hint int) bool {
	return hint > s.fullSize && hint <= int(float32(s.fullSize)*(1+yEncOverheadMax))
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
