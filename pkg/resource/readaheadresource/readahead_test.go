package readaheadresource_test

import (
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"git.ruekov.eu/ruakij/nzbStreamer/pkg/resource/bytesresource"
	"git.ruekov.eu/ruakij/nzbStreamer/pkg/resource/readaheadresource"
)

func TestReadAndSeek(t *testing.T) {
	reader, err := readaheadresource.New(&bytesresource.BytesResource{Content: []byte("0123456789")}, 4, 4, 3, 0).Open()
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()

	buf := make([]byte, 5)
	if _, err := io.ReadFull(reader, buf); err != nil || string(buf) != "01234" {
		t.Fatalf("first read = %q, %v", buf, err)
	}
	if _, err := reader.Seek(2, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadFull(reader, buf); err != nil || string(buf) != "23456" {
		t.Fatalf("read after seek = %q, %v", buf, err)
	}
	if _, err := io.ReadAll(reader); err != nil && !errors.Is(err, io.EOF) {
		t.Fatal(err)
	}
}

type slowReader struct {
	data   []byte
	active atomic.Int64
	peak   atomic.Int64
	calls  atomic.Int64

	mu   sync.Mutex
	each map[int64]int // reads per offset
}

func (r *slowReader) Open() (io.ReadSeekCloser, error) { return &slowHandle{resource: r}, nil }
func (r *slowReader) SizeHint() (int64, error)         { return int64(len(r.data)), nil }

type slowHandle struct {
	resource *slowReader
	position int64
}

func (r *slowHandle) Read(p []byte) (int, error) {
	n, err := r.ReadAt(p, r.position)
	r.position += int64(n)
	return n, err
}
func (r *slowHandle) ReadAt(p []byte, off int64) (int, error) {
	r.resource.calls.Add(1)
	r.resource.mu.Lock()
	if r.resource.each == nil {
		r.resource.each = map[int64]int{}
	}
	r.resource.each[off]++
	r.resource.mu.Unlock()
	active := r.resource.active.Add(1)
	defer r.resource.active.Add(-1)
	for {
		peak := r.resource.peak.Load()
		if active <= peak || r.resource.peak.CompareAndSwap(peak, active) {
			break
		}
	}
	time.Sleep(time.Millisecond)
	if off >= int64(len(r.resource.data)) {
		return 0, io.EOF
	}
	n := copy(p, r.resource.data[off:])
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}
func (r *slowHandle) Seek(offset int64, whence int) (int64, error) {
	switch whence {
	case io.SeekStart:
		r.position = offset
	case io.SeekCurrent:
		r.position += offset
	case io.SeekEnd:
		r.position = int64(len(r.resource.data)) + offset
	}
	return r.position, nil
}
func (r *slowHandle) Close() error { return nil }

func TestReadsAheadInParallel(t *testing.T) {
	underlying := &slowReader{data: make([]byte, 32)}
	reader, err := readaheadresource.New(underlying, 16, 16, 4, 0).Open()
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	if _, err := io.ReadFull(reader, make([]byte, 16)); err != nil {
		t.Fatal(err)
	}
	if underlying.peak.Load() != 4 {
		t.Fatalf("peak reads = %d, want 4", underlying.peak.Load())
	}
}

// Fuse only ever reads positionally, and out of order, so the window has to be
// what serves those reads rather than something a stream drives past it.
func TestReadAtIsServedFromTheWindow(t *testing.T) {
	underlying := &slowReader{data: make([]byte, 64)}
	opened, err := readaheadresource.New(underlying, 16, 16, 4, 0).Open()
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()

	reader, ok := opened.(io.ReaderAt)
	if !ok {
		t.Fatal("reader is not positional; fuse would serialise it behind a seeking wrapper")
	}

	// The chunks ahead are warm, and one behind the head does not drop them.
	// Counting every read would race the warming the advancing anchor starts,
	// so what is asserted is that no chunk was fetched twice; Close waits for
	// the fetches still in flight.
	buf := make([]byte, 4)
	for _, off := range []int64{16, 20, 24, 28, 12} {
		if _, err := reader.ReadAt(buf, off); err != nil {
			t.Fatal(err)
		}
	}
	if underlying.peak.Load() != 4 {
		t.Fatalf("peak reads = %d, want the whole window", underlying.peak.Load())
	}
	if err := opened.Close(); err != nil {
		t.Fatal(err)
	}

	underlying.mu.Lock()
	defer underlying.mu.Unlock()
	for off, n := range underlying.each {
		if n != 1 {
			t.Fatalf("offset %d read %d times; want every chunk fetched once", off, n)
		}
	}
	for _, off := range []int64{16, 20, 24, 28} {
		if underlying.each[off] != 1 {
			t.Fatalf("chunk %d was not held warm", off)
		}
	}
}

// A reader that takes a header and closes must not pay for the whole window.
func TestShortReadDoesNotFetchTheWholeWindow(t *testing.T) {
	underlying := &slowReader{data: make([]byte, 1024)}
	opened, err := readaheadresource.New(underlying, 4, 64, 4, 2).Open()
	if err != nil {
		t.Fatal(err)
	}

	if _, err := io.ReadFull(opened, make([]byte, 4)); err != nil {
		t.Fatal(err)
	}
	if err := opened.Close(); err != nil {
		t.Fatal(err)
	}

	if calls := underlying.calls.Load(); calls > 2 {
		t.Fatalf("one chunk read cost %d fetches, want at most 2 of a 16-chunk window", calls)
	}
}

// Scattered reads warm nothing worth having, so the ramp starts again at every
// jump out of the window rather than growing on forward motion that is not one.
func TestScatteredReadsDoNotRampTheWindow(t *testing.T) {
	underlying := &slowReader{data: make([]byte, 1024)}
	opened, err := readaheadresource.New(underlying, 4, 64, 4, 2).Open()
	if err != nil {
		t.Fatal(err)
	}

	reader, ok := opened.(io.ReaderAt)
	if !ok {
		t.Fatal("reader is not positional")
	}
	buf := make([]byte, 4)
	for _, off := range []int64{0, 100, 200, 300, 400, 500} {
		if _, err := reader.ReadAt(buf, off); err != nil {
			t.Fatal(err)
		}
	}
	if err := opened.Close(); err != nil {
		t.Fatal(err)
	}

	if calls := underlying.calls.Load(); calls > 12 {
		t.Fatalf("6 scattered reads cost %d fetches, want them not to ramp the window", calls)
	}
}

// One seek in a stream is a player, not a scattered reader, so what it earned
// is halved rather than dropped and the read after it is still warm.
func TestSeekKeepsHalfTheWindow(t *testing.T) {
	underlying := &slowReader{data: make([]byte, 1024)}
	opened, err := readaheadresource.New(underlying, 4, 64, 4, 2).Open()
	if err != nil {
		t.Fatal(err)
	}

	reader, ok := opened.(io.ReaderAt)
	if !ok {
		t.Fatal("reader is not positional")
	}
	buf := make([]byte, 4)
	for _, off := range []int64{0, 4, 8, 12, 16, 500} {
		if _, err := reader.ReadAt(buf, off); err != nil {
			t.Fatal(err)
		}
	}
	if err := opened.Close(); err != nil {
		t.Fatal(err)
	}

	underlying.mu.Lock()
	defer underlying.mu.Unlock()
	if underlying.each[504] != 1 {
		t.Fatal("the chunk after the seek was not warmed; the seek dropped the whole window")
	}
}

// Closing on the first chunk of a warm window throws the rest of it away, which
// is the readahead the reader never asked for.
func TestClosingCountsTheReadaheadNothingRead(t *testing.T) {
	fetchedBefore, discardedBefore := readaheadresource.Stats()

	underlying := &slowReader{data: make([]byte, 1024)}
	opened, err := readaheadresource.New(underlying, 16, 64, 4, 1).Open()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadFull(opened, make([]byte, 4)); err != nil {
		t.Fatal(err)
	}
	if err := opened.Close(); err != nil {
		t.Fatal(err)
	}

	fetched, discarded := readaheadresource.Stats()
	if fetched-fetchedBefore != 16 {
		t.Errorf("fetched %d bytes, want the 4 warm chunks of the window", fetched-fetchedBefore)
	}
	if discarded-discardedBefore != 12 {
		t.Errorf("discarded %d bytes, want the 3 chunks no read touched", discarded-discardedBefore)
	}
}
