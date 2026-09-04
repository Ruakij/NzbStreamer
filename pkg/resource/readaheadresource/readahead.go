package readaheadresource

import (
	"errors"
	"fmt"
	"io"
	"sync"

	"git.ruekov.eu/ruakij/nzbStreamer/pkg/resource"
)

// Resource reads a bounded window ahead through positional reads. The ordinary
// resource API remains unchanged for callers and lower layers.
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

func (r *Resource) Open() (io.ReadSeekCloser, error) {
	underlying, err := r.underlying.Open()
	if err != nil {
		return nil, fmt.Errorf("open underlying resource: %w", err)
	}
	readerAt, ok := underlying.(io.ReaderAt)
	if !ok {
		return underlying, nil
	}

	reader := &reader{
		underlying: underlying,
		readerAt:   readerAt,
		chunk:      r.chunk,
		window:     max(1, r.size/r.chunk),
		results:    make(map[int64]result),
	}
	reader.ready = sync.NewCond(&reader.mu)
	return reader, nil
}

type result struct {
	data []byte
	err  error
}

type reader struct {
	underlying io.ReadSeekCloser
	readerAt   io.ReaderAt
	chunk      int
	window     int

	mu         sync.Mutex
	ready      *sync.Cond
	position   int64
	next       int64
	generation uint64
	active     int
	results    map[int64]result
	current    []byte
	currentEnd int64
	currentErr error
	eofAt      int64
	closed     bool
}

func (r *reader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.fillLocked()
	for len(r.current) == 0 && r.currentErr == nil && !r.closed {
		if result, ok := r.results[r.position]; ok {
			delete(r.results, r.position)
			r.current, r.currentErr = result.data, result.err
			r.currentEnd = r.position + int64(len(result.data))
			break
		}
		r.ready.Wait()
	}
	if len(r.current) == 0 {
		if r.currentErr != nil {
			return 0, r.currentErr
		}
		return 0, io.ErrClosedPipe
	}

	n := copy(p, r.current)
	r.current = r.current[n:]
	r.position += int64(n)
	if len(r.current) == 0 && errors.Is(r.currentErr, io.EOF) {
		return n, io.EOF
	}
	if len(r.current) == 0 {
		r.position = r.currentEnd
		r.currentErr = nil
	}
	r.fillLocked()
	return n, nil
}

func (r *reader) fillLocked() {
	for !r.closed && r.active < r.window && (r.eofAt == 0 || r.next < r.eofAt) {
		offset := r.next
		r.next += int64(r.chunk)
		r.active++
		generation := r.generation
		go func() {
			buf := make([]byte, r.chunk)
			n, err := r.readerAt.ReadAt(buf, offset)
			r.mu.Lock()
			defer r.mu.Unlock()
			r.active--
			if generation == r.generation && !r.closed {
				r.results[offset] = result{data: buf[:n], err: err}
				if errors.Is(err, io.EOF) && (r.eofAt == 0 || offset+int64(n) < r.eofAt) {
					r.eofAt = offset + int64(n)
				}
			}
			if !r.closed {
				r.fillLocked()
			}
			r.ready.Broadcast()
		}()
	}
}

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
			return 0, err
		}
		position += size
	default:
		return 0, resource.ErrInvalidSeek
	}
	if position < 0 {
		return 0, resource.ErrInvalidSeek
	}
	if position == r.position {
		return position, nil
	}

	r.generation++
	r.position, r.next = position, position
	r.results = make(map[int64]result)
	r.current, r.currentErr, r.currentEnd, r.eofAt = nil, nil, position, 0
	r.fillLocked()
	return position, nil
}

func (r *reader) Close() error {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return nil
	}
	r.closed = true
	r.ready.Broadcast()
	for r.active > 0 {
		r.ready.Wait()
	}
	r.mu.Unlock()
	return r.underlying.Close()
}

var _ resource.ReadSeekCloseableResource = (*Resource)(nil)
var _ resource.Sized = (*Resource)(nil)
var _ io.ReadSeekCloser = (*reader)(nil)
