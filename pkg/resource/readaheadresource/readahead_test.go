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
	reader, err := readaheadresource.New(&bytesresource.BytesResource{Content: []byte("0123456789")}, 4, 3).Open()
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
	reader, err := readaheadresource.New(underlying, 16, 4).Open()
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
	opened, err := readaheadresource.New(underlying, 16, 4).Open()
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()

	reader, ok := opened.(io.ReaderAt)
	if !ok {
		t.Fatal("reader is not positional; fuse would serialise it behind a seeking wrapper")
	}

	buf := make([]byte, 4)
	if _, err := reader.ReadAt(buf, 16); err != nil {
		t.Fatal(err)
	}
	if underlying.peak.Load() != 4 {
		t.Fatalf("peak reads = %d, want the whole window", underlying.peak.Load())
	}

	// The three chunks ahead are warm, and one behind the head does not drop
	// them. Counting every read would race the warming the advancing anchor
	// starts, so what is asserted is that no chunk was fetched twice; Close
	// waits for the fetches still in flight.
	for _, off := range []int64{12, 20, 24, 28} {
		if _, err := reader.ReadAt(buf, off); err != nil {
			t.Fatal(err)
		}
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
