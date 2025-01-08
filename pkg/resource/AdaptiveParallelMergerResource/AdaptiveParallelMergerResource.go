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
						expectedRead = int(float32(expectedRead) * 1.1)
						buf = append(buf, make([]byte, expectedRead-totalN)...)
					}
				} else {
					break
				}
			}

			readResponses[localReadIndex] = &readResponse{
				index:       localReadIndex,
				readerIndex: readerIndex,
				buffer:      buf,
				n:           totalN,
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
		newIndex = r.index + offset
	case io.SeekEnd:
		// When seeking from the end, all resources need to be accurate to reliably get to its position
		if !areResourcesAccurate(r.resource.resources) {
			return 0, errors.New("Not all resources accurate, cannot SeekEnd")
		}
		totalSize, err := r.resource.Size()
		if err != nil {
			return 0, err
		}
		newIndex = totalSize + offset
	default:
		return 0, errors.New("invalid whence value")
	}

	// Try to seek forward or backward based on the new index
	if newIndex != r.index {
		if newIndex > r.index {
			err = seekThroughReaders(r, newIndex-r.index)
		} else {
			// Reset all readers when seeking backwards
			for _, reader := range r.readers {
				if _, err = reader.Seek(0, io.SeekStart); err != nil {
					return 0, err
				}
			}
			r.index = 0
			r.readerIndex = 0
			r.readerByteIndex = 0

			err = seekThroughReaders(r, newIndex)
		}
	}
	if err != nil {
		return 0, err
	}
	return newIndex, nil
}

func areResourcesAccurate(resources []resource.ReadSeekCloseableResource) bool {
	for _, r := range resources {
		if sizeAccurateResource, ok := r.(resource.SizeAccurateResource); ok && sizeAccurateResource.IsSizeAccurate() {
			if !sizeAccurateResource.IsSizeAccurate() {
				return false
			}
		} else {
			return false
		}
	}
	return true
}

func seekThroughReaders(r *AdaptiveParallelMergerResourceReader, remaining int64) error {
	// Start iterating over the readers until we've sought through all bytes or run out of readers.
	remainingBytes := remaining
	for r.readerIndex < len(r.readers) && remainingBytes > 0 {
		currentReader := r.readers[r.readerIndex]
		currentResource := r.resource.resources[r.readerIndex]

		// Check if the current reader can report an accurate size.
		if sizeAccurateResource, ok := currentResource.(resource.SizeAccurateResource); ok && sizeAccurateResource.IsSizeAccurate() {
			// Retrieve the size of the current reader.
			size, err := currentResource.Size()
			if err != nil {
				return err
			}

			// Calculate if the remaining bytes to seek are within the current resource.
			if r.readerByteIndex+remainingBytes < size {
				// Seek within the current reader to the needed position.
				if _, err := currentReader.Seek(r.readerByteIndex+remainingBytes, io.SeekStart); err != nil {
					return err
				}
				// Update internal trackers for position and index.
				r.readerByteIndex += remainingBytes
				r.index += remainingBytes
				return nil // Seeking finished.
			} else {
				// Adjust remaining bytes since we'll need to jump to the next reader.
				remainingBytes -= size - r.readerByteIndex
				r.index += size - r.readerByteIndex
				// Reset reader position and advance to the next reader.
				r.readerByteIndex = 0
				r.readerIndex++
			}
		} else {
			// If current reader size is not accurate, use a utility to discard bytes.
			consumed, err := io.CopyN(io.Discard, currentReader, remainingBytes)
			if err != nil && err != io.EOF {
				return err
			}

			r.index += consumed
			remainingBytes -= consumed

			// When we read EOF, next reader
			if err != nil && err == io.EOF {
				r.readerByteIndex = 0
				r.readerIndex++
			} else if remainingBytes == 0 {
				// When we read all of remainingBytes, thats our new index
				r.readerByteIndex += consumed
				return nil // Seeking finished.
			}
		}
	}
	// If we exit the loop, it means we've processed all readers or there's no more to seek.
	return nil
}
