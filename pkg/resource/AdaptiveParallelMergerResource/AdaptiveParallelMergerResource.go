package AdaptiveParallelMergerResource

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
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
	resources  []resource.ReadSeekCloseableResource
	minReaders int
}

func NewAdaptiveParallelMergerResource(resources []resource.ReadSeekCloseableResource, minReaders int) *AdaptiveParallelMergerResource {
	return &AdaptiveParallelMergerResource{
		resources:  resources,
		minReaders: minReaders,
	}
}

type AdaptiveParallelMergerResourceReader struct {
	resource *AdaptiveParallelMergerResource
	readers  []io.ReadSeekCloser
	mutexes  []sync.Mutex
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

	// TODO: DEBUG
	f, err := os.OpenFile("../../.testfiles/test1-files/c77a091729ff4f02bc2c33da12cabe5c.part01.rar", os.O_RDONLY, os.ModePerm)
	file = f
	if err != nil {
		panic(err)
	}

	reader = &AdaptiveParallelMergerResourceReader{
		resource:        r,
		readers:         readers,
		mutexes:         make([]sync.Mutex, len(r.resources)),
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
	actualTotalRead := 0

	readResponseCh := make(chan *readResponse)
	defer close(readResponseCh)

	readResponses := make([]readResponse, 0, 1)

	readCtx, readCtxDone := context.WithCancel(context.Background())
	defer readCtxDone()
	group, _ := errgroup.WithContext(readCtx)

	// Which local index this reader and thus readResponse has
	readIndex := 0
	activeReaders := 0
	currMinReaders := r.resource.minReaders

	debug := 0

	for actualTotalRead < len(p) {
		// Start readers
		//for (actualTotalRead+expectedTotalRead < len(p) || currMinReaders-(len(readResponses)+activeReaders) > 0) && r.readerIndex < len(r.readers) {
		for actualTotalRead+expectedTotalRead < len(p) && r.readerIndex < len(r.readers) {
			requiredRead := len(p) - actualTotalRead - expectedTotalRead

			resourceSize, err := r.resource.resources[r.readerIndex].Size()
			if err != nil {
				return 0, err
			}

			resourceSizeLeft := int(resourceSize - r.readerByteIndex)
			// When size changed after first read, byteIndex can be behind size and the reader is thus exausted
			if resourceSizeLeft < 0 {
				r.readerIndex++
				r.readerByteIndex = 0
				continue
			}

			// Expect either full resource or part up to whatever is expected to be needed at this point
			expectedRead := resourceSizeLeft
			if requiredRead < expectedRead {
				expectedRead = requiredRead
			}

			localReadIndex := readIndex
			readerIndex := r.readerIndex

			activeReaders++
			//go r.readWithReader(r.readerIndex, readResponseCh, expectedRead)

			fmt.Printf("%p\t\t[%d] %d \tRead %d bytes\n", r, localReadIndex, readerIndex, expectedRead)
			group.Go(func() (err error) {
				r.readWithReader(readCtx, localReadIndex, readerIndex, readResponseCh, expectedRead)
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

		if debug == 0 {
			fmt.Printf("ActiveReaders=%d\tfor %.2f MiB\n", activeReaders, float32(len(p))/1024/1024)
			debug++
		}

		// Wait for read responses
		response := <-readResponseCh
		activeReaders--

		if response.err != nil && response.err != io.EOF {
			return 0, response.err
		}

		readResponses = append(readResponses, *response)

		expectedRead := len(response.buffer)
		actualRead := response.n

		if expectedRead != actualRead {
			fmt.Printf("%p\t\t[%d] %d \tExpected %d bytes\t!= Actual %d bytes \t%s\n", r, response.index, response.readerIndex, expectedRead, actualRead, response.err)
		} else {
			fmt.Printf("%p\t\t[%d] %d \tExpected %d bytes\t== Actual %d bytes \t%s\n", r, response.index, response.readerIndex, expectedRead, actualRead, response.err)
		}
		if expectedRead != actualRead {
			// Increase minReaders
			currMinReaders++

			// In case reader is still current reader and hit EOF, we need to advance
			if response.readerIndex == r.readerIndex && response.err == io.EOF {
				r.readerIndex++
				r.readerByteIndex = 0
			}
		}

		expectedTotalRead -= expectedRead
		actualTotalRead += actualRead

		// If no more readers can be started and all are done, consider operation finished
		if activeReaders == 0 && r.readerIndex >= len(r.readers) {
			break
		}
	}
	// Cancel whatever is left
	readCtxDone()

	return finalizeRead(r, p, readResponses)
}

type readerCopyAction struct {
	expected int64
	actual   int64
}

func finalizeRead(r *AdaptiveParallelMergerResourceReader, p []byte, readResponses []readResponse) (int, error) {
	// Sort
	slices.SortFunc(readResponses, func(a, b readResponse) int {
		return a.index - b.index
	})

	// Copy data to p
	totalRead := 0
	perReader := make([]readerCopyAction, len(readResponses))
	for _, response := range readResponses {
		if totalRead < len(p) {
			// Copy offset into p, and max what we have as data
			copied := copy(p[totalRead:], response.buffer[:response.n])
			totalRead += copied

			// TODO: DEBUG
			checkErr := r.checkData(response.buffer[:response.n])
			if checkErr != nil {
				fmt.Println("Error")
			}

			r.index += int64(copied)
			r.readerIndex = response.readerIndex

			perReader[r.readerIndex-readResponses[0].readerIndex].actual += int64(copied)
			perReader[r.readerIndex-readResponses[0].readerIndex].expected += int64(response.n)
		}
	}
	for readerIndexOffset, copyAction := range perReader {
		readerIndex := readerIndexOffset + readResponses[0].readerIndex
		//readerIndex := readerIndexOffset + len(readResponses) + 1
		if copyAction.actual != copyAction.expected {
			fmt.Printf("Read Reader %d\ttoo far, seeking to %d\n", readerIndex, copyAction.actual)
			r.readers[readerIndex].Seek(copyAction.actual, io.SeekStart)

			r.readerByteIndex = copyAction.actual
		}
	}

	if r.readerIndex == len(r.readers)-1 && len(readResponses) > 0 {
		return totalRead, readResponses[len(readResponses)-1].err
	}
	return totalRead, nil
}

func (r *AdaptiveParallelMergerResourceReader) readWithReader(ctx context.Context, index, readerIndex int, responseCh chan *readResponse, readAmount int) {
	// TODO: Check if reserving buffer should be pooled to save memory alloc overhead
	buf := make([]byte, readAmount)
	// Read from underlying reader into slice
	n, err := r.readers[readerIndex].Read(buf)
	select {
	case <-ctx.Done():
		return
	default:
	}
	responseCh <- &readResponse{
		index:       index,
		readerIndex: readerIndex,
		buffer:      buf,
		n:           n,
		err:         err,
	}
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

// TODO: ###### DEBUG ######
var file io.Reader

func (r *AdaptiveParallelMergerResourceReader) checkData(buffer []byte) error {
	a := buffer

	b := make([]byte, len(a))
	n, _ := file.Read(b)

	if n != len(a) {
		return errors.New("length mismatch")
	}

	if err := compare(a, b); err != nil {
		return errors.New("Comparison failed: " + err.Error())
	}
	return nil
}

func compare(a, b []byte) error {
	if len(a) != len(b) {
		return errors.New("length mismatch")
	}
	for i := 0; i < len(a); i++ {
		if a[i] != b[i] {
			return fmt.Errorf("mismatch at %d: %s != %s", i, a[i:i+16], b[i:i+16])
		}
	}
	return nil
}

// TODO: ###### DEBUG ######
