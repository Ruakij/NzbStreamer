package readeratwrapper

import (
	"errors"
	"fmt"
	"io"
	"sync"
)

// Based off https://stackoverflow.com/a/40206454

type ReaderAt struct {
	mu               sync.Mutex
	underlyingReader io.Reader
	index            int64
}

// Creates a new ReaderAt from a reader; Limitation: Supports only sequential read & only one ReadAt at a time (enforced with mutex)
func NewReaderAt(r io.Reader) io.ReaderAt {
	return &ReaderAt{underlyingReader: r}
}

var ErrSeekInvalidOffset = errors.New("invalid offset")

// Because ReadAt is based of a Reader, it must be read sequentially, otherwise error occurs
func (r *ReaderAt) ReadAt(p []byte, off int64) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if off < r.index {
		return 0, ErrSeekInvalidOffset
	}
	diff := off - r.index
	written, err := io.CopyN(io.Discard, r.underlyingReader, diff)
	r.index += written
	if err != nil {
		return 0, fmt.Errorf("failed copying: %w", err)
	}

	n, err := r.underlyingReader.Read(p)
	r.index += int64(n)

	return n, err
}
