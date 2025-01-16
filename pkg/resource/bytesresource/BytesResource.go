package bytesresource

import (
	"io"

	"git.ruekov.eu/ruakij/nzbStreamer/pkg/resource"
)

// BytesResource is a utility type that allows using a byte-slice resource.
type BytesResource struct {
	Content []byte
}

type BytesResourceReader struct {
	resource *BytesResource
	index    int64
}

func (r *BytesResource) Open() (io.ReadSeekCloser, error) {
	return &BytesResourceReader{
		resource: r,
		index:    0,
	}, nil
}

func (r *BytesResource) Size() (int64, error) {
	return int64(len(r.Content)), nil
}

func (r *BytesResourceReader) Close() error {
	r.resource.Content = nil
	r.resource = nil
	return nil
}

func (r *BytesResourceReader) Read(p []byte) (int, error) {
	if r.index >= int64(len(r.resource.Content)) {
		return 0, io.EOF
	}

	if len(p) == 0 {
		return 0, io.EOF
	}

	n := copy(p, r.resource.Content[r.index:])
	r.index += int64(n)

	return n, nil
}

func (r *BytesResourceReader) Seek(offset int64, whence int) (int64, error) {
	var newIndex int64

	resourceSize, err := r.resource.Size()
	if err != nil {
		return 0, err
	}

	switch whence {
	case io.SeekStart:
		newIndex = offset
	case io.SeekCurrent:
		newIndex = r.index + offset
	case io.SeekEnd:
		newIndex = resourceSize + offset
	default:
		return 0, resource.ErrInvalidSeek
	}

	if newIndex < 0 {
		return 0, resource.ErrInvalidSeek
	}

	if newIndex > resourceSize {
		newIndex = resourceSize
	}

	r.index = newIndex
	return r.index, nil
}
