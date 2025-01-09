package AdaptiveParallelMergerResource

import (
	"context"
	"errors"
	"io"
	"math"
	"sync"
	"sync/atomic"

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
	resource    *AdaptiveParallelMergerResource
	readers     []io.ReadSeekCloser
	readerGroup errgroup.Group
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

	responses := make([]*readResponse, 0, 1)
	responsesLock := &sync.RWMutex{}
	responsesCond := sync.NewCond(responsesLock)

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

		responsesLock.Lock()
		responses = append(responses, nil) // Reserve space
		responsesLock.Unlock()

		activeReaders++

		if expectedRead < resourceSizeLeft {
			r.readerByteIndex += int64(expectedRead)
		} else {
			r.readerIndex++
			r.readerByteIndex = 0
		}

		expectedTotalRead += expectedRead

		group.Go(func() (_ error) {
			// Check if resource supports size accuracy reporting
			sizeAccurateResource, sizeAccurateResourceOk := r.resource.resources[readerIndex].(resource.SizeAccurateResource)
			// TODO: Support writing directly to p if supported (all previous readers also need to have accurate resource)
			buf := make([]byte, expectedRead)
			totalN, n := 0, 0
			var err error
			for {
				// Check if job is cancelled while before next read
				select {
				case <-readCtx.Done():
					return nil
				default:
				}

				n, err = r.readers[readerIndex].Read(buf[totalN:])
				totalN += n

				// If underlyingResource supports accuracy reporting and its accurate, single read suffices
				if sizeAccurateResourceOk && sizeAccurateResource.IsSizeAccurate() {
					break
				}

				// Part reads dont require EOF
				if expectedRead < resourceSizeLeft {
					break
				}

				// When there is no EOF yet and we are below the total read request
				if err == nil && totalN < len(p) {
					if totalN == len(buf) {
						// When we read our buffer full, increase read request by 10%; n < len(buf)-totalN might indicate we did hit EOF, but will only be returned at next read
						expectedRead = int(math.Ceil(float64(expectedRead) * 1.1))
						buf = append(buf, make([]byte, expectedRead-totalN)...)
					}
				} else {
					break
				}
			}

			responsesLock.RLock()
			responses[localReadIndex] = &readResponse{
				index:       localReadIndex,
				readerIndex: readerIndex,
				buffer:      buf,
				n:           totalN,
				err:         err,
			}
			responsesLock.RUnlock()
			responsesCond.Signal() // Signal that a response is ready
			return
		})

		readIndex++
	}

	// Process responses
	for idx := 0; idx < len(responses); idx++ {
		responsesLock.Lock()
		// Wait for next response to be ready
		for responses[idx] == nil {
			responsesCond.Wait()
		}
		response := responses[idx]
		responsesLock.Unlock()

		activeReaders--

		if response.err != nil && response.err != io.EOF {
			// TODO: Early returns cancel the goroutines, but already running ones will complete their work, leaving the readers in an instable state; A solution must be found i.e. Seeking when switching to a new reader or via goroutine and lock to seek affected readers back
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
				r.readers[response.readerIndex].Seek(r.readerByteIndex, io.SeekStart)
			}
		} else {
			// Filled p, anymore is too much
			r.readers[response.readerIndex].Seek(0, io.SeekStart)
		}
	}

	// Cancel if any work is left
	//readCtxDone()
	// Wait for all goroutines to finish, this should never be the case, but as good practise included
	group.Wait()

	if len(responses) > 0 {
		lastReadResponse := responses[len(responses)-1]
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
		newIndex = r.index + offset
	case io.SeekEnd:
		// Seek all to end to get their accurate size
		var totalSize atomic.Int64
		for _, reader := range r.readers {
			r.readerGroup.Go(func() (err error) {
				size, err := reader.Seek(0, io.SeekEnd)

				if err != nil {
					return
				}
				// TODO: Inefficient as we might be calling seek of readers up to 4 times!
				// And back to start
				if _, err = reader.Seek(0, io.SeekStart); err != nil {
					return
				}
				totalSize.Add(size)
				return
			})
		}

		err := r.readerGroup.Wait()
		if err != nil {
			return 0, err
		}
		newIndex = totalSize.Load() + offset
	default:
		return 0, errors.New("invalid whence value")
	}

	if newIndex == r.index {
		return
	}

	// Try to seek forward or backward based on the new index
	if newIndex > r.index {
		err = seekThroughReaders(r, newIndex-r.index)
	} else {
		// TODO: Actually seek back instead of resetting all and seeking forwards
		// Reset all readers when seeking backwards
		for _, reader := range r.readers {
			r.readerGroup.Go(func() (err error) {
				_, err = reader.Seek(0, io.SeekStart)
				return
			})
		}
		if err = r.readerGroup.Wait(); err != nil {
			return 0, err
		}

		r.index = 0
		r.readerIndex = 0
		r.readerByteIndex = 0

		err = seekThroughReaders(r, newIndex)
	}

	if err != nil {
		return r.index, err
	}
	return r.index, nil
}

func (r *AdaptiveParallelMergerResource) IsSizeAccurate() bool {
	for _, re := range r.resources {
		if sizeAccurateResource, ok := re.(resource.SizeAccurateResource); ok && sizeAccurateResource.IsSizeAccurate() {
			if !sizeAccurateResource.IsSizeAccurate() {
				return false
			}
		} else {
			return false
		}
	}
	return true
}

type seekResponse struct {
	index       int
	readerIndex int
	expected    int64
	actual      int64
	err         error
}

func seekThroughReaders(r *AdaptiveParallelMergerResourceReader, seekAmount int64) error {
	// Start iterating over the readers until we've sought through all bytes or run out of readers.

	var expectedTotalSeek int64 = 0
	var totalSeeked int64 = 0
	responses := make([]*seekResponse, 0, 1)
	responsesLock := &sync.RWMutex{}
	responsesCond := sync.NewCond(responsesLock)

	readCtx, readCtxDone := context.WithCancel(context.Background())
	defer readCtxDone()
	group, _ := errgroup.WithContext(readCtx)

	index := 0
	processIndex := 0

	for totalSeeked < seekAmount && r.readerIndex < len(r.readers) && (index == 0 || processIndex <= index) {
		for totalSeeked+expectedTotalSeek < seekAmount && r.readerIndex < len(r.readers) {
			reader := r.readers[r.readerIndex]
			resource := r.resource.resources[r.readerIndex]

			size, err := resource.Size()
			if err != nil {
				return err
			}

			expectedSeek := size - r.readerByteIndex

			responsesLock.Lock()
			responses = append(responses, nil) // Reserve space
			responsesLock.Unlock()

			// Copy to local stack into goroutine
			localIndex := index
			readerIndex := r.readerIndex
			readerByteIndex := r.readerByteIndex

			group.Go(func() (err error) {
				// Determine size by seeking to the end and capturing the current position.
				size, err := reader.Seek(0, io.SeekEnd)

				responsesLock.RLock()
				responses[localIndex] = &seekResponse{
					index:       localIndex,
					readerIndex: readerIndex,
					expected:    expectedSeek,
					actual:      size - readerByteIndex,
					err:         err,
				}
				responsesLock.RUnlock()
				responsesCond.Signal()

				return
			})

			index++
			r.readerIndex++
			r.readerByteIndex = 0

			expectedTotalSeek += expectedSeek
		}

		// Process responses
		responsesLock.Lock()
		// Wait for next response to be ready
		for responses[processIndex] == nil {
			responsesCond.Wait()
		}
		response := responses[processIndex]
		responsesLock.Unlock()

		if response.err != nil && response.err != io.EOF {
			// TODO: Early returns cancel the goroutines, but already running ones will complete their work, leaving the readers in an unknown state; A solution must be found i.e. Seeking when switching to a new reader or via goroutine and lock to seek affected readers back
			return response.err
		}

		reader := r.readers[response.readerIndex]

		expectedTotalSeek -= response.expected

		if totalSeeked >= seekAmount {
			// We already have seeked far enough, this one is too far
			if _, err := reader.Seek(0, io.SeekStart); err != nil {
				return err
			}
		} else {
			totalSeeked += response.actual
			readerSeek := totalSeeked - seekAmount - 1
			if totalSeeked >= seekAmount {
				totalSeeked = seekAmount
			}

			r.readerIndex = response.readerIndex

			// Calculate if the remaining bytes to seek are within the current resource.
			if readerSeek > 0 {
				// Seek within the current reader to the needed position.
				if _, err := reader.Seek(readerSeek, io.SeekStart); err != nil {
					return err
				}
				// Update internal trackers for position and index.
				r.readerByteIndex = readerSeek
				// Seeking finished, but cant return, need to process the rest
			}
		}

		processIndex++
	}

	// Wait for all goroutines to finish, this should never be the case, but as good practise included
	group.Wait()

	r.index += totalSeeked

	// If we exit the loop, it means we've processed all readers or there's no more to seek.
	return nil
}
