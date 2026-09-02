package readeratwrapper_test

import (
	"bytes"
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"testing"

	"git.ruekov.eu/ruakij/nzbStreamer/pkg/readeratwrapper"
)

const poolContent = "HelloWorldAndGoodbye"

// countingReader reports how often it was seeked, which is how the test sees
// whether a stream stayed on a reader of its own.
type countingReader struct {
	*bytes.Reader
	seeks  *atomic.Int64
	closed *atomic.Int64
}

func (c *countingReader) Seek(offset int64, whence int) (int64, error) {
	c.seeks.Add(1)

	//nolint:wrapcheck // The test wants the underlying error unchanged
	return c.Reader.Seek(offset, whence)
}

func (c *countingReader) Close() error {
	c.closed.Add(1)

	return nil
}

type poolCounters struct {
	opens  atomic.Int64
	seeks  atomic.Int64
	closed atomic.Int64
}

func newPool(t *testing.T, max int) (*readeratwrapper.PooledReadSeekerAt, *poolCounters) {
	t.Helper()

	counters := &poolCounters{}
	pool := readeratwrapper.NewPooledReadSeekerAt(func() (io.ReadSeekCloser, error) {
		counters.opens.Add(1)

		return &countingReader{
			Reader: bytes.NewReader([]byte(poolContent)),
			seeks:  &counters.seeks,
			closed: &counters.closed,
		}, nil
	}, max)

	return pool, counters
}

func TestPooledReadAt(t *testing.T) {
	t.Parallel()

	pool, _ := newPool(t, 4)
	defer pool.Close()

	for off := range len(poolContent) {
		for length := 1; off+length <= len(poolContent); length++ {
			buffer := make([]byte, length)

			n, err := pool.ReadAt(buffer, int64(off))
			if err != nil {
				t.Fatalf("ReadAt(%d, %d) = %v, want no error", length, off, err)
			}
			if got, want := string(buffer[:n]), poolContent[off:off+length]; got != want {
				t.Errorf("ReadAt(%d, %d) = %q, want %q", length, off, got, want)
			}
		}
	}
}

// A read running off the end reports what it got and io.EOF, since a short read
// reaching a kernel reads as the end of the file.
func TestPooledReadAtPastEnd(t *testing.T) {
	t.Parallel()

	pool, _ := newPool(t, 4)
	defer pool.Close()

	buffer := make([]byte, 8)
	n, err := pool.ReadAt(buffer, int64(len(poolContent))-3)
	if !errors.Is(err, io.EOF) {
		t.Errorf("ReadAt at end = %v, want io.EOF", err)
	}
	if got, want := string(buffer[:n]), poolContent[len(poolContent)-3:]; got != want {
		t.Errorf("ReadAt at end = %q, want %q", got, want)
	}

	if _, err := pool.ReadAt(buffer, int64(len(poolContent))); !errors.Is(err, io.EOF) {
		t.Errorf("ReadAt past end = %v, want io.EOF", err)
	}
}

// A stream walking forwards should find its own reader already sitting there,
// so only the first read of it costs a seek.
func TestPooledReadAtSequentialDoesNotSeek(t *testing.T) {
	t.Parallel()

	pool, counters := newPool(t, 4)
	defer pool.Close()

	buffer := make([]byte, 4)
	for off := 0; off+len(buffer) <= len(poolContent); off += len(buffer) {
		if _, err := pool.ReadAt(buffer, int64(off)); err != nil {
			t.Fatalf("ReadAt(4, %d) = %v, want no error", off, err)
		}
	}

	if got := counters.seeks.Load(); got != 1 {
		t.Errorf("sequential reads seeked %d times, want 1 - the first placement", got)
	}
	if got := counters.opens.Load(); got != 1 {
		t.Errorf("sequential reads opened %d readers, want 1", got)
	}
}

// Concurrent reads must not move a position out from under each other, and must
// open readers of their own rather than queue. Run with -race.
func TestPooledReadAtConcurrent(t *testing.T) {
	t.Parallel()

	const max = 4

	pool, counters := newPool(t, max)
	defer pool.Close()

	var group sync.WaitGroup
	for off := range len(poolContent) {
		group.Add(1)
		go func() {
			defer group.Done()

			for range 20 {
				length := min(4, len(poolContent)-off)
				buffer := make([]byte, length)

				n, err := pool.ReadAt(buffer, int64(off))
				if err != nil {
					t.Errorf("ReadAt(%d, %d) = %v, want no error", length, off, err)

					return
				}

				want := poolContent[off : off+length]
				if !bytes.Equal(buffer[:n], []byte(want)) {
					t.Errorf("ReadAt(%d, %d) = %q, want %q", length, off, buffer[:n], want)

					return
				}
			}
		}()
	}
	group.Wait()

	if got := counters.opens.Load(); got > max {
		t.Errorf("opened %d readers, want at most %d", got, max)
	}
}

func TestPooledClose(t *testing.T) {
	t.Parallel()

	pool, counters := newPool(t, 4)

	if _, err := pool.ReadAt(make([]byte, 4), 0); err != nil {
		t.Fatalf("ReadAt = %v, want no error", err)
	}
	if err := pool.Close(); err != nil {
		t.Fatalf("Close() = %v, want no error", err)
	}

	if got := counters.closed.Load(); got != counters.opens.Load() {
		t.Errorf("closed %d of %d readers", got, counters.opens.Load())
	}
	if _, err := pool.ReadAt(make([]byte, 4), 0); !errors.Is(err, readeratwrapper.ErrPoolClosed) {
		t.Errorf("ReadAt after Close = %v, want ErrPoolClosed", err)
	}
}
