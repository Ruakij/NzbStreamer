// Package readaheadresource wraps a resource to issue reads for future
// positions before they are asked for.
package readaheadresource

import (
	"errors"
	"fmt"
	"io"
	"os"
	"sync"

	"git.ruekov.eu/ruakij/nzbStreamer/pkg/resource"
)

// Resource keeps a bounded window of chunks warm ahead of where a reader is
// working. Both Read and ReadAt are served from it, since fuse only ever issues
// positional reads and webdav only ever a stream.
type Resource struct {
	underlying resource.ReadSeekCloseableResource
	size       int
	chunk      int
}

func New(underlying resource.ReadSeekCloseableResource, size, chunk int) *Resource {
	if chunk > size {
		chunk = size
	}
	return &Resource{underlying: underlying, size: size, chunk: chunk}
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

	return &reader{
		underlying: underlying,
		readerAt:   readerAt,
		chunkSize:  int64(r.chunk),
		window:     max(1, r.size/r.chunk),
		chunks:     make(map[int64]*chunk),
		eof:        -1,
	}, nil
}

// chunk is one aligned read of the underlying resource, shared by every request
// that lands in it. data shorter than the chunk size is the end of the file.
type chunk struct {
	done chan struct{}
	data []byte
	err  error
}

type reader struct {
	underlying io.ReadSeekCloser
	readerAt   io.ReaderAt
	chunkSize  int64
	// window is how many chunks are held warm, the one being read included
	window int

	mu       sync.Mutex
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
			return read, c.err
		}

		inner := offset - base
		if inner >= int64(len(c.data)) {
			return read, io.EOF
		}
		read += copy(p[read:], c.data[inner:])
	}

	return read, nil
}

// chunkAt returns the chunk holding base and warms the window ahead of it. The
// anchor only ever advances: fuse delivers readahead out of order and a read
// landing behind the head says nothing about what follows.
func (r *reader) chunkAt(base int64) (*chunk, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.closed {
		return nil, os.ErrClosed
	}
	if base > r.anchor {
		r.anchor = base
	}

	c := r.fetchLocked(base)
	for i := 1; i < r.window; i++ {
		r.fetchLocked(r.anchor + int64(i)*r.chunkSize)
	}
	for offset := range r.chunks {
		if offset < r.anchor-r.chunkSize {
			delete(r.chunks, offset)
		}
	}

	return c, nil
}

// Requires mu.
func (r *reader) fetchLocked(offset int64) *chunk {
	if c, ok := r.chunks[offset]; ok {
		return c
	}
	if r.eof >= 0 && offset >= r.eof {
		return &chunk{done: closed}
	}

	c := &chunk{done: make(chan struct{})}
	r.chunks[offset] = c
	r.fetches.Add(1)

	go func() {
		defer r.fetches.Done()

		buf := make([]byte, r.chunkSize)
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

	return r.underlying.Close()
}

var (
	_ resource.ReadSeekCloseableResource = (*Resource)(nil)
	_ resource.Sized                     = (*Resource)(nil)
	_ io.ReadSeekCloser                  = (*reader)(nil)
	_ io.ReaderAt                        = (*reader)(nil)
)
