package readerresource

import (
	"fmt"
	"io"
)

// ReaderResource is a utility type that allows using a reader as resource
type ReaderResource struct {
	reader io.ReadSeekCloser
}

func NewReaderResource(reader io.ReadSeekCloser) *ReaderResource {
	return &ReaderResource{
		reader: reader,
	}
}

type ReaderResourceReader struct{}

func (r *ReaderResource) Open() (io.ReadSeekCloser, error) {
	return r.reader, nil
}

func (r *ReaderResource) Size() (int64, error) {
	size, err := r.reader.Seek(0, io.SeekEnd)
	if err != nil {
		return size, fmt.Errorf("failed getting size from underlying reader: %w", err)
	}

	return size, nil
}
