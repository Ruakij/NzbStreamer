package readerAtWrapper

import (
	"errors"
	"io"
	"sync"
)

// Based off https://stackoverflow.com/a/40206454

type ReaderAt struct {
	sync.Mutex
	underlyingReader io.Reader
	index            int64
}

// Creates a new ReaderAt from a reader; Limitation: Supports only sequential read & only one ReadAt at a time (enforced with mutex)
func NewReaderAt(r io.Reader) io.ReaderAt {
	return &ReaderAt{underlyingReader: r}
}

// Because ReadAt is based of a Reader, it must be read sequentially, otherwise error occurs
func (r *ReaderAt) ReadAt(p []byte, off int64) (n int, err error) {
	r.Lock()
	defer r.Unlock()

	if off < r.index {
		return 0, errors.New("invalid offset")
	}
	diff := off - r.index
	written, err := io.CopyN(io.Discard, r.underlyingReader, diff)
	r.index += written
	if err != nil {
		return 0, err
	}

	n, err = r.underlyingReader.Read(p)
	r.index += int64(n)
	return
}
