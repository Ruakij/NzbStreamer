package mergerresource

import (
	"errors"
	"fmt"
	"io"

	"git.ruekov.eu/ruakij/nzbStreamer/pkg/resource"
)

// MergerResource is a Resource type which allows combining multiple Resources as if it was one
type MergerResource struct {
	resources []resource.ReadSeekCloseableResource
}

func NewMergerResource(resources []resource.ReadSeekCloseableResource) *MergerResource {
	return &MergerResource{
		resources: resources,
	}
}

type MergerResourceReader struct {
	resource *MergerResource
	readers  []io.ReadSeekCloser
	// Position in data
	index int64
	// Active reader index
	readerIndex int
	// Active reader byte index
	readerByteIndex int64
}

// Open will also eagerly open all underlying Resources
func (r *MergerResource) Open() (io.ReadSeekCloser, error) {
	readers := make([]io.ReadSeekCloser, len(r.resources))
	var err error
	for i, resource := range r.resources {
		readers[i], err = resource.Open()
		if err != nil {
			return nil, fmt.Errorf("failed opening underlying resource %d: %w", i, err)
		}
	}

	return &MergerResourceReader{
		resource: r,
		readers:  readers,
		index:    0,
	}, nil
}

func (r *MergerResource) Size() (int64, error) {
	var totalSize int64
	for i, resource := range r.resources {
		size, err := resource.Size()
		if err != nil {
			return totalSize, fmt.Errorf("failed getting size from underlying resource %d: %w", i, err)
		}

		totalSize += size
	}
	return totalSize, nil
}

func (r *MergerResourceReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}

	var totalRead int
	for r.readerIndex < len(r.readers) {
		readerRead, err := r.readers[r.readerIndex].Read(p[totalRead:])
		totalRead += readerRead
		r.index += int64(readerRead)
		r.readerByteIndex += int64(readerRead)

		// If buffer p is full, return
		if totalRead == len(p) {
			return totalRead, nil
		}

		// Error-check, ignore EOF in the middle
		if err != nil && !errors.Is(err, io.EOF) {
			return 0, fmt.Errorf("failed reading from underlying reader %d: %w", r.readerIndex, err)
		}

		r.readerIndex++
	}

	// All readers exausted, EOF
	return totalRead, io.EOF
}

func (r *MergerResourceReader) Close() error {
	for i, reader := range r.readers {
		err := reader.Close()
		if err != nil {
			return fmt.Errorf("failed closing underlying resource %d: %w", i, err)
		}
	}
	return nil
}

func (r *MergerResourceReader) Seek(offset int64, whence int) (int64, error) {
	var newIndex int64

	switch whence {
	case io.SeekStart:
		newIndex = offset
	case io.SeekCurrent:
		newIndex = r.index + offset
	case io.SeekEnd:
		resourceSize, err := r.resource.Size()
		if err != nil {
			return 0, err
		}

		newIndex = resourceSize - offset
	}

	// Seek to same pos we are at
	if newIndex == r.index {
		return r.index, nil
	}
	// Out of range
	if newIndex < 0 {
		return 0, resource.ErrInvalidSeek
	}

	// Find affected sub-reader
	readerIndex, readerByteIndex, err := r.getReaderIndexAndByteIndexAtByteIndex(newIndex)
	if err != nil {
		if !errors.Is(err, io.ErrUnexpectedEOF) {
			return 0, err
		}
		// Unexpected EOF here just means we couldnt get a reader, because newIndex is behind everything
		// Set reader to after last
		readerIndex = len(r.readers)
	}

	if readerIndex < len(r.readers) {
		// Seek to position in reader
		_, err := r.readers[readerIndex].Seek(readerByteIndex, io.SeekStart)
		if err != nil {
			return 0, fmt.Errorf("failed seeking reader %d to index %d: %w", readerIndex, readerByteIndex, err)
		}

		// Seek to start for all following readers
		for i := readerIndex + 1; i < len(r.readers); i++ {
			_, err = r.readers[i].Seek(0, io.SeekStart)
			if err != nil {
				return 0, fmt.Errorf("failed seeking reader %d to index 0: %w", i, err)
			}
		}
	}

	r.readerIndex = readerIndex
	r.index = newIndex
	return r.index, nil
}

func (r *MergerResourceReader) getReaderIndexAndByteIndexAtByteIndex(index int64) (readerIndex int, readerByteIndex int64, err error) {
	var byteIndex int64 = 0
	for readerIndex, reader := range r.readers {
		size, err := reader.Seek(0, io.SeekEnd)
		if err != nil {
			return 0, 0, fmt.Errorf("failed getting size via SeekEnd from underlying reader %d: %w", readerIndex, err)
		}
		_, err = reader.Seek(r.readerByteIndex, io.SeekStart)
		if err != nil {
			return 0, 0, fmt.Errorf("failed reseeking underlying reader %d: %w", readerIndex, err)
		}

		if index <= byteIndex+size {
			byteIndex = index - byteIndex
			return readerIndex, byteIndex, nil
		}
		byteIndex += size
	}
	// Arriving here means our byteIndex is higher than the actual data
	return 0, 0, io.ErrUnexpectedEOF
}
