package parallelmergerresource

import (
	"errors"
	"fmt"
	"io"
	"sync"

	"git.ruekov.eu/ruakij/nzbStreamer/pkg/resource"
	"golang.org/x/sync/errgroup"
)

// AdaptiveParallelMergerResource is a Resource type which allows combining multiple Resources as if it was one.
// It reads underlying sources in parallel, but requires their size to be known and exact!
type ParallelMergerResource struct {
	resources []resource.ReadSeekCloseableResource
}

func NewParallelMergerResource(resources []resource.ReadSeekCloseableResource) *ParallelMergerResource {
	return &ParallelMergerResource{
		resources: resources,
	}
}

type ParallelMergerResourceReader struct {
	resource *ParallelMergerResource
	readers  []io.ReadSeekCloser
	mu       sync.RWMutex
	// Position in data
	index int64
	// Active reader index
	readerIndex int
	// Active reader byte index
	readerByteIndex int64
}

// Open will also eagerly open all underlying Resources
func (r *ParallelMergerResource) Open() (io.ReadSeekCloser, error) {
	readers := make([]io.ReadSeekCloser, len(r.resources))
	var err error
	for i, resource := range r.resources {
		readers[i], err = resource.Open()
		if err != nil {
			return nil, fmt.Errorf("failed opening underlying resource %d: %w", i, err)
		}
	}

	return &ParallelMergerResourceReader{
		resource:        r,
		readers:         readers,
		index:           0,
		readerIndex:     0,
		readerByteIndex: 0,
	}, nil
}

func (r *ParallelMergerResource) Size() (int64, error) {
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

type readResponse struct {
	n   int
	err error
}

var ErrReadMismatch = errors.New("Read amount mismatch")

func (r *ParallelMergerResourceReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}

	var totalRead int
	group := errgroup.Group{}
	index := 0
	readResponses := make([]readResponse, 0, 1)

	for r.readerIndex < len(r.readers) {
		resourceSize, err := r.resource.resources[r.readerIndex].Size()
		if err != nil {
			return 0, fmt.Errorf("failed getting size from underlying resource %d: %w", r.readerIndex, err)
		}

		// What the reader can return
		readerRead := int(resourceSize - r.readerByteIndex)

		done := totalRead+readerRead >= len(p)

		// Start in parallel
		r.mu.Lock()
		readResponses = append(readResponses, readResponse{})
		r.mu.Unlock()
		readerIndex := r.readerIndex
		localIndex := index
		offset := totalRead
		group.Go(func() error {
			// Read from underlying reader into slice
			n, err := r.readers[readerIndex].Read(p[offset:])
			// Store result
			r.mu.RLock()
			readResponses[localIndex].n = n
			readResponses[localIndex].err = err
			r.mu.RUnlock()
			return nil
		})

		index++
		totalRead += readerRead

		// If buffer p will be full, we are done reading
		if done {
			r.readerByteIndex += int64(len(p) - totalRead)
			r.index += int64(totalRead)
			break
		} else {
			r.index += int64(totalRead)
			r.readerIndex++
			r.readerByteIndex = 0
		}
	}

	// Wait for completion
	err := group.Wait()
	if err != nil {
		return 0, fmt.Errorf("failed reading from underlying resource: %w", err)
	}

	// Read and check responses
	totalReadFromResponses := 0
	for _, response := range readResponses {
		totalReadFromResponses += response.n
		if response.err != nil && !errors.Is(response.err, io.EOF) {
			return 0, response.err
		}
	}

	if totalReadFromResponses != totalRead { // Plausibility check between calculated total and real total
		return 0, fmt.Errorf("expected to read %d but read %d: %w", totalRead, totalReadFromResponses, ErrReadMismatch)
	}

	if r.readerIndex < len(r.readers) {
		// Normal response
		return totalRead, nil
	}
	// All readers exhausted, EOF
	return totalRead, io.EOF
}

func (r *ParallelMergerResourceReader) Close() error {
	for i, reader := range r.readers {
		err := reader.Close()
		if err != nil {
			return fmt.Errorf("failed closing underlying resource %d: %w", i, err)
		}
	}
	return nil
}

func (r *ParallelMergerResourceReader) Seek(offset int64, whence int) (int64, error) {
	resourceSize, err := r.resource.Size()
	if err != nil {
		return 0, err
	}

	var newIndex int64

	switch whence {
	case io.SeekStart:
		newIndex = offset
	case io.SeekCurrent:
		newIndex = r.index + offset
	case io.SeekEnd:
		newIndex = resourceSize + offset
	}

	// Seek to same pos we are at
	if newIndex == r.index {
		return r.index, nil
	}
	// Out of range
	if newIndex < 0 || newIndex > resourceSize {
		return 0, resource.ErrInvalidSeek
	}

	// Find affected sub-reader
	readerIndex, readerByteIndex, err := r.getReaderIndexAndByteIndexAtByteIndex(newIndex)
	if err != nil {
		return 0, fmt.Errorf("failed getting reader from index: %w", err)
	}

	// Seek to position in reader
	_, err = r.readers[readerIndex].Seek(readerByteIndex, io.SeekStart)
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

	r.index = newIndex
	r.readerIndex = readerIndex
	r.readerByteIndex = readerByteIndex
	return r.index, nil
}

func (r *ParallelMergerResourceReader) getReaderIndexAndByteIndexAtByteIndex(index int64) (readerIndex int, readerByteIndex int64, err error) {
	var byteIndex int64 = 0
	for readerIndex := range r.readers {
		resource := r.resource.resources[readerIndex]
		size, err := resource.Size()
		if err != nil {
			return 0, 0, fmt.Errorf("failed getting size from underlying resource %d: %w", readerIndex, err)
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
