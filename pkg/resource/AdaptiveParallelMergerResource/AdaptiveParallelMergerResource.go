package AdaptiveParallelMergerResource

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"

	"git.ruekov.eu/ruakij/nzbStreamer/pkg/resource"
	"golang.org/x/sync/errgroup"
)

const (
	ReadModeNormal   = iota
	ReadModeAdaptive = iota
)

// AdaptiveParallelMergerResource is a Resource type which allows combining multiple Resources as if it was one.
// It reads underlying sources in parallel and can handle their size to be unknown.
type AdaptiveParallelMergerResource struct {
	resources []resource.ReadSeekCloseableResource
}

func NewAdaptiveParallelMergerResource(resources []resource.ReadSeekCloseableResource) *AdaptiveParallelMergerResource {
	return &AdaptiveParallelMergerResource{
		resources: resources,
	}
}

type AdaptiveParallelMergerResourceReader struct {
	resource *AdaptiveParallelMergerResource
	readers  []io.ReadSeekCloser
	// Position in data
	index int64
	// Active reader index
	readerIndex int
	// Active reader byte index
	readerByteIndex int64
}

// Open prepares buffers and eagerly opens all underlying Resources
func (r *AdaptiveParallelMergerResource) Open() (reader io.ReadSeekCloser, err error) {
	readers := make([]io.ReadSeekCloser, len(r.resources), len(r.resources))
	for i, resource := range r.resources {
		readers[i], err = resource.Open()
		if err != nil {
			return
		}
	}

	reader = &AdaptiveParallelMergerResourceReader{
		resource:        r,
		readers:         readers,
		index:           0,
		readerIndex:     0,
		readerByteIndex: 0,
	}
	return
}

func (mrm *AdaptiveParallelMergerResource) Size() (int64, error) {
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
	index       int
	readerIndex int
	buffer      []byte
	n           int
	err         error
}

func (r *AdaptiveParallelMergerResourceReader) Read(p []byte) (totalRead int, err error) {
	if len(p) == 0 {
		return
	}

	expectedTotalRead := 0

	readResponses := make([]*readResponse, 0, 1)
	readResponsesLock := &sync.Mutex{}
	readResponsesCond := sync.NewCond(readResponsesLock)

	readCtx, readCtxDone := context.WithCancel(context.Background())
	defer readCtxDone()
	group, _ := errgroup.WithContext(readCtx)

	// Which local index this reader and thus readResponse has
	readIndex := 0
	activeReaders := 0

	// Start readers
	for expectedTotalRead < len(p) && r.readerIndex < len(r.readers) {
		requiredRead := len(p) - expectedTotalRead

		resourceSize, err := r.resource.resources[r.readerIndex].Size()
		if err != nil {
			return 0, err
		}

		resourceSizeLeft := int(resourceSize - r.readerByteIndex)

		// Expect either full resource or part up to whatever is expected to be needed at this point
		expectedRead := resourceSizeLeft
		if requiredRead < expectedRead {
			expectedRead = requiredRead
		}

		localReadIndex := readIndex
		readerIndex := r.readerIndex

		readResponses = append(readResponses, nil) // Reserve space

		activeReaders++

		// TODO: This only reads up to max. resourceSize, but if thats reported too small, we are missing data; We need to handle this e.g. reading until EOF or len(p)
		group.Go(func() (err error) {
			/*select {
			case <-readCtx.Done():
				return nil
			default:
			}*/

			buf := make([]byte, expectedRead)
			n, err := r.readers[readerIndex].Read(buf)

			readResponses[localReadIndex] = &readResponse{
				index:       localReadIndex,
				readerIndex: readerIndex,
				buffer:      buf,
				n:           n,
				err:         err,
			}
			readResponsesCond.Signal() // Signal that a response is ready
			return
		})

		readIndex++

		if expectedRead < resourceSizeLeft {
			r.readerByteIndex += int64(expectedRead)
		} else {
			r.readerIndex++
			r.readerByteIndex = 0
		}

		expectedTotalRead += expectedRead
	}

	// Process responses
	for idx := 0; idx < len(readResponses); idx++ {
		readResponsesLock.Lock()
		// Wait for next response to be ready
		for readResponses[idx] == nil {
			readResponsesCond.Wait()
		}
		response := readResponses[idx]
		readResponsesLock.Unlock()

		activeReaders--

		if response.err != nil && response.err != io.EOF {
			return 0, response.err
		}

		//expectedRead := len(response.buffer)
		actualRead := response.n

		// Copy data to p
		if totalRead < len(p) {
			copied := copy(p[totalRead:], response.buffer[:actualRead])
			totalRead += copied
			r.index += int64(copied)
			r.readerIndex = response.readerIndex

			if copied < actualRead {
				// When not all was copied, we filled p, the rest is too much
				r.readerByteIndex += int64(copied)
				fmt.Printf("%p\tRead too far, seeking %d\tto %d\n", r, response.readerIndex, r.readerByteIndex)
				r.readers[response.readerIndex].Seek(r.readerByteIndex, io.SeekStart)
			}
		} else {
			// Filled p, anymore is too much
			fmt.Printf("%p\tRead too far, seeking %d\tto %d\n", r, response.readerIndex, 0)
			r.readers[response.readerIndex].Seek(0, io.SeekStart)
		}
	}

	// Cancel if any work is left
	//readCtxDone()
	// Wait for all goroutines to finish, this should never be the case, but as good practise included
	group.Wait()

	if len(readResponses) > 0 {
		lastReadResponse := readResponses[len(readResponses)-1]
		// When last response was from last reader in batch and hit EOF, we need to advance readers
		if lastReadResponse.err == io.EOF {
			r.readerIndex++
			r.readerByteIndex = 0
		}
		// When last response was from last actual reader
		if lastReadResponse.readerIndex == len(r.readers)-1 && lastReadResponse.err != nil {
			err = lastReadResponse.err
		}
	}

	return totalRead, err
}

func (mrmr *AdaptiveParallelMergerResourceReader) Close() (err error) {
	for _, reader := range mrmr.readers {
		err = reader.Close()
		if err != nil {
			return
		}
	}
	return
}

func (r *AdaptiveParallelMergerResourceReader) Seek(offset int64, whence int) (newIndex int64, err error) {
	switch whence {
	case io.SeekStart:
		newIndex = offset
	case io.SeekCurrent:
		if offset < 0 {
			return 0, errors.New("Cannot seek backwards")
		}
		newIndex = r.index + offset
	case io.SeekEnd:
		return 0, errors.New("Cannot seek from end")
	}

	// Seek to same pos we are at
	if newIndex == r.index {
		return r.index, nil
	}
	// Out of range
	if newIndex < 0 {
		return 0, resource.ErrInvalidSeek
	}

	if r.index < newIndex {
		// Skip forwards
		_, err = discardBytes(r, newIndex-r.index)
	} else {
		// We cannot actually seek, so seeking backwards is specially not supported
		// Seek all readers to start
		for _, reader := range r.readers {
			if _, err = reader.Seek(0, io.SeekStart); err != nil {
				return
			}
		}
		// Seek from start
		r.index = 0
		r.readerIndex = 0
		r.readerByteIndex = 0

		_, err = discardBytes(r, newIndex)
	}
	if err != nil {
		return 0, err
	}

	return
}

func discardBytes(reader io.Reader, amountToDiscard int64) (totalDiscarded int64, err error) {
	bufferSize := 1 * 1024 * 1024
	buf := make([]byte, bufferSize)
	var n int

	for totalDiscarded < amountToDiscard {
		bytesToRead := bufferSize
		if amountToDiscard-totalDiscarded < int64(bufferSize) {
			bytesToRead = int(amountToDiscard - totalDiscarded)
		}

		n, err = io.ReadFull(reader, buf[:bytesToRead])
		totalDiscarded += int64(n)

		if err != nil {
			return
		}
	}

	return
}
