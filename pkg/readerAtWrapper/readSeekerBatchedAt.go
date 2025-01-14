package readerAtWrapper

import (
	"io"
	"sort"
	"sync"
	"time"
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

// ReadSeekerBatchedAt wraps a io.ReadSeeker to support io.ReaderAt interface.
//
// ReadAt calls are bached in <interval> and executed in parallel, unless ReadAt offset perfectly matches previous ReadAt call, then the interval is reset;
// Its best to keep <interval> as small as possible, but high enough for sequencial reading to happen as best as possible.
type ReadSeekerBatchedAt struct {
	sync.Mutex
	underlyingReader io.ReadSeeker
	interval         time.Duration
	timer            *time.Timer
	requests         chan readRequest
	lastOffset       int64
}

func NewReadSeekerBatchedAt(r io.ReadSeeker, interval time.Duration) *ReadSeekerBatchedAt {
	rsa := &ReadSeekerBatchedAt{
		underlyingReader: r,
		interval:         interval,
		timer:            time.NewTimer(0),
		requests:         make(chan readRequest, 5), // buffered channel of requests
	}

	go rsa.batchReadLoop()
	return rsa
}

func (rsa *ReadSeekerBatchedAt) ReadAt(p []byte, off int64) (n int, err error) {
	req := readRequest{p: p, off: off, done: make(chan readResult, 1)}
	rsa.requests <- req // send request to the channel
	res := <-req.done   // wait for the result
	return res.n, res.err
}

func (rsa *ReadSeekerBatchedAt) performSeekAndRead(req *readRequest) {
	rsa.Lock()
	defer rsa.Unlock()

	// Seek to the requested offset
	_, err := rsa.underlyingReader.Seek(req.off, io.SeekStart)
	if err != nil {
		req.done <- readResult{n: 0, err: err}
		return
	}

	// Read the data into the request's buffer
	n, err := rsa.underlyingReader.Read(req.p)
	req.done <- readResult{n: n, err: err}

	// Update the last offset
	rsa.lastOffset = req.off + int64(n)
}

func (rsa *ReadSeekerBatchedAt) batchReadLoop() {
	var batch []readRequest
	rsa.timer.Reset(rsa.interval)

	insertInOrder := func(req readRequest) {
		pos := sort.Search(len(batch), func(i int) bool {
			return batch[i].off > req.off
		})
		batch = append(batch, req)       // append at the end
		copy(batch[pos+1:], batch[pos:]) // shift elements to the right
		batch[pos] = req                 // insert the new element
	}

	processSequentially := func() {
		for len(batch) > 0 && batch[0].off == rsa.lastOffset {
			rsa.performSeekAndRead(&batch[0])
			batch = batch[1:] // remove the processed element
		}
	}

	for {
		select {
		case req := <-rsa.requests:
			insertInOrder(req)

			// Process any batch that's ready due to new insertion
			processSequentially()

			// Only reset the timer if there's more to process
			if len(batch) > 0 {
				rsa.timer.Reset(rsa.interval)
			}
		case <-rsa.timer.C:
			if len(batch) > 0 {
				// Process whatever is in the batch as a catch-all
				for len(batch) > 0 {
					rsa.performSeekAndRead(&batch[0])
					batch = batch[1:] // remove the processed element
				}
			}
		}
	}
}
