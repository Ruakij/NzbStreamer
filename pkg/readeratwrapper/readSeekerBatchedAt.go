package readeratwrapper

import (
	"errors"
	"fmt"
	"io"
	"sort"
	"sync"
	"time"
)

var (
	// ErrClosed is returned once Close has been called.
	ErrClosed = errors.New("reader is closed")
	// ErrInvalidOffset is returned for a read at a negative offset.
	ErrInvalidOffset = errors.New("negative read offset")
)

type readRequest struct {
	p    []byte
	off  int64
	done chan readResult
}

type readResult struct {
	n   int
	err error
}

// ReadSeekerBatchedAt turns a reopenable stream into an io.ReaderAt.
//
// A request the cursor can serve right away runs immediately; a request the
// stream has moved past opens a short window so the requests beside it run
// sorted by offset instead of seeking back and forth. A sequential stream never
// waits.
//
// The underlying reader is only ever touched by a single worker, which a
// decoder stream requires.
type ReadSeekerBatchedAt struct {
	underlying io.ReadSeekCloser
	delay      time.Duration
	requests   chan readRequest
	quit       chan struct{}
	done       chan struct{}

	closeOnce sync.Once
	closeErr  error
}

// NewReadSeekerBatchedAt wraps r so positional reads against it are batched;
// maxBatchDelay bounds an out-of-order request's wait.
func NewReadSeekerBatchedAt(r io.ReadSeekCloser, maxBatchDelay time.Duration) *ReadSeekerBatchedAt {
	if maxBatchDelay < 0 {
		maxBatchDelay = 0
	}

	b := &ReadSeekerBatchedAt{
		underlying: r,
		delay:      maxBatchDelay,
		requests:   make(chan readRequest, 32),
		quit:       make(chan struct{}),
		done:       make(chan struct{}),
	}

	go b.worker()

	return b
}

func (b *ReadSeekerBatchedAt) ReadAt(p []byte, off int64) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if off < 0 {
		return 0, ErrInvalidOffset
	}

	req := readRequest{p: p, off: off, done: make(chan readResult, 1)}
	select {
	case b.requests <- req:
	case <-b.quit:
		return 0, ErrClosed
	}

	// done closes when the worker shuts down; a result delivered by then wins.
	select {
	case res := <-req.done:
		return res.n, res.err
	case <-b.done:
		select {
		case res := <-req.done:
			return res.n, res.err
		default:
			return 0, ErrClosed
		}
	}
}

// Close stops the worker, closes the underlying reader, unblocks queued reads
// with ErrClosed, and is safe to call more than once.
func (b *ReadSeekerBatchedAt) Close() error {
	b.closeOnce.Do(func() { close(b.quit) })
	<-b.done

	return b.closeErr
}

// worker is the only goroutine touching the underlying reader.
func (b *ReadSeekerBatchedAt) worker() {
	defer close(b.done)

	var pending []readRequest
	cursor := int64(0) // -1 means the position is unknown

	insert := func(req readRequest) {
		// First greater offset keeps equal offsets in arrival order (stable).
		i := sort.Search(len(pending), func(i int) bool { return pending[i].off > req.off })
		pending = append(pending, readRequest{})
		copy(pending[i+1:], pending[i:])
		pending[i] = req
	}

	// run executes one request, seeking only when the stream sits elsewhere.
	run := func(req readRequest) {
		if req.off != cursor {
			position, err := b.underlying.Seek(req.off, io.SeekStart)
			if err != nil {
				cursor = -1
				req.done <- readResult{0, fmt.Errorf("failed seeking to %d: %w", req.off, err)}
				return
			}
			cursor = position
		}

		n, err := io.ReadFull(b.underlying, req.p)
		if errors.Is(err, io.ErrUnexpectedEOF) {
			err = io.EOF
		}
		if err != nil && !errors.Is(err, io.EOF) {
			cursor = -1
		} else {
			cursor += int64(n)
		}

		req.done <- readResult{n, err}
	}

	// drain runs requests the cursor can serve right now, skipping the wait.
	drain := func() {
		for len(pending) > 0 {
			req := pending[0]
			if cursor >= 0 && req.off != cursor {
				break
			}
			pending = pending[1:]
			run(req)
		}
	}

	// arm waits for company only on an out-of-order request.
	timer := time.NewTimer(0)
	if !timer.Stop() {
		<-timer.C
	}
	armed := false
	arm := func() {
		if !armed && len(pending) > 0 && cursor >= 0 && pending[0].off != cursor {
			timer.Reset(b.delay)
			armed = true
		}
	}

	for {
		// The worker stops servicing once Close was called.
		select {
		case <-b.quit:
			goto shutdown
		default:
		}

		select {
		case req := <-b.requests:
			// A request claimed just as Close fires is rejected, not run.
			select {
			case <-b.quit:
				req.done <- readResult{0, ErrClosed}
				goto shutdown
			default:
			}
			insert(req)
			drain()
			arm()
		case <-timer.C:
			armed = false
			// Run the out-of-order batch, sorted by offset.
			for len(pending) > 0 {
				req := pending[0]
				pending = pending[1:]
				run(req)
			}
			arm()
		case <-b.quit:
			goto shutdown
		}
	}

shutdown:
	reject := func(req readRequest) { req.done <- readResult{0, ErrClosed} }
	for {
		select {
		case req := <-b.requests:
			reject(req)
		default:
			goto closing
		}
	}
closing:
	for _, req := range pending {
		reject(req)
	}
	b.closeErr = b.underlying.Close()
}
