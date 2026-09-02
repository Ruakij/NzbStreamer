package readeratwrapper

import (
	"errors"
	"fmt"
	"io"
	"slices"
	"sync"
)

var ErrPoolClosed = errors.New("reader pool is closed")

// PooledReadSeekerAt serves positional reads from a set of independent
// ReadSeekers, one per read in flight, so concurrent reads do not queue behind
// each other the way they do on a single seeking reader.
//
// A reader is picked by how close it already sits to the offset wanted, which
// keeps a sequential stream on the same one and whatever readahead that stream
// drives intact. Readers are opened on demand up to max and kept until Close.
type PooledReadSeekerAt struct {
	open func() (io.ReadSeekCloser, error)
	max  int

	mutex  sync.Mutex
	ready  *sync.Cond
	idle   []*pooledReader
	live   int
	closed bool
}

type pooledReader struct {
	reader io.ReadSeekCloser
	// Where the reader sits, or -1 when a failure left it unknown
	position int64
}

func NewPooledReadSeekerAt(open func() (io.ReadSeekCloser, error), max int) *PooledReadSeekerAt {
	if max < 1 {
		max = 1
	}

	p := &PooledReadSeekerAt{open: open, max: max}
	p.ready = sync.NewCond(&p.mutex)

	return p
}

// ReadAt fills p, which is what io.ReaderAt promises and what a presenter
// handing the bytes to a kernel needs: a short read there reads as the end of
// the file.
func (p *PooledReadSeekerAt) ReadAt(b []byte, off int64) (int, error) {
	if len(b) == 0 {
		return 0, nil
	}

	reader, err := p.acquire(off)
	if err != nil {
		return 0, err
	}
	defer p.release(reader)

	if reader.position != off {
		if _, err := reader.reader.Seek(off, io.SeekStart); err != nil {
			reader.position = -1
			return 0, fmt.Errorf("failed seeking to %d: %w", off, err)
		}
		reader.position = off
	}

	n, err := io.ReadFull(reader.reader, b)
	reader.position += int64(n)

	if errors.Is(err, io.ErrUnexpectedEOF) {
		err = io.EOF
	}
	if err != nil && !errors.Is(err, io.EOF) {
		reader.position = -1
		return n, fmt.Errorf("failed reading at %d: %w", off, err)
	}

	return n, err
}

// acquire takes the idle reader closest to off, opens another while the pool has
// room for one, and otherwise waits for one to come back.
func (p *PooledReadSeekerAt) acquire(off int64) (*pooledReader, error) {
	p.mutex.Lock()

	for {
		if p.closed {
			p.mutex.Unlock()
			return nil, ErrPoolClosed
		}

		if i := nearest(p.idle, off); i >= 0 {
			reader := p.idle[i]
			p.idle = slices.Delete(p.idle, i, i+1)
			p.mutex.Unlock()

			return reader, nil
		}

		if p.live < p.max {
			p.live++
			p.mutex.Unlock()

			reader, err := p.open()
			if err != nil {
				p.mutex.Lock()
				p.live--
				p.mutex.Unlock()
				p.ready.Signal()

				return nil, fmt.Errorf("failed opening pooled reader: %w", err)
			}

			return &pooledReader{reader: reader, position: -1}, nil
		}

		p.ready.Wait()
	}
}

func (p *PooledReadSeekerAt) release(reader *pooledReader) {
	p.mutex.Lock()
	if p.closed {
		p.live--
		p.mutex.Unlock()
		//nolint:errcheck // Nothing to do with a failure of a reader we are done with
		reader.reader.Close()

		return
	}
	p.idle = append(p.idle, reader)
	p.mutex.Unlock()

	p.ready.Signal()
}

// nearest picks the idle reader closest to off, preferring one already sitting
// there, since that read needs no seek at all.
func nearest(idle []*pooledReader, off int64) int {
	best := -1
	var bestDistance int64

	for i, reader := range idle {
		distance := off - reader.position
		if distance < 0 {
			distance = -distance
		}
		if best < 0 || distance < bestDistance {
			best, bestDistance = i, distance
		}
	}

	return best
}

// Close releases every reader. Ones still serving a read are closed as they come
// back, so a read in flight finishes rather than reading from a closed handle.
func (p *PooledReadSeekerAt) Close() error {
	p.mutex.Lock()
	p.closed = true
	idle := p.idle
	p.idle = nil
	p.live -= len(idle)
	p.mutex.Unlock()

	p.ready.Broadcast()

	errs := make([]error, 0, len(idle))
	for _, reader := range idle {
		errs = append(errs, reader.reader.Close())
	}

	//nolint:wrapcheck // Nothing to add to a close failure of the underlying readers
	return errors.Join(errs...)
}
