package ReaderResource

import (
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

type ReaderResourceReader struct {
	resource *ReaderResource
}

func (m *ReaderResource) Open() (io.ReadSeekCloser, error) {
	return m.reader, nil
}

func (r *ReaderResource) Size() (size int64, err error) {
	size, err = r.reader.Seek(-1, io.SeekEnd)
	if err != nil {
		return
	}
	size++
	return
}
