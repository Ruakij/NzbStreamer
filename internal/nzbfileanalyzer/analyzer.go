// Package nzbfileanalyzer reconciles the disagreeing size conventions of nzb
// producers and estimates the size of each segment from them.
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
	512000,
	655360,
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
	return sizer.settleWithTotals(nzbData)
}

// Segment sizes are round: every size seen in the wild is a whole number of
// 10 KiB. Assuming that is what makes one derivable rather than merely bounded,
// since the escape overhead alone leaves a range of sizes that fit. A producer
// cutting somewhere else derives nothing and stays unknown, which is where it
// already was.
const segmentAlignment = 10 * 1024

// settleWithTotals identifies a convention the segment sizes alone could not,
// from the total size a subject says its file holds decoded. Adding up the
// bytes-hints of that file and comparing says which of the two the producer
// counted: the sums agree where the hints count content, and the sum is larger
// by the escape overhead where they count wire bytes.
//
// One file answers for the whole nzb, since a single tool built it.
func (s SegmentSizer) settleWithTotals(nzbData *nzbparser.NzbData) SegmentSizer {
	for i := range nzbData.Files {
		file := &nzbData.Files[i]
		total := file.TotalSizeHint
		if total <= 0 || len(file.Segments) < 2 {
			continue
		}

		var sum int64
		for _, segment := range file.Segments {
			sum += int64(segment.BytesHint)
		}

		if sum == total {
			s.convention = ConventionContent
			return s
		}

		// A file missing a segment sums short by about as much as the overhead
		// the two conventions are told apart by, so the wire case needs the
		// subject to confirm that all of them are here
		if file.SegmentCountHint != len(file.Segments) {
			continue
		}
		if sum < int64(float32(total)*(1+yEncOverheadMin)) || sum > int64(float32(total)*(1+yEncOverheadMax)) {
			continue
		}
		if fullSize, ok := fullSizeFrom(file, total); ok {
			s.convention = ConventionWire
			s.fullSize = fullSize
			return s
		}
	}

	return s
}

// fullSizeFrom derives the decoded size of a wire-counted files full segments:
// the one segment-aligned size that fits every full hint as a wire size and
// leaves a tail fitting its own. Shifting it by one alignment step moves the
// tail by a step per full segment, so the fit is unique for all but the shortest
// files, and where it is not the file says nothing.
func fullSizeFrom(file *nzbparser.File, total int64) (int, bool) {
	tail := 0
	for i := range file.Segments {
		if file.Segments[i].Index > file.Segments[tail].Index {
			tail = i
		}
	}

	minHint, maxHint := 0, 0
	for i := range file.Segments {
		hint := file.Segments[i].BytesHint
		if i == tail {
			continue
		}
		if hint < minHint || minHint == 0 {
			minHint = hint
		}
		if hint > maxHint {
			maxHint = hint
		}
	}

	fulls := int64(len(file.Segments) - 1)
	tailHint := int64(file.Segments[tail].BytesHint)
	lowest := int(float32(maxHint)/(1+yEncOverheadMax)) + segmentAlignment - 1

	var found, matches int
	for candidate := lowest - lowest%segmentAlignment; candidate < minHint; candidate += segmentAlignment {
		derivedTail := total - int64(candidate)*fulls
		if derivedTail <= 0 || derivedTail > tailHint || int64(float32(derivedTail)*(1+yEncOverheadMax)) < tailHint {
			continue
		}
		found, matches = candidate, matches+1
	}

	return found, matches == 1
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

// SegmentSize is what a segment contributes to its file decoded, and whether that
// is exact rather than an upper-bounded estimate.
type SegmentSize struct {
	Size  int
	Exact bool
}

// FileSizes sizes every segment of a file, in the order they are given.
//
// The total-size hint of the subject makes the last segment exact where every
// other one already is: the tail is what the hint leaves over. It is taken only
// when the length it yields fits the tails own bytes-hint, which rejects a hint
// counting something else, and a file the nzb is missing segments of.
func (s SegmentSizer) FileSizes(file *nzbparser.File) []SegmentSize {
	sizes := make([]SegmentSize, len(file.Segments))
	tail, sum := -1, 0
	for i := range file.Segments {
		size, exact := s.Size(file.Segments[i].BytesHint)
		sizes[i] = SegmentSize{Size: size, Exact: exact}
		sum += size
		if tail < 0 || file.Segments[i].Index > file.Segments[tail].Index {
			tail = i
		}
	}

	if tail < 0 || sizes[tail].Exact || file.TotalSizeHint <= 0 {
		return sizes
	}
	for i, size := range sizes {
		if i != tail && !size.Exact {
			return sizes
		}
	}

	derived := file.TotalSizeHint - int64(sum-sizes[tail].Size)
	if hint := int64(file.Segments[tail].BytesHint); derived > 0 && derived <= hint && int64(float32(derived)*(1+yEncOverheadMax)) >= hint {
		sizes[tail] = SegmentSize{Size: int(derived), Exact: true}
	}

	return sizes
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

// FetchSizeFunc downloads one article and returns its decoded length.
type FetchSizeFunc func(group, messageID string) (int, error)

// SettleByProbing resolves an unknown convention by downloading a full segment,
// for an nzb where nothing already known could settle it. It picks a segment
// carrying the hint the sizer took for a full one, so the length it learns is the
// decoded size of a full segment.
//
// This is the one thing in the add path that reads a body rather than checking
// that one exists. It costs an article, once per nzb ever, against every full
// segment in it becoming exact. A candidate that cannot be fetched or whose
// length fits neither convention is tried past, up to maxAttempts of them, since
// one dead article says nothing about the nzb. The error is what the last attempt
// failed with, and it is reported only when no attempt settled anything.
func (s SegmentSizer) SettleByProbing(nzbData *nzbparser.NzbData, fetchSize FetchSizeFunc, maxAttempts int) (SegmentSizer, error) {
	if s.convention != ConventionUnknown || maxAttempts <= 0 {
		return s, nil
	}

	var lastErr error
	for _, candidate := range s.probeCandidates(nzbData, maxAttempts) {
		size, err := fetchSize(candidate.group, candidate.id)
		if err != nil {
			lastErr = fmt.Errorf("failed fetching segment %s: %w", candidate.id, err)
			continue
		}

		if settled := s.SettleWith(s.fullSize, size); settled.convention != ConventionUnknown {
			return settled, nil
		}
		lastErr = fmt.Errorf("segment %s decoded to %d bytes against a hint of %d, which fits neither convention", candidate.id, size, s.fullSize)
	}

	return s, lastErr
}

type probeCandidate struct {
	group string
	id    string
}

// probeCandidates picks the segments worth probing: the ones carrying the hint
// the sizer took for a full segment, since only those can settle anything.
func (s SegmentSizer) probeCandidates(nzbData *nzbparser.NzbData, maxAttempts int) []probeCandidate {
	candidates := make([]probeCandidate, 0, maxAttempts)

	for i := range nzbData.Files {
		file := &nzbData.Files[i]
		if len(file.Groups) == 0 {
			continue
		}

		for _, segment := range file.Segments {
			if segment.BytesHint != s.fullSize {
				continue
			}

			candidates = append(candidates, probeCandidate{group: file.Groups[0], id: segment.ID})
			if len(candidates) == maxAttempts {
				return candidates
			}
		}
	}

	return candidates
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
