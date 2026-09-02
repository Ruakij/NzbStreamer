package adaptiveparallelmergerresource

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"sync"
	"sync/atomic"
	"time"

	"git.ruekov.eu/ruakij/nzbStreamer/pkg/resource"
	"golang.org/x/sync/errgroup"
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
	// Readers of resources touched so far; nil where not opened yet
	readers   []io.ReadSeekCloser
	readersMu sync.Mutex
	// Lowest index that may still hold an open reader
	openFrom    int
	mutex       sync.RWMutex
	readerGroup errgroup.Group
	// Position in data
	index int64
	// Active reader index
	readerIndex int
	// Active reader byte index
	readerByteIndex int64
	// Highest index prefetch has been issued for
	prefetchedTo int
	// Bytes served and since when, measuring how fast the consumer takes them.
	// A seek starts a new run, since a jump says nothing about the next one.
	runStart time.Time
	runBytes int64
}

// Open prepares buffers; underlying Resources are opened when a read reaches them,
// so descriptors are bound by what is read rather than by the size of the file.
func (r *AdaptiveParallelMergerResource) Open() (io.ReadSeekCloser, error) {
	return &AdaptiveParallelMergerResourceReader{
		resource:        r,
		readers:         make([]io.ReadSeekCloser, len(r.resources)),
		index:           0,
		readerIndex:     0,
		readerByteIndex: 0,
	}, nil
}

// reader opens resource i on first use and keeps it until it falls behind the
// read head.
func (r *AdaptiveParallelMergerResourceReader) reader(i int) (io.ReadSeekCloser, error) {
	r.readersMu.Lock()
	defer r.readersMu.Unlock()

	if r.readers[i] == nil {
		reader, err := r.resource.resources[i].Open()
		if err != nil {
			return nil, fmt.Errorf("failed opening resource %d: %w", i, err)
		}
		r.readers[i] = reader
		if i < r.openFrom {
			r.openFrom = i
		}
	}

	return r.readers[i], nil
}

// seekReader positions reader i, opening it if a read has not reached it yet.
func (r *AdaptiveParallelMergerResourceReader) seekReader(i int, offset int64) error {
	reader, err := r.reader(i)
	if err != nil {
		return err
	}

	if _, err := reader.Seek(offset, io.SeekStart); err != nil {
		return fmt.Errorf("failed seeking resource %d to %d: %w", i, offset, err)
	}

	return nil
}

// closeBehind releases the readers before the read head. A backwards seek
// reopens them, which is a fresh handle on cached bytes, not a refetch.
func (r *AdaptiveParallelMergerResourceReader) closeBehind() {
	r.readersMu.Lock()
	defer r.readersMu.Unlock()

	for ; r.openFrom < r.readerIndex && r.openFrom < len(r.readers); r.openFrom++ {
		if r.readers[r.openFrom] == nil {
			continue
		}
		//nolint:errcheck // Nothing to do with a failure of a reader we are done with
		r.readers[r.openFrom].Close()
		r.readers[r.openFrom] = nil
	}
}

func (r *AdaptiveParallelMergerResource) SizeHint() (int64, error) {
	var totalSize int64
	for i, resource := range r.resources {
		size, err := resource.SizeHint()
		if err != nil {
			return totalSize, fmt.Errorf("failed getting size from resource %d: %w", i, err)
		}

		totalSize += size
	}
	return totalSize, nil
}

// partSize is the length of resource i. A resource that knows it exactly answers
// for free; the rest have to be seeked to their end, which for an uncached
// segment means downloading it.
func (r *AdaptiveParallelMergerResourceReader) partSize(i int) (int64, error) {
	if sized, ok := r.resource.resources[i].(resource.Sized); ok {
		size, err := sized.Size()
		if err == nil {
			return size, nil
		}
		if !errors.Is(err, resource.ErrSizeNotExact) {
			return 0, fmt.Errorf("failed getting size from resource %d: %w", i, err)
		}
	}

	reader, err := r.reader(i)
	if err != nil {
		return 0, err
	}

	size, err := reader.Seek(0, io.SeekEnd)
	if err != nil {
		return 0, fmt.Errorf("failed seeking resource %d to end: %w", i, err)
	}

	return size, nil
}

type readResponse struct {
	index           int
	readerIndex     int
	readerByteIndex int64
	buffer          []byte
	n               int
	err             error
}

func (r *AdaptiveParallelMergerResourceReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}

	r.mutex.Lock()

	if r.runStart.IsZero() {
		r.runStart = time.Now()
	}
	r.prefetch()

	totalRead := 0
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
	processIndex := 0

	// Unlock mutex, when group finished
	// TODO: Maybe its possible to only block affected readers separately not to halt all activity? Might not be that critical though
	defer func() {
		r.runBytes += int64(totalRead)

		// When already everything processed, dont start goroutine
		if processIndex >= len(responses) {
			defer r.mutex.Unlock()
			// group should have finished, in case it hasnt, wait
			group.Wait()
			r.closeBehind()
			return
		}
		go func() {
			defer r.mutex.Unlock()
			defer r.closeBehind()

			// Function to process responses
			processResponses := func() {
				responsesLock.Lock()
				defer responsesLock.Unlock()

				// Stop at the first reader still running; the pass after
				// group.Wait picks it up
				for processIndex < len(responses) && responses[processIndex] != nil {
					// Read has concluded, seek back
					if reader, err := r.reader(responses[processIndex].readerIndex); err == nil {
						reader.Seek(0, io.SeekStart)
					}
					processIndex++
				}
			}

			// Process remaining responses
			processResponses()

			// Wait for group & discard error, we can't raise it here anyways
			group.Wait()

			// Process remaining responses after waiting for all goroutines to finish
			processResponses()
		}()
	}()

	// Start readers
	for expectedTotalRead < len(p) && r.readerIndex < len(r.readers) {
		requiredRead := len(p) - expectedTotalRead

		resourceSize, err := r.resource.resources[r.readerIndex].SizeHint()
		if err != nil {
			return 0, fmt.Errorf("failed getting size from resource %d: %w", r.readerIndex, err)
		}

		// TODO: When resourceSize is fully unknown all of this falls apart
		resourceSizeLeft := int(resourceSize - r.readerByteIndex)
		if resourceSizeLeft < 0 {
			resourceSizeLeft = 0
		}

		// Expect either full resource or part up to whatever is expected to be needed at this point
		expectedRead := resourceSizeLeft
		if requiredRead < expectedRead {
			expectedRead = requiredRead
		}

		responsesLock.Lock()
		responses = append(responses, nil) // Reserve space
		responsesLock.Unlock()

		activeReaders++

		// In case last read of len=x was sufficient, but not at the end and resourceSize<=x which would lead to calculation above leading to 0
		// TODO: Enforce min. read per reader for unknown sizes?
		if expectedRead <= 0 {
			expectedRead = 1
		}

		expectedTotalRead += expectedRead

		// Copy non-local vars to local stack for goroutine
		localReadIndex := readIndex
		readerIndex := r.readerIndex
		readerByteIndex := r.readerByteIndex

		// Modify here to avoid data races
		readIndex++
		if expectedRead < resourceSizeLeft {
			r.readerByteIndex += int64(expectedRead)
		} else {
			r.readerIndex++
			r.readerByteIndex = 0
		}

		group.Go(func() error {
			reader, err := r.reader(readerIndex)
			if err != nil {
				return err
			}

			sized, isSized := r.resource.resources[readerIndex].(resource.Sized)
			// TODO: Support writing directly to p if supported (all previous readers also need to have accurate resource)
			buf := make([]byte, expectedRead)
			totalN := 0
			var n int
			var prevNCount int
			for {
				// Check if job is cancelled while before next read
				select {
				case <-readCtx.Done():
					break
				default:
				}

				n, err = reader.Read(buf[totalN:])
				totalN += n

				// With an exact size a single read suffices
				if isSized {
					_, sizeErr := sized.Size()
					if sizeErr == nil {
						break
					}
					if !errors.Is(sizeErr, resource.ErrSizeNotExact) {
						return fmt.Errorf("failed getting size from resource %d: %w", readerIndex, sizeErr)
					}
				}

				// Part reads dont require EOF
				if expectedRead < resourceSizeLeft {
					break
				}

				// If we read nothing 3 times consecutively with no error, stop with error
				if n == 0 && err == nil {
					if prevNCount >= 3-1 {
						err = io.ErrNoProgress
					} else {
						prevNCount++
					}
				} else {
					prevNCount = 0
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
				index:           localReadIndex,
				readerIndex:     readerIndex,
				readerByteIndex: readerByteIndex,
				buffer:          buf[:totalN],
				n:               totalN,
				err:             err,
			}
			responsesLock.RUnlock()
			responsesCond.Signal() // Signal that a response is ready
			return nil
		})
	}

	// Process responses
	for processIndex < len(responses) {
		responsesLock.Lock()
		// Wait for next response to be ready
		for responses[processIndex] == nil {
			responsesCond.Wait()
		}
		response := responses[processIndex]
		responsesLock.Unlock()

		activeReaders--

		if response.err != nil && !errors.Is(response.err, io.EOF) {
			return 0, response.err
		}

		actualRead := response.n

		// Copy data to p
		if totalRead < len(p) {
			copied := copy(p[totalRead:], response.buffer[:actualRead])
			totalRead += copied
			// TODO: These global vars should only be manipulated here, when we actually process the response, not up in the for loop!
			r.index += int64(copied)
			r.readerIndex = response.readerIndex
			r.readerByteIndex = response.readerByteIndex + int64(copied)

			// TODO: Also move this into deferred group-finish action to not have to wait for seek?
			if copied < actualRead {
				// When not all was copied, we filled p, the rest is too much
				if err := r.seekReader(response.readerIndex, int64(copied)); err != nil {
					return totalRead, err
				}
			}
		}

		processIndex++

		// If we just filled p, we are done
		if totalRead == len(p) {
			break
		}
	}

	// Cancel if any work is left
	readCtxDone()

	var err error
	if len(responses) > 0 {
		lastReadResponse := responses[processIndex-1]

		// When last processed response hit EOF, advance readers
		if errors.Is(lastReadResponse.err, io.EOF) {
			r.readerIndex++
			r.readerByteIndex = 0
		} /*else {
			r.readerIndex = lastReadResponse.readerIndex
			r.readerByteIndex = lastReadResponse.readerByteIndex
		}*/

		// When last response was from last actual reader
		if lastReadResponse.readerIndex == len(r.readers)-1 && lastReadResponse.err != nil {
			err = lastReadResponse.err
		}
	}

	return totalRead, err
}

func (r *AdaptiveParallelMergerResourceReader) Close() error {
	// TODO: Cancel everything immediately on close
	r.mutex.Lock()
	defer r.mutex.Unlock()
	r.readersMu.Lock()
	defer r.readersMu.Unlock()

	for i, reader := range r.readers {
		if reader == nil {
			continue
		}
		err := reader.Close()
		if err != nil {
			return fmt.Errorf("failed closing reader %d: %w", i, err)
		}
	}
	r.readers = nil
	return nil
}

func (r *AdaptiveParallelMergerResourceReader) Seek(offset int64, whence int) (int64, error) {
	r.mutex.Lock()
	defer r.mutex.Unlock()
	// A jump has no lead to keep and says nothing about the speed of what
	// follows, so both are re-established from wherever this lands
	defer func() {
		r.prefetchedTo = r.readerIndex
		r.runStart = time.Time{}
		r.runBytes = 0
		r.closeBehind()
	}()

	var newIndex int64

	switch whence {
	case io.SeekStart:
		newIndex = offset
	case io.SeekCurrent:
		newIndex = r.index + offset
	case io.SeekEnd:
		err := r.seekToEnd()
		if err != nil {
			return 0, fmt.Errorf("failed seeking to end: %w", err)
		}

		newIndex = r.index + offset
	}

	// Seek to same pos we are at
	if newIndex == r.index {
		return r.index, nil
	}
	// Out of range
	if newIndex < 0 {
		return 0, resource.ErrInvalidSeek
	}

	// Check seek direction
	var err error
	if r.index < newIndex {
		err = r.seekForwards(newIndex - r.index)
	} else {
		err = r.seekBackwards(r.index - newIndex)
	}
	if err != nil {
		return r.index, err
	}

	return r.index, nil
}

type seekResponse struct {
	readerIndex int
	expected    int64
	actual      int64
	err         error
}

func (r *AdaptiveParallelMergerResourceReader) seekForwards(seekAmount int64) error {
	var expectedTotalSeek int64 = 0
	var totalSeeked int64 = 0
	index := 0
	processIndex := 0

	// Local variables to work with in the loop
	readerIndex := r.readerIndex
	readerByteIndex := r.readerByteIndex

	responses := make([]*seekResponse, 0, 1)
	responsesLock := &sync.RWMutex{}
	responsesCond := sync.NewCond(responsesLock)

	for totalSeeked < seekAmount && readerIndex < len(r.readers) {
		for expectedTotalSeek < seekAmount && readerIndex < len(r.readers) {
			resource := r.resource.resources[readerIndex]

			resourceSizeHint, err := resource.SizeHint()
			if err != nil {
				return fmt.Errorf("failed getting size from resource %d: %w", readerIndex, err)
			}

			expectedSeek := resourceSizeHint - readerByteIndex
			if expectedSeek < 0 {
				expectedSeek = 0
			}

			expectedTotalSeek += expectedSeek

			responsesLock.Lock()
			responses = append(responses, nil) // Reserve space
			responsesLock.Unlock()

			// Copy non-local vars to local stack for goroutine
			localIndex := index
			localReaderIndex := readerIndex
			localReaderByteIndex := readerByteIndex
			r.readerGroup.Go(func() error {
				size, err := r.partSize(localReaderIndex)

				responsesLock.RLock()
				responses[localIndex] = &seekResponse{
					readerIndex: localReaderIndex,
					expected:    expectedSeek,
					actual:      size - localReaderByteIndex,
					err:         err,
				}
				responsesLock.RUnlock()
				responsesCond.Signal()

				return nil
			})

			index++
			readerIndex++
			readerByteIndex = 0
		}

		for processIndex < len(responses) {
			// Process all responses we expect in order
			responsesLock.Lock()
			// Wait for next response to be ready
			for responses[processIndex] == nil {
				responsesCond.Wait()
			}
			responsesLock.Unlock()
			response := responses[processIndex]

			if response.err != nil {
				return fmt.Errorf("failed to SeekEnd resource %d: %w", response.readerIndex, response.err)
			}

			if totalSeeked < seekAmount {
				totalSeeked += response.actual

				// With this reader we reached the seekAmount
				if totalSeeked > seekAmount {
					// Seek affected reader back to correct position
					seekOffset := response.actual - (totalSeeked - seekAmount)
					if r.readerIndex == response.readerIndex {
						// If reader is still current one, add its readerByteIndex
						seekOffset += r.readerByteIndex
					}
					if err := r.seekReader(response.readerIndex, seekOffset); err != nil {
						return err
					}

					r.readerIndex = response.readerIndex
					r.readerByteIndex = seekOffset
					totalSeeked = seekAmount
					r.index += seekAmount
				}
			} else {
				// Already reached, seek reader back
				if err := r.seekReader(response.readerIndex, 0); err != nil {
					return err
				}
			}

			processIndex++
		}
	}

	// Wait for goroutines to finish, this shouldnt be the case as we wait for all responses above, but just in case
	//nolint:errcheck // There is no error
	r.readerGroup.Wait()

	return nil
}

func (r *AdaptiveParallelMergerResourceReader) seekBackwards(seekAmount int64) error {
	var expectedTotalSeek int64 = 0
	var totalSeeked int64 = 0
	index := 0
	processIndex := 0

	// Local variables to work with in the loop
	readerIndex := r.readerIndex
	readerByteIndex := r.readerByteIndex

	responses := make([]*seekResponse, 0, 1)
	responsesLock := &sync.RWMutex{}
	responsesCond := sync.NewCond(responsesLock)

	// When we have some bytes left in a reader, we know how much we will seek back there
	// readerByteIndex = 0 here means not yet seeked backwards
	if readerByteIndex > 0 {
		responses = append(responses, &seekResponse{
			readerIndex: readerIndex,
			expected:    readerByteIndex,
			actual:      readerByteIndex,
			err:         nil,
		})

		index++
		readerIndex--
		readerByteIndex = 0
	} else {
		// When we are at 0, next reader backwards is one less
		readerIndex--
	}

	for (totalSeeked < seekAmount && readerIndex >= 0) || processIndex < len(responses) {
		for expectedTotalSeek < seekAmount && readerIndex >= 0 {
			resource := r.resource.resources[readerIndex]

			resourceSizeHint, err := resource.SizeHint()
			if err != nil {
				return fmt.Errorf("failed getting size from resource %d: %w", readerIndex, err)
			}

			expectedSeek := resourceSizeHint

			expectedTotalSeek += expectedSeek

			responsesLock.Lock()
			responses = append(responses, nil) // Reserve space
			responsesLock.Unlock()

			// Copy non-local vars to local stack for goroutine
			localIndex := index
			localReaderIndex := readerIndex
			r.readerGroup.Go(func() error {
				size, err := r.partSize(localReaderIndex)

				responsesLock.RLock()
				responses[localIndex] = &seekResponse{
					readerIndex: localReaderIndex,
					expected:    expectedSeek,
					actual:      size,
					err:         err,
				}
				responsesLock.RUnlock()
				responsesCond.Signal()

				return nil
			})

			index++
			readerIndex--
			readerByteIndex = 0
		}

		for processIndex < len(responses) {
			// Process all responses we expect in order
			responsesLock.Lock()
			// Wait for next response to be ready
			for responses[processIndex] == nil {
				responsesCond.Wait()
			}
			responsesLock.Unlock()
			response := responses[processIndex]

			if response.err != nil {
				return fmt.Errorf("failed to SeekEnd resource %d: %w", response.readerIndex, response.err)
			}

			// With this reader we reached the seekAmount
			if totalSeeked < seekAmount {
				totalSeeked += response.actual

				// With this reader we reached the seekAmount
				if totalSeeked >= seekAmount {
					// Seek affected reader back
					seekPos := totalSeeked - seekAmount
					if err := r.seekReader(response.readerIndex, seekPos); err != nil {
						return err
					}

					r.readerIndex = response.readerIndex
					r.readerByteIndex = seekPos
					totalSeeked = seekAmount
					r.index -= seekAmount
				} else {
					// Otherwise seek to 0
					if err := r.seekReader(response.readerIndex, 0); err != nil {
						return err
					}
				}
			}

			processIndex++
		}
	}

	// Wait for goroutines to finish, this shouldnt be the case as we wait for all responses above, but just in case
	//nolint:errcheck // There is no error
	r.readerGroup.Wait()

	return nil
}

func (r *AdaptiveParallelMergerResourceReader) seekToEnd() error {
	var seekAmountAtomic atomic.Int64
	seekAmountAtomic.Add(-r.readerByteIndex)

	for i := r.readerIndex; i < len(r.readers); i++ {
		r.readerGroup.Go(func() error {
			size, err := r.partSize(i)
			if err != nil {
				//nolint:wrapcheck // Error is handled outside
				return err
			}
			seekAmountAtomic.Add(size)

			// Last reader sets readerByteIndex and has to sit where it says
			if i == len(r.readers)-1 {
				if err := r.seekReader(i, size); err != nil {
					return err
				}
				r.readerByteIndex = size
			}

			return nil
		})
	}

	err := r.readerGroup.Wait()
	if err != nil {
		return fmt.Errorf("failed seeking all readers to end: %w", err)
	}

	r.readerIndex = len(r.readers) - 1
	r.index += seekAmountAtomic.Load()

	return nil
}

// Size is exact only once every part knows its own length, since the total is
// their sum.
func (r *AdaptiveParallelMergerResource) Size() (int64, error) {
	var totalSize int64
	for i, re := range r.resources {
		sized, ok := re.(resource.Sized)
		if !ok {
			return 0, resource.ErrSizeNotExact
		}

		size, err := sized.Size()
		if err != nil {
			return 0, fmt.Errorf("failed getting size from resource %d: %w", i, err)
		}

		totalSize += size
	}
	return totalSize, nil
}
