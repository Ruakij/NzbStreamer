// Package readclient plays a read pattern against the streamer over HTTP
// (WebDAV). A pattern is an ordered list of [start, length) ranges over a file
// of known size; the transport fetches each one with a Range request.
package readclient

import (
	"context"
	"fmt"
	"io"
	"math"
	"math/rand"
	"net/http"
	"slices"
	"strings"
	"time"
)

// Range is a byte span to read from a file.
type Range struct {
	Offset, Len int64
}

// ReadPatterns are the read types the matrix can sweep. ParseMatrix validates
// against this, so a typo is refused before anything is brought up rather than
// panicking in Plan minutes into a run.
var ReadPatterns = []string{"sequential", "random", "tail", "stride"}

// KnownPattern reports whether readtype is one Plan can serve.
func KnownPattern(readtype string) bool { return slices.Contains(ReadPatterns, readtype) }

// Plan returns the ranges that make up one read pass of readtype over a file
// of size bytes. chunk is the granularity patterns operate at. The returned
// ranges never overlap. A size <= 0 (a file whose length is unknown, like an
// archive that stays an archive) collapses every pattern to a whole-file read.
func Plan(readtype string, size, chunk, seed int64) []Range {
	if size <= 0 {
		// Whole file, unknown length. The transport reads it as one GET.
		return []Range{{0, -1}}
	}
	ranges := func() []Range {
		var out []Range
		for off := int64(0); off < size; off += chunk {
			l := chunk
			if size-off < l {
				l = size - off
			}
			out = append(out, Range{off, l})
		}
		return out
	}

	switch readtype {
	case "sequential":
		return []Range{{0, size}}
	case "tail":
		// A player wants the index at the end of the file first, then plays
		// from the start: read the last chunk, then everything before it.
		if size <= chunk {
			return []Range{{0, size}}
		}
		return []Range{{size - chunk, chunk}, {0, size - chunk}}
	case "random":
		// A reader that seeks: every chunk once, in a shuffled order. The seed
		// makes a given cell reproducible.
		chunks := ranges()
		rand.New(rand.NewSource(seed)).Shuffle(len(chunks), func(i, j int) { chunks[i], chunks[j] = chunks[j], chunks[i] })
		return chunks
	case "stride":
		// A scatter-read: every fourth chunk, which is what parallelism wants.
		step := chunk * 4
		var out []Range
		for off := int64(0); off < size; off += step {
			l := chunk
			if size-off < l {
				l = size - off
			}
			out = append(out, Range{off, l})
		}
		return out
	default:
		panic("unknown readtype " + readtype)
	}
}

// ReadHTTP fetches every range of plan from base/webdav/path and writes the
// bytes to w (nil means discard), returning the total bytes read, the time
// until the first body byte of the plan arrived (0 when no range delivered
// bytes, i.e. a read that failed up front), a per-range list of TTFB samples,
// and a per-range list of call->reply RTT samples aligned with plan (how long
// each range's request-and-full-reply exchange took).
func ReadHTTP(ctx context.Context, client *http.Client, base, path string, plan []Range, w io.Writer) (int64, time.Duration, []time.Duration, []time.Duration, error) {
	if w == nil {
		w = io.Discard
	}
	var total int64
	var ttfb time.Duration
	perRange := make([]time.Duration, 0, len(plan))
	perRTT := make([]time.Duration, 0, len(plan))
	for i, r := range plan {
		n, t, rtt, err := ranged(ctx, client, base, path, r, w)
		if i == 0 {
			ttfb = t
		}
		perRange = append(perRange, t)
		perRTT = append(perRTT, rtt)
		if err != nil {
			return total, ttfb, perRange, perRTT, fmt.Errorf("%s %+v: %w", path, r, err)
		}
		if n, ok := hasLen(r); ok && n != r.Len {
			return total, ttfb, perRange, perRTT, fmt.Errorf("%s %+v: read %d of %d bytes", path, r, n, r.Len)
		}
		total += n
	}
	return total, ttfb, perRange, perRTT, nil
}

// PercentileDur returns the q-th percentile (0..100) of a duration sample,
// linearly interpolated between the two neighbouring ranks and clamped at the
// ends, and 0 for an empty sample.
func PercentileDur(xs []time.Duration, q float64) time.Duration {
	if len(xs) == 0 {
		return 0
	}
	s := make([]time.Duration, len(xs))
	copy(s, xs)
	slices.Sort(s)
	if len(s) == 1 {
		return s[0]
	}
	idx := q / 100 * float64(len(s)-1)
	if idx < 0 {
		idx = 0
	}
	if idx > float64(len(s)-1) {
		idx = float64(len(s) - 1)
	}
	lo := int(math.Floor(idx))
	hi := int(math.Ceil(idx))
	if lo == hi {
		return s[lo]
	}
	frac := idx - float64(lo)
	// Round to absorb float imprecision in the interpolation; a truncated
	// fraction would systematically shave sub-nanosecond amounts off samples.
	return time.Duration(math.Round(float64(s[lo]) + float64(s[hi]-s[lo])*frac))
}

// hasLen reports an open range (unknown whole-file length) as (n, false).
func hasLen(r Range) (int64, bool) { return r.Len, r.Len >= 0 }

// ranged fetches one range. A whole-file range is a plain GET; a partial one
// carries a Range header. ttfb is the time from the request going out to the
// first body byte of this range arriving (0 if the response carried no bytes).
// rtt is the full call->reply round trip: from the request going out to the
// whole response body having been consumed (0 if the exchange failed up front).
func ranged(ctx context.Context, client *http.Client, base, path string, r Range, w io.Writer) (int64, time.Duration, time.Duration, error) {
	u := strings.TrimSuffix(base, "/") + "/webdav/" + path
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return 0, 0, 0, err
	}
	whole := r.Offset == 0 && r.Len <= 0
	if !whole {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", r.Offset, r.Offset+r.Len-1))
	}
	sent := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		return 0, 0, 0, err
	}
	defer resp.Body.Close()
	if !whole && resp.StatusCode != http.StatusPartialContent {
		return 0, 0, 0, fmt.Errorf("%s: %s", u, resp.Status)
	}
	if whole && resp.StatusCode != http.StatusOK {
		return 0, 0, 0, fmt.Errorf("%s: %s", u, resp.Status)
	}
	first := &firstByteReader{r: resp.Body, sent: sent}
	n, err := io.Copy(w, first)
	return n, first.ttfb(), time.Since(sent), err
}

// firstByteReader clocks the first byte a body delivers after the request was
// sent. Wrapping the body does not touch the bytes, so the measurement is the
// actual wire latency, not a read of a copy.
type firstByteReader struct {
	r       io.Reader
	sent    time.Time
	clocked bool
	t       time.Duration
}

func (f *firstByteReader) Read(p []byte) (int, error) {
	n, err := f.r.Read(p)
	if n > 0 && !f.clocked {
		f.clocked = true
		f.t = time.Since(f.sent)
	}
	return n, err
}

func (f *firstByteReader) ttfb() time.Duration { return f.t }
