package ParallelMergerResource

import (
	"fmt"
	"io"
	"sync"

	"git.ruekov.eu/ruakij/nzbStreamer/pkg/resource"
)

// AdaptiveParallelMergerResource is a Resource type which allows combining multiple Resources as if it was one.
// It reads underlying sources in parallel, but requires their size to be known and exact!
type parallelMergerResource struct {
	resources []resource.ReadSeekCloseableResource
}

func NewParallelMergerResource(resources []resource.ReadSeekCloseableResource) *parallelMergerResource {
	return &parallelMergerResource{
		resources: resources,
	}
}

type ParallelMergerResourceReader struct {
	resource *parallelMergerResource
	readers  []io.ReadSeekCloser
	// Position in data
	index int64
	// Active reader index
	readerIndex int
	// Active reader byte index
	readerByteIndex int64
}

// Open will also eagerly open all underlying Resources
func (r *parallelMergerResource) Open() (reader io.ReadSeekCloser, err error) {
	readers := make([]io.ReadSeekCloser, len(r.resources), len(r.resources))
	for i, resource := range r.resources {
		readers[i], err = resource.Open()
		if err != nil {
			return
		}
	}

	reader = &ParallelMergerResourceReader{
		resource:        r,
		readers:         readers,
		index:           0,
		readerIndex:     0,
		readerByteIndex: 0,
	}
	return
}

func (mrm *parallelMergerResource) Size() (int64, error) {
	var totalSize int64
	for _, resource := range mrm.resources {
		size, err := resource.Size()
		if err != nil {
			return int64(totalSize), err
		}

		totalSize += size
	}
	return totalSize, nil
}

type readResponse struct {
	n   int
	err error
}

func (r *ParallelMergerResourceReader) Read(p []byte) (totalRead int, err error) {
	if len(p) == 0 {
		return
	}

	var wg sync.WaitGroup
	readResponses := make([]readResponse, 0, 1)

	for r.readerIndex < len(r.readers) {
		resourceSize, err := r.resource.resources[r.readerIndex].Size()
		if err != nil {
			return 0, err
		}

		// What the reader can return
		readerRead := int(resourceSize - r.readerByteIndex)

		done := totalRead+readerRead >= len(p)

		wg.Add(1)
		if !done { // Start in parallel
			readResponses = append(readResponses, readResponse{})
			go readWithReader(&wg, r.readers[r.readerIndex], &readResponses[len(readResponses)-1], p[totalRead:])
		} else { // Last reader starts sequentially, this also ensures we don't start any goroutines for only 1 reader
			readResponses = append(readResponses, readResponse{})
			readWithReader(&wg, r.readers[r.readerIndex], &readResponses[len(readResponses)-1], p[totalRead:])
		}

		// If buffer p will be full, we are done reading
		if done {
			r.readerByteIndex += int64(len(p) - totalRead)
			totalRead = len(p)

			r.index += int64(totalRead)
			break
		} else {
			totalRead += readerRead

			r.index += int64(totalRead)
			r.readerIndex++
			r.readerByteIndex = 0
		}
	}

	// Wait for completion
	wg.Wait()

	// Read and check responses
	totalReadFromResponses := 0
	for _, response := range readResponses {
		totalReadFromResponses += response.n
		if response.err != nil && response.err != io.EOF {
			return 0, response.err
		}
	}

	if totalReadFromResponses != totalRead { // Plausibility check between calculated total and real total
		return 0, fmt.Errorf("expected to read %d but read %d", totalRead, totalReadFromResponses)
	}

	if r.readerIndex < len(r.readers) {
		// Normal response
		return totalRead, nil
	}
	// All readers exhausted, EOF
	return totalRead, io.EOF
}

func readWithReader(wg *sync.WaitGroup, reader io.Reader, response *readResponse, buf []byte) {
	defer wg.Done()
	// Read from underlying reader into slice
	n, err := reader.Read(buf)
	// Store result
	response.n = n
	response.err = err
}

func (mrmr *ParallelMergerResourceReader) Close() (err error) {
	for _, reader := range mrmr.readers {
		err = reader.Close()
		if err != nil {
			return
		}
	}
	return
}

func (r *ParallelMergerResourceReader) Seek(offset int64, whence int) (newIndex int64, err error) {
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
		newIndex = resourceSize - offset
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
		return
	}

	// Seek to position in reader
	r.readers[readerIndex].Seek(readerByteIndex, io.SeekStart)
	// Seek to start for all following readers
	for i := readerIndex + 1; i < len(r.readers); i++ {
		r.readers[i].Seek(0, io.SeekStart)
	}

	r.index = newIndex
	r.readerIndex = readerIndex
	r.readerByteIndex = readerByteIndex
	return
}

func (mrmr *ParallelMergerResourceReader) getReaderIndexAndByteIndexAtByteIndex(index int64) (int, int64, error) {
	var byteIndex int64 = 0
	for readerIndex := range mrmr.readers {
		resource := mrmr.resource.resources[readerIndex]
		size, err := resource.Size()
		if err != nil {
			return 0, 0, err
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
