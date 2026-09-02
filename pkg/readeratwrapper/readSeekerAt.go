package readeratwrapper

import (
	"errors"
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

// Positional takes a reader as an io.ReaderAt, wrapping only one that cannot
// answer positional reads itself. Addressable data answers them directly and its
// readers then run concurrently; a decoder stream runs forwards only, and
// serialising it is the constraint rather than an artefact.
func Positional(r io.ReadSeeker) io.ReaderAt {
	if readerAt, ok := r.(io.ReaderAt); ok {
		return readerAt
	}

	return NewReadSeekerAt(r)
}

// ReadAt uses the ReadSeeker's Seek method to navigate and read data at a given
// offset. It fills p, which is what io.ReaderAt promises and what a presenter
// handing the bytes to a kernel needs: a short read there reads as the end of
// the file.
func (r *ReadSeekerAt) ReadAt(p []byte, off int64) (n int, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	_, err = r.underlyingReader.Seek(off, io.SeekStart)
	if err != nil {
		return 0, fmt.Errorf("failed seeking: %w", err)
	}

	n, err = io.ReadFull(r.underlyingReader, p)
	if errors.Is(err, io.ErrUnexpectedEOF) {
		err = io.EOF
	}

	return n, err
}
