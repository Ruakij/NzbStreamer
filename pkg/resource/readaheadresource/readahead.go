// Package readaheadresource wraps a resource to issue reads for future
// positions before they are asked for.
package readaheadresource

import (
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"sync/atomic"

	"git.ruekov.eu/ruakij/nzbStreamer/pkg/resource"
)

// Resource keeps a bounded window of chunks warm ahead of where a reader is
// working. Both Read and ReadAt are served from it, since fuse only ever issues
// positional reads and webdav only ever a stream.
type Resource struct {
	underlying resource.ReadSeekCloseableResource
	minSize    int
	maxSize    int
	chunk      int
	rampSpeed  float64
}

func New(underlying resource.ReadSeekCloseableResource, minSize, maxSize, chunk int, rampSpeed float64) *Resource {
	if chunk > maxSize {
		chunk = maxSize
	}
	// Below 1 the window would shrink where it is meant to grow, and 0 would
	// divide by zero on the way back down
	if rampSpeed < 1 {
		rampSpeed = 1
	}
	return &Resource{underlying: underlying, minSize: min(minSize, maxSize), maxSize: maxSize, chunk: chunk, rampSpeed: rampSpeed}
}

func (r *Resource) SizeHint() (int64, error) { return r.underlying.SizeHint() }

func (r *Resource) Size() (int64, error) {
	sized, ok := r.underlying.(resource.Sized)
	if !ok {
		return 0, resource.ErrSizeNotExact
	}
	return sized.Size()
}

// Open wraps only what the window can work on: a decoder stream has no
// addressable offsets and reaches the caller as it is.
func (r *Resource) Open() (io.ReadSeekCloser, error) {
	underlying, err := r.underlying.Open()
	if err != nil {
		return nil, fmt.Errorf("open underlying resource: %w", err)
	}
	readerAt, ok := underlying.(io.ReaderAt)
	if !ok {
		return underlying, nil
	}

	window := max(1, r.maxSize/r.chunk)

	return &reader{
		underlying: underlying,
		readerAt:   readerAt,
		chunkSize:  int64(r.chunk),
		window:     window,
		warmMin:    min(window, max(1, r.minSize/r.chunk)),
		warm:       min(window, max(1, r.minSize/r.chunk)),
		rampSpeed:  r.rampSpeed,
		chunks:     make(map[int64]*chunk),
		eof:        -1,
	}, nil
}

// chunk is one aligned read of the underlying resource, shared by every request
// that lands in it. data shorter than the chunk size is the end of the file.
//
// buf is the whole allocation data is cut from, returned to the reader's pool
// once nothing holds the chunk any more: refs counts the window slot it sits in
// plus every read copying out of it, since a read copies without the lock and
// eviction must not hand its bytes to the next fetch underneath it.
type chunk struct {
	done chan struct{}
	data []byte
	err  error
	buf  []byte
	refs atomic.Int32
}

type reader struct {
	underlying io.ReadSeekCloser
	readerAt   io.ReaderAt
	chunkSize  int64
	// window is the most chunks held warm, the one being read included, warmMin
	// the fewest and rampSpeed what warm moves by between them
	window    int
	warmMin   int
	rampSpeed float64

	mu sync.Mutex
	// warm is how many of the window the reader has earned
	warm     int
	fetches  sync.WaitGroup
	chunks   map[int64]*chunk
	anchor   int64
	position int64
	// eof is where the file ended, or -1 while that is unknown
	eof    int64
	closed bool
}

func (r *reader) Read(p []byte) (int, error) {
	r.mu.Lock()
	position := r.position
	r.mu.Unlock()

	n, err := r.ReadAt(p, position)
	// A stream reports the end on the read that has nothing left, not together
	// with bytes; rardecode keeps the error of a filled read and carries it into
	// the next volume
	if n > 0 && errors.Is(err, io.EOF) {
		err = nil
	}

	r.mu.Lock()
	r.position = position + int64(n)
	r.mu.Unlock()

	return n, err
}

func (r *reader) ReadAt(p []byte, off int64) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if off < 0 {
		return 0, resource.ErrInvalidSeek
	}

	read := 0
	for read < len(p) {
		offset := off + int64(read)
		base := offset - offset%r.chunkSize

		c, err := r.chunkAt(base)
		if err != nil {
			return read, err
		}
		<-c.done
		if c.err != nil {
			r.release(c)
			return read, c.err
		}

		inner := offset - base
		if inner >= int64(len(c.data)) {
			r.release(c)
			return read, io.EOF
		}
		read += copy(p[read:], c.data[inner:])
		r.release(c)
	}

	return read, nil
}

// chunkAt returns the chunk holding base and warms what the reader has earned
// of the window ahead of it, which reaches the whole of it after log2(window).
// A read inside the window says nothing about where the reader went, since fuse
// delivers readahead out of order; one a whole window away moves the anchor and
// costs what it earned.
func (r *reader) chunkAt(base int64) (*chunk, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.closed {
		return nil, os.ErrClosed
	}
	switch {
	case base > r.anchor+int64(r.window)*r.chunkSize:
		r.anchor = base
		r.warm = r.cooled()
	case base > r.anchor:
		r.anchor = base
		r.warm = r.warmed()
	case base < r.anchor-int64(r.window)*r.chunkSize:
		// A whole window behind the head is a seek, not the out-of-order readahead
		// fuse delivers: that is bounded by the kernel's own window, which is
		// smaller. lseek never reaches a filesystem, so this is all a player
		// scrubbing backwards gives us
		r.anchor = base
		r.warm = r.cooled()
	}

	c := r.fetchLocked(base)
	c.refs.Add(1) // held by the caller until it has copied out of it
	for i := 1; i < r.warm; i++ {
		r.fetchLocked(r.anchor + int64(i)*r.chunkSize)
	}
	for offset, evicted := range r.chunks {
		if offset < r.anchor-r.chunkSize || offset > r.anchor+int64(r.window)*r.chunkSize {
			delete(r.chunks, offset)
			r.release(evicted)
		}
	}

	return c, nil
}

// warmed and cooled are the window after the reader ran on and after it jumped:
// what it earns and what that costs. A rampSpeed of 1 or less earns nothing, so
// the window stays the one it opened on. Both require mu.
func (r *reader) warmed() int {
	if r.rampSpeed <= 1 {
		return r.warm
	}
	return min(r.window, max(r.warm+1, int(float64(r.warm)*r.rampSpeed)))
}

func (r *reader) cooled() int { return max(r.warmMin, int(float64(r.warm)/r.rampSpeed)) }

// Requires mu.
func (r *reader) fetchLocked(offset int64) *chunk {
	if c, ok := r.chunks[offset]; ok {
		return c
	}
	if r.eof >= 0 && offset >= r.eof {
		return &chunk{done: closed}
	}

	c := &chunk{done: make(chan struct{}), buf: r.buffer()}
	c.refs.Store(2) // the window slot it now sits in, and the fetch writing into it
	r.chunks[offset] = c
	r.fetches.Add(1)

	go func() {
		defer r.fetches.Done()
		// Held until the write is over
		defer r.release(c)

		buf := c.buf
		n, err := r.readerAt.ReadAt(buf, offset)
		if errors.Is(err, io.EOF) {
			err = nil
		}
		c.data, c.err = buf[:n], err
		close(c.done)

		if int64(n) < r.chunkSize && err == nil {
			r.mu.Lock()
			r.eof = offset + int64(n)
			r.mu.Unlock()
		}
	}()

	return c
}

// bufs holds chunk-sized buffers a fetch takes instead of allocating: the
// runtime zeroes every large allocation and a chunk is overwritten whole, which
// measured as a sixth of the cpu of a fast read. It is shared by every open
// file, since the chunk size is one setting and a file that closes hands its
// window to the next one that opens.
var bufs sync.Pool

// release drops one hold on a chunk and pools its buffer once the last one goes.
func (r *reader) release(c *chunk) {
	if c.buf == nil {
		return
	}
	if c.refs.Add(-1) == 0 {
		bufs.Put(&c.buf)
	}
}

func (r *reader) buffer() []byte {
	if b, ok := bufs.Get().(*[]byte); ok && int64(cap(*b)) >= r.chunkSize {
		return (*b)[:r.chunkSize]
	}

	return make([]byte, r.chunkSize)
}

var closed = func() chan struct{} {
	c := make(chan struct{})
	close(c)
	return c
}()

func (r *reader) Seek(offset int64, whence int) (int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	position := offset
	switch whence {
	case io.SeekStart:
	case io.SeekCurrent:
		position += r.position
	case io.SeekEnd:
		size, err := r.underlying.Seek(0, io.SeekEnd)
		if err != nil {
			return 0, fmt.Errorf("seek underlying reader to end: %w", err)
		}
		position += size
	default:
		return 0, resource.ErrInvalidSeek
	}
	if position < 0 {
		return 0, resource.ErrInvalidSeek
	}

	// A seek says where the stream goes next, which is the one thing that lets the
	// anchor move back: fuse never seeks, it only issues positional reads, and
	// those carry no such promise. It costs the window what a jump costs it, since
	// the reader leaves behind everything it warmed
	if base := position - position%r.chunkSize; base < r.anchor {
		r.anchor = base
		r.warm = r.cooled()
	}

	r.position = position
	return position, nil
}

func (r *reader) Close() error {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return nil
	}
	r.closed = true
	r.mu.Unlock()

	// A fetch reads the underlying reader, so nothing may close it underneath one
	r.fetches.Wait()

	// The window goes back to the pool rather than to the garbage collector; a
	// read still copying out of a chunk holds its own reference
	r.mu.Lock()
	for offset, c := range r.chunks {
		delete(r.chunks, offset)
		r.release(c)
	}
	r.mu.Unlock()

	return r.underlying.Close()
}

var (
	_ resource.ReadSeekCloseableResource = (*Resource)(nil)
	_ resource.Sized                     = (*Resource)(nil)
	_ io.ReadSeekCloser                  = (*reader)(nil)
	_ io.ReaderAt                        = (*reader)(nil)
)
