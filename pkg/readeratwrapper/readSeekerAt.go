package readeratwrapper

import (
	"fmt"
	"io"
	"sync"
)

// Taken from https://stackoverflow.com/a/40206454

type ReadSeekerAt struct {
	mu               sync.Mutex
	underlyingReader io.ReadSeeker
}

// Creates a new ReadSeekerAt from a ReadSeeker; Limitation: Supports only one ReadAt at a time (enforced with mutex)
func NewReadSeekerAt(r io.ReadSeeker) io.ReaderAt {
	return &ReadSeekerAt{underlyingReader: r}
}

// ReadAt uses the ReadSeeker's Seek method to navigate and read data at a given offset.
func (r *ReadSeekerAt) ReadAt(p []byte, off int64) (n int, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	_, err = r.underlyingReader.Seek(off, io.SeekStart)
	if err != nil {
		return 0, fmt.Errorf("failed seeking: %w", err)
	}

	n, err = r.underlyingReader.Read(p)

	return n, err
}
