package readerAtWrapper

import (
	"io"
	"sync"
)

// Taken from https://stackoverflow.com/a/40206454

type ReadSeekerAt struct {
	sync.Mutex
	underlyingReader io.ReadSeeker
	index            int64
}

// Creates a new ReadSeekerAt from a ReadSeeker; Limitation: Supports only one ReadAt at a time (enforced with mutex)
func NewReadSeekerAt(r io.ReadSeeker) io.ReaderAt {
	return &ReadSeekerAt{underlyingReader: r}
}

// ReadAt uses the ReadSeeker's Seek method to navigate and read data at a given offset.
func (r *ReadSeekerAt) ReadAt(p []byte, off int64) (n int, err error) {
	r.Lock()
	defer r.Unlock()

	_, err = r.underlyingReader.Seek(off, io.SeekStart)
	if err != nil {
		return 0, err
	}

	n, err = r.underlyingReader.Read(p)
	if err != nil {
		return n, err
	}

	return n, nil
}
