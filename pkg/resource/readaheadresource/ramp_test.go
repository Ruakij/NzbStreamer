package readaheadresource_test

import (
	"errors"
	"io"
	"math/rand"
	"sync/atomic"
	"testing"
	"time"

	"git.ruekov.eu/ruakij/nzbStreamer/pkg/resource/readaheadresource"
)

// latencyResource is a plain file whose every read costs what a segment costs,
// which is what makes the window worth having and overfetch worth counting.
type latencyResource struct {
	size     int64
	latency  time.Duration
	fetched  atomic.Int64
	calls    atomic.Int64
	inflight atomic.Int64
	peak     atomic.Int64
}

func (r *latencyResource) Open() (io.ReadSeekCloser, error) { return &latencyHandle{resource: r}, nil }
func (r *latencyResource) SizeHint() (int64, error)         { return r.size, nil }

type latencyHandle struct {
	resource *latencyResource
	position int64
}

func (h *latencyHandle) ReadAt(p []byte, off int64) (int, error) {
	h.resource.calls.Add(1)
	if n := h.resource.inflight.Add(1); n > h.resource.peak.Load() {
		h.resource.peak.Store(n)
	}
	defer h.resource.inflight.Add(-1)
	time.Sleep(h.resource.latency)
	if off >= h.resource.size {
		return 0, io.EOF
	}
	n := len(p)
	if rest := h.resource.size - off; int64(n) > rest {
		n = int(rest)
	}
	h.resource.fetched.Add(int64(n))
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

func (h *latencyHandle) Read(p []byte) (int, error) {
	n, err := h.ReadAt(p, h.position)
	h.position += int64(n)
	return n, err
}

func (h *latencyHandle) Seek(offset int64, whence int) (int64, error) {
	switch whence {
	case io.SeekStart:
		h.position = offset
	case io.SeekCurrent:
		h.position += offset
	case io.SeekEnd:
		h.position = h.resource.size + offset
	}
	return h.position, nil
}
func (h *latencyHandle) Close() error { return nil }

const (
	benchFile      = 8 << 20
	benchChunk     = 64 << 10
	benchMaxSize   = 16 * benchChunk
	benchLatency   = 2 * time.Millisecond
	benchRampMin   = benchChunk
	benchRampSpeed = 2.0
)

// readPatterns is what a presenter actually does to a file: stream it, take a
// header and go, scrub, or read scattered ranges the way a player does over
// fuse. Each returns the bytes it wanted.
var readPatterns = []struct {
	name string
	read func(r io.ReadSeekCloser, size int64) int64
}{
	{"Sequential", func(r io.ReadSeekCloser, _ int64) int64 {
		buf := make([]byte, 128<<10)
		var read int64
		for {
			n, err := r.Read(buf)
			read += int64(n)
			if err != nil {
				return read
			}
		}
	}},
	{"HeaderOnly", func(r io.ReadSeekCloser, _ int64) int64 {
		buf := make([]byte, 256<<10)
		n, _ := io.ReadFull(r, buf)
		return int64(n)
	}},
	{"HeadAndTail", func(r io.ReadSeekCloser, size int64) int64 {
		buf := make([]byte, 128<<10)
		n, _ := io.ReadFull(r, buf)
		if _, err := r.Seek(size-int64(len(buf)), io.SeekStart); err != nil {
			return int64(n)
		}
		m, _ := io.ReadFull(r, buf)
		return int64(n + m)
	}},
	{"Scrub", func(r io.ReadSeekCloser, size int64) int64 {
		buf := make([]byte, 128<<10)
		var read int64
		for _, start := range []int64{0, size / 2, size / 4} {
			if _, err := r.Seek(start, io.SeekStart); err != nil {
				return read
			}
			for range 8 {
				n, err := r.Read(buf)
				read += int64(n)
				if err != nil {
					break
				}
			}
		}
		return read
	}},
	{"Scattered", func(r io.ReadSeekCloser, size int64) int64 {
		at, ok := r.(io.ReaderAt)
		if !ok {
			return 0
		}
		rng := rand.New(rand.NewSource(1))
		buf := make([]byte, 4<<10)
		var read int64
		for range 32 {
			n, err := at.ReadAt(buf, rng.Int63n(size-int64(len(buf))))
			read += int64(n)
			if err != nil && !errors.Is(err, io.EOF) {
				return read
			}
		}
		return read
	}},
}

// BenchmarkReadPatterns reports what each pattern costs in wall time and in
// bytes pulled off the underlying resource, which is the whole question the
// ramp answers: fetched/wanted is what a reader pays for what it did not read.
func BenchmarkReadPatterns(b *testing.B) {
	for _, pattern := range readPatterns {
		b.Run(pattern.name, func(b *testing.B) {
			underlying := &latencyResource{size: benchFile, latency: benchLatency}
			resource := readaheadresource.New(underlying, benchRampMin, benchMaxSize, benchChunk, benchRampSpeed)

			var wanted int64
			b.ReportAllocs()
			for b.Loop() {
				reader, err := resource.Open()
				if err != nil {
					b.Fatal(err)
				}
				wanted += pattern.read(reader, benchFile)
				if err := reader.Close(); err != nil {
					b.Fatal(err)
				}
			}

			iterations := float64(b.N)
			fetched := float64(underlying.fetched.Load())
			b.ReportMetric(fetched/iterations/(1<<20), "MiB-fetched/op")
			b.ReportMetric(float64(wanted)/iterations/(1<<20), "MiB-wanted/op")
			b.ReportMetric(fetched/float64(wanted), "overfetch")
			b.ReportMetric(float64(underlying.calls.Load())/iterations, "fetches/op")
		})
	}
}

// TestSeekingBackWarmsTheNewPosition covers what a webdav client does with a
// reused reader: a seek behind the head has to move the window with it, or the
// rest of the request is served one chunk at a time.
func TestSeekingBackWarmsTheNewPosition(t *testing.T) {
	const chunk = 64 << 10

	underlying := &latencyResource{size: 8 << 20, latency: 2 * time.Millisecond}
	resource := readaheadresource.New(underlying, 4*chunk, 16*chunk, chunk, 2)
	reader, err := resource.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()

	buf := make([]byte, chunk)
	if _, err := io.ReadFull(reader, buf); err != nil {
		t.Fatal(err)
	}
	if _, err := reader.Seek(4<<20, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadFull(reader, buf); err != nil {
		t.Fatal(err)
	}

	if _, err := reader.Seek(1<<20, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	underlying.peak.Store(0)
	if _, err := io.ReadFull(reader, buf); err != nil {
		t.Fatal(err)
	}

	if peak := underlying.peak.Load(); peak < 2 {
		t.Errorf("%d fetches in flight after seeking back, want the window warming ahead of it", peak)
	}
}

// TestReadingFarBehindMovesTheWindow is a player scrubbing backwards over fuse,
// which reaches us as reads at a lower offset and nothing else: lseek never
// leaves the kernel.
func TestReadingFarBehindMovesTheWindow(t *testing.T) {
	const chunk = 64 << 10

	underlying := &latencyResource{size: 8 << 20, latency: 2 * time.Millisecond}
	reader, err := readaheadresource.New(underlying, 16*chunk, 16*chunk, chunk, 1).Open()
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()

	at, ok := reader.(io.ReaderAt)
	if !ok {
		t.Fatal("reader is not a ReaderAt")
	}

	buf := make([]byte, chunk)
	if _, err := at.ReadAt(buf, 6<<20); err != nil {
		t.Fatal(err)
	}

	// Fetching ahead of where the reader went, rather than one chunk where it was
	underlying.peak.Store(0)
	if _, err := at.ReadAt(buf, 1<<20); err != nil {
		t.Fatal(err)
	}
	if peak := underlying.peak.Load(); peak < 2 {
		t.Errorf("%d fetches in flight for the read after the scrub, want the window warming ahead of it", peak)
	}
}

// settledCalls is what the reader has fetched once the window has caught up. A
// fetch counts itself from its own goroutine, so a count taken straight after a
// read sees whichever of them happened to have started.
func settledCalls(t *testing.T, r *latencyResource) int64 {
	t.Helper()

	last := r.calls.Load()
	for range 500 {
		time.Sleep(time.Millisecond)
		now := r.calls.Load()
		if now == last {
			return now
		}
		last = now
	}

	t.Fatal("fetches never settled")
	return 0
}

// TestAMinimumOfZeroOpensOnOneChunk is what a caller asking for no minimum
// gets: the chunk the read needs and nothing beside it, growing from there.
func TestAMinimumOfZeroOpensOnOneChunk(t *testing.T) {
	const chunk = 64 << 10

	underlying := &latencyResource{size: 8 << 20}
	reader, err := readaheadresource.New(underlying, 0, 16*chunk, chunk, 2).Open()
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()

	buf := make([]byte, chunk)
	if _, err := io.ReadFull(reader, buf); err != nil {
		t.Fatal(err)
	}
	if calls := settledCalls(t, underlying); calls != 1 {
		t.Errorf("%d fetches for the first read, want the one chunk it asked for", calls)
	}

	// Doubling per chunk read in order, so the fifth of them is warming all 16
	for range 4 {
		if _, err := io.ReadFull(reader, buf); err != nil {
			t.Fatal(err)
		}
	}
	// 1, 2, 4, 8 and then the whole window of 16
	if calls := settledCalls(t, underlying); calls != 20 {
		t.Errorf("%d fetches over five chunks read in order, want 20 for a window doubling to its full 16", calls)
	}
}

// TestARampSpeedOfOneHoldsTheWindow is the fixed window: whatever it opened on
// is what every read gets, so a reader that runs on earns nothing.
func TestARampSpeedOfOneHoldsTheWindow(t *testing.T) {
	const chunk = 64 << 10

	underlying := &latencyResource{size: 8 << 20}
	reader, err := readaheadresource.New(underlying, 4*chunk, 16*chunk, chunk, 1).Open()
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()

	buf := make([]byte, chunk)
	if _, err := io.ReadFull(reader, buf); err != nil {
		t.Fatal(err)
	}
	if calls := settledCalls(t, underlying); calls != 4 {
		t.Errorf("%d fetches for the first read, want the four chunks it opened on", calls)
	}

	// Each further chunk read in order brings one new chunk into a window that
	// does not grow
	for i := range 8 {
		if _, err := io.ReadFull(reader, buf); err != nil {
			t.Fatal(err)
		}
		if calls, want := settledCalls(t, underlying), int64(5+i); calls != want {
			t.Fatalf("%d fetches after reading %d chunks, want %d", calls, i+2, want)
		}
	}
}

// TestReadingFarBehindCostsTheWindow covers the other half of a jump: a
// positional read nowhere near the head is not the stream running on, so the
// window it earned goes even though the anchor cannot follow it.
func TestReadingFarBehindCostsTheWindow(t *testing.T) {
	const chunk = 64 << 10

	fetches := func(behind bool) int64 {
		underlying := &latencyResource{size: 8 << 20}
		resource := readaheadresource.New(underlying, 4*chunk, 16*chunk, chunk, 2)
		reader, err := resource.Open()
		if err != nil {
			t.Fatal(err)
		}
		defer reader.Close()

		at, ok := reader.(io.ReaderAt)
		if !ok {
			t.Fatal("reader is not a ReaderAt")
		}

		// Ramp the window to its full size, then leave the head far behind
		buf := make([]byte, chunk)
		for off := int64(0); off < 3<<20; off += chunk {
			if _, err := at.ReadAt(buf, off); err != nil {
				t.Fatal(err)
			}
		}
		if behind {
			if _, err := at.ReadAt(buf, 0); err != nil {
				t.Fatal(err)
			}
		}

		before := underlying.calls.Load()
		if _, err := at.ReadAt(buf, 6<<20); err != nil {
			t.Fatal(err)
		}
		return underlying.calls.Load() - before
	}

	plain, behind := fetches(false), fetches(true)
	if behind >= plain {
		t.Errorf("%d fetches after reading far behind, want fewer than the %d of a window still at full size", behind, plain)
	}
}

// patternResource fills every read from the offset, so a byte says where it came
// from, and writes it in two halves around a pause: a fetch whose buffer is
// pooled underneath it corrupts whichever chunk takes that buffer next, and the
// pause is what makes the two overlap reliably.
type patternResource struct{ size int64 }

func (r *patternResource) Open() (io.ReadSeekCloser, error) { return &patternHandle{size: r.size}, nil }
func (r *patternResource) SizeHint() (int64, error)         { return r.size, nil }

type patternHandle struct {
	size     int64
	position int64
}

func patternByte(off int64) byte { return byte(off*31 + off/251) }

func (h *patternHandle) ReadAt(p []byte, off int64) (int, error) {
	if off >= h.size {
		return 0, io.EOF
	}
	n := len(p)
	if rest := h.size - off; int64(n) > rest {
		n = int(rest)
	}
	for i := range n / 2 {
		p[i] = patternByte(off + int64(i))
	}
	time.Sleep(time.Millisecond)
	for i := n / 2; i < n; i++ {
		p[i] = patternByte(off + int64(i))
	}
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

func (h *patternHandle) Read(p []byte) (int, error) {
	n, err := h.ReadAt(p, h.position)
	h.position += int64(n)
	return n, err
}

func (h *patternHandle) Seek(offset int64, whence int) (int64, error) {
	switch whence {
	case io.SeekStart:
		h.position = offset
	case io.SeekCurrent:
		h.position += offset
	case io.SeekEnd:
		h.position = h.size + offset
	}
	return h.position, nil
}
func (h *patternHandle) Close() error { return nil }

// TestReadsSkippingAheadStayCorrect strides far enough per read to evict the
// chunks the previous one warmed while they are still being fetched.
func TestReadsSkippingAheadStayCorrect(t *testing.T) {
	const (
		size   = 4 << 20
		chunk  = 4 << 10
		stride = 128 << 10
	)

	resource := readaheadresource.New(&patternResource{size: size}, chunk, 256<<10, chunk, 2)
	reader, err := resource.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()

	at, ok := reader.(io.ReaderAt)
	if !ok {
		t.Fatal("reader is not a ReaderAt")
	}

	buf := make([]byte, chunk)
	for off := int64(0); off+chunk <= size; off += stride {
		if _, err := at.ReadAt(buf, off); err != nil {
			t.Fatalf("read at %d: %v", off, err)
		}
		for i, got := range buf {
			if want := patternByte(off + int64(i)); got != want {
				t.Fatalf("byte %d of the read at %d = %d, want %d", i, off, got, want)
			}
		}
	}
}
