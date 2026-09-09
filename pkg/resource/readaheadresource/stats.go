package readaheadresource

import "sync/atomic"

// What every window in the process has pulled in and what of it was thrown away
// unread. Only the fetch path can keep these, so they are counted rather than
// derived, and they are package-level because there is one window per open file
// and attributing per file is a label that turns over with every open.
var (
	fetched   atomic.Int64
	discarded atomic.Int64
)

// Stats reports the bytes the windows pulled from underneath them against the
// bytes of the chunks that were evicted or closed without a single read copying
// out of them, which is what the window guessed wrong about: a seek past them,
// or a file closed mid-stream.
//
// A chunk one byte was read from counts as read whole, so the discarded side is
// the readahead nothing touched at all rather than the part of it that went
// unused.
func Stats() (fetchedBytes, discardedBytes int64) {
	return fetched.Load(), discarded.Load()
}
