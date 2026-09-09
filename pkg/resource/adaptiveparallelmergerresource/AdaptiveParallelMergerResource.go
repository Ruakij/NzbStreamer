// Package adaptiveparallelmergerresource merges a list of resources into one
// stream, reading in parallel or sequentially depending on the access.
package adaptiveparallelmergerresource

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"sort"
	"sync"
	"sync/atomic"

	"git.ruekov.eu/ruakij/nzbStreamer/pkg/resource"
	"golang.org/x/sync/errgroup"
)

// AdaptiveParallelMergerResource is a Resource type which allows combining multiple Resources as if it was one.
// It reads underlying sources in parallel and can handle their size to be unknown.
type AdaptiveParallelMergerResource struct {
	resources []resource.ReadSeekCloseableResource

	// Start offset of each resource, filled in as far as a positional read has
	// needed it. offsets[i] is where resource i begins, so a complete table has
	// one entry more than there are resources.
	offsetsMutex sync.Mutex
	offsets      []int64
}

func NewAdaptiveParallelMergerResource(resources []resource.ReadSeekCloseableResource) *AdaptiveParallelMergerResource {
	return &AdaptiveParallelMergerResource{
		resources: resources,
		offsets:   []int64{0},
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

// knownSize answers from the resource itself, and reports false where only
// reading it can settle the length.
func knownSize(res resource.ReadSeekCloseableResource) (int64, bool, error) {
	sized, ok := res.(resource.Sized)
	if !ok {
		return 0, false, nil
	}

	size, err := sized.Size()
	switch {
	case err == nil:
		return size, true, nil
	case errors.Is(err, resource.ErrSizeNotExact):
		return 0, false, nil
	default:
		return 0, false, err
	}
}

// partSize is the length of resource i. A resource that knows it exactly answers
// for free; the rest have to be seeked to their end, which for an uncached
// segment means downloading it.
func (r *AdaptiveParallelMergerResourceReader) partSize(i int) (int64, error) {
	size, known, err := knownSize(r.resource.resources[i])
	if err != nil {
		return 0, fmt.Errorf("failed getting size from resource %d: %w", i, err)
	}
	if known {
		return size, nil
	}

	reader, err := r.reader(i)
	if err != nil {
		return 0, err
	}

	size, err = reader.Seek(0, io.SeekEnd)
	if err != nil {
		return 0, fmt.Errorf("failed seeking resource %d to end: %w", i, err)
	}

	return size, nil
}

// offsetsCovering extends the offset table until it holds the resource off falls
// in, or until every resource has been measured, and returns it. The table only
// ever grows, so the returned slice stays valid without the lock.
//
// It is built from exact sizes only - an estimate would send a read to the wrong
// byte - so a resource that does not know its own length is measured, which for
// an uncached segment costs a download, the same price a seek across it pays.
func (r *AdaptiveParallelMergerResource) offsetsCovering(off int64) ([]int64, error) {
	r.offsetsMutex.Lock()
	defer r.offsetsMutex.Unlock()

	for len(r.offsets) <= len(r.resources) && r.offsets[len(r.offsets)-1] <= off {
		first := len(r.offsets) - 1

		sizes, err := r.sizesFrom(first, off)
		if err != nil {
			return nil, err
		}

		for _, size := range sizes {
			r.offsets = append(r.offsets, r.offsets[len(r.offsets)-1]+size)
		}
	}

	return r.offsets, nil
}

// sizesFrom is the length of every resource from first up to the one the hints
// put off in. Measuring is a download, so the ones the offset needs run together
// rather than one after the other; where the hints came up short the caller asks
// again for the next batch.
func (r *AdaptiveParallelMergerResource) sizesFrom(first int, off int64) ([]int64, error) {
	last := first
	for estimate := r.offsets[first]; last < len(r.resources); {
		hint, err := r.resources[last].SizeHint()
		if err != nil {
			return nil, fmt.Errorf("failed getting size-hint from resource %d: %w", last, err)
		}

		estimate += hint
		last++
		if estimate > off {
			break
		}
	}

	sizes := make([]int64, last-first)
	var group errgroup.Group
	for i := first; i < last; i++ {
		size, known, err := knownSize(r.resources[i])
		if err != nil {
			return nil, fmt.Errorf("failed getting size from resource %d: %w", i, err)
		}
		if known {
			sizes[i-first] = size
			continue
		}

		group.Go(func() error {
			size, err := measure(r.resources[i])
			if err != nil {
				return fmt.Errorf("failed measuring resource %d: %w", i, err)
			}
			sizes[i-first] = size

			return nil
		})
	}
	if err := group.Wait(); err != nil {
		//nolint:wrapcheck // Already wrapped with the resource it came from
		return nil, err
	}

	return sizes, nil
}

// measure reads a resource to its end to settle its length, on a reader of its
// own so no position is shared with anything else.
func measure(res resource.ReadSeekCloseableResource) (int64, error) {
	reader, err := res.Open()
	if err != nil {
		return 0, fmt.Errorf("failed opening resource: %w", err)
	}
	defer reader.Close()

	size, err := reader.Seek(0, io.SeekEnd)
	if err != nil {
		return 0, fmt.Errorf("failed seeking resource to end: %w", err)
	}

	return size, nil
}

// ReadAt reads at an absolute offset without touching the position Read and Seek
// share, so concurrent calls proceed in parallel.
//
// It opens a reader per resource it touches rather than borrowing the ones the
// read head keeps, which would need reference counting to stay safe against
// closeBehind. That is one cache-file open per resource per call.
//
// The offset table gives every resource the request spans its own slice of p, so
// they are read at once rather than one after the other.
func (r *AdaptiveParallelMergerResourceReader) ReadAt(p []byte, off int64) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if off < 0 {
		return 0, resource.ErrInvalidSeek
	}

	end := off + int64(len(p))
	offsets, err := r.resource.offsetsCovering(end)
	if err != nil {
		return 0, err
	}

	type part struct {
		index int
		inner int64
		buf   []byte
		n     int
		err   error
	}

	var parts []part
	for pos := off; pos < end; {
		index := sort.Search(len(offsets), func(i int) bool { return offsets[i] > pos }) - 1
		if index < 0 || index >= len(r.resource.resources) || index >= len(offsets)-1 {
			break
		}

		partEnd := min(end, offsets[index+1])
		parts = append(parts, part{
			index: index,
			inner: pos - offsets[index],
			buf:   p[pos-off : partEnd-off],
		})
		pos = partEnd
	}

	switch len(parts) {
	case 0:
		return 0, io.EOF
	case 1:
		// A read inside one resource, which is the common one, stays on this goroutine
		n, err := readResourceAt(r.resource.resources[parts[0].index], parts[0].buf, parts[0].inner)
		if err != nil && !errors.Is(err, io.EOF) {
			return n, fmt.Errorf("failed reading resource %d at %d: %w", parts[0].index, parts[0].inner, err)
		}
		if n < len(p) {
			return n, io.EOF
		}

		return n, nil
	}

	var group errgroup.Group
	for i := range parts {
		group.Go(func() error {
			parts[i].n, parts[i].err = readResourceAt(r.resource.resources[parts[i].index], parts[i].buf, parts[i].inner)
			return nil
		})
	}
	//nolint:errcheck // Errors are kept per part, so a short one still yields its prefix
	group.Wait()

	// Only the contiguous prefix is readable: a part that came up short leaves a
	// hole, whatever the parts behind it returned
	totalRead := 0
	for i := range parts {
		totalRead += parts[i].n
		if parts[i].err != nil && !errors.Is(parts[i].err, io.EOF) {
			return totalRead, fmt.Errorf("failed reading resource %d at %d: %w", parts[i].index, parts[i].inner, parts[i].err)
		}
		if parts[i].n < len(parts[i].buf) {
			return totalRead, io.EOF
		}
	}

	if totalRead < len(p) {
		return totalRead, io.EOF
	}

	return totalRead, nil
}

// readResourceAt fills p from an offset inside one resource. A reader that
// answers positional reads is used as it is; the rest get a reader of their own,
// since seeking a shared one is what ReadAt exists to avoid.
func readResourceAt(res resource.ReadSeekCloseableResource, p []byte, off int64) (int, error) {
	reader, err := res.Open()
	if err != nil {
		return 0, fmt.Errorf("failed opening resource: %w", err)
	}
	defer reader.Close()

	if readerAt, ok := reader.(io.ReaderAt); ok {
		//nolint:wrapcheck // io.EOF has to reach the caller unwrapped
		return readerAt.ReadAt(p, off)
	}

	if _, err := reader.Seek(off, io.SeekStart); err != nil {
		return 0, fmt.Errorf("failed seeking to %d: %w", off, err)
	}

	n, err := io.ReadFull(reader, p)
	if errors.Is(err, io.ErrUnexpectedEOF) {
		err = io.EOF
	}

	//nolint:wrapcheck // io.EOF has to reach the caller unwrapped
	return n, err
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
	defer func() {
		// When already everything processed, dont start goroutine
		if processIndex >= len(responses) {
			defer r.mutex.Unlock()
			// group should have finished, in case it hasnt, wait
			_ = group.Wait()
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
						_, _ = reader.Seek(0, io.SeekStart)
					}
					processIndex++
				}
			}

			// Process remaining responses
			processResponses()

			// Wait for group & discard error, we can't raise it here anyways
			_ = group.Wait()

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
			for readCtx.Err() == nil {
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

	defer r.closeBehind()

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
