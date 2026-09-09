package readaheadresource

import "sync/atomic"

// What every window in the process has pulled in, what of it was thrown away
// unread, how much of it is being fetched right now and how wide the windows
// serving reads had ramped. Only the fetch path can keep these, so they are
// counted rather than derived, and they are package-level because there is one
// window per open file and attributing per file is a label that turns over with
// every open.
var (
	fetched   atomic.Int64
	discarded atomic.Int64
	inflight  atomic.Int64
	warmSum   atomic.Int64
	warmReads atomic.Int64
)

// Counts is what the windows have done. Bytes on the two sides that are about
// volume, chunks on the two that are about how many reads run at once: the
// chunk size is one setting for the whole process, so a count of chunks is the
// same number as the bytes it stands for without having to carry it.
type Counts struct {
	// FetchedBytes is what the windows pulled from underneath them, the chunk a
	// read asked for and the ones warmed ahead of it alike, and DiscardedBytes
	// the chunks evicted or closed without a single read copying out of them,
	// which is what the window guessed wrong about: a seek past them, or a file
	// closed mid-stream. A chunk one byte was read from counts as read whole, so
	// the discarded side is the readahead nothing touched at all rather than the
	// part of it that went unused.
	FetchedBytes   int64
	DiscardedBytes int64
	// InflightChunks is the reads the windows have outstanding underneath them
	// right now, which is the parallelism the layers below are actually asked
	// for rather than the one the window is allowed
	InflightChunks int64
	// WarmChunks is the warm window summed over the reads it served and
	// WarmReads those reads, so the two of them are the mean window a read ran
	// under. Against the width a window is configured for, that is whether the
	// ramp is reaching it at all.
	WarmChunks int64
	WarmReads  int64
}

// Stats reports what every window in the process has done between them.
func Stats() Counts {
	return Counts{
		FetchedBytes:   fetched.Load(),
		DiscardedBytes: discarded.Load(),
		InflightChunks: inflight.Load(),
		WarmChunks:     warmSum.Load(),
		WarmReads:      warmReads.Load(),
	}
}
