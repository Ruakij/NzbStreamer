package circularbuffer

import (
	"errors"
	"fmt"
	"io"
	"sync"
)

var (
	ErrSeekInvalid     = errors.New("invalid value for whence")
	ErrSeekOutOfBounds = errors.New("seek position out of bounds")
	ErrResizeTooSmall  = errors.New("new capacity cannot be less than the current size")
)

type CircularBuffer[T any] struct {
	mu       sync.RWMutex
	canRead  *sync.Cond
	canWrite *sync.Cond
	// Underlying data-store
	buffer []T
	// Currently used buffer
	size int
	// Maximum capacity data-store can grow to
	maxCapacity int
	readPos     int
	writePos    int
	blockRead   bool
	blockWrite  bool
}

// NewCircularBuffer initializes and returns a new CircularBuffer with the specified initial and maximum sizes.
func NewCircularBuffer[T any](initialSize, maxCapacity int) *CircularBuffer[T] {
	cb := &CircularBuffer[T]{
		buffer:      make([]T, initialSize),
		maxCapacity: maxCapacity,
		readPos:     0,
		writePos:    0,
		size:        0,
	}
	cb.canRead = sync.NewCond(&cb.mu)
	cb.canWrite = sync.NewCond(&cb.mu)
	return cb
}

func (cb *CircularBuffer[T]) GetSize() int {
	cb.mu.RLock()
	defer cb.mu.RUnlock()
	return cb.size
}

func (cb *CircularBuffer[T]) GetMaxCapacity() int {
	cb.mu.RLock()
	defer cb.mu.RUnlock()
	return cb.maxCapacity
}

func (cb *CircularBuffer[T]) GetReadPos() int {
	cb.mu.RLock()
	defer cb.mu.RUnlock()
	return cb.readPos
}

func (cb *CircularBuffer[T]) GetWritePos() int {
	cb.mu.RLock()
	defer cb.mu.RUnlock()
	return cb.writePos
}

func (cb *CircularBuffer[T]) GetCurrCapacity() int {
	cb.mu.RLock()
	defer cb.mu.RUnlock()
	return cb.getCurrCapacity()
}

func (cb *CircularBuffer[T]) getCurrCapacity() int {
	return len(cb.buffer)
}

// Get currently free space in buffer
func (cb *CircularBuffer[T]) GetCurrFree() int {
	cb.mu.RLock()
	defer cb.mu.RUnlock()
	return cb.getCurrFree()
}

func (cb *CircularBuffer[T]) getCurrFree() int {
	return cb.getCurrCapacity() - cb.size
}

// Get currently theoretical total free space in buffer
func (cb *CircularBuffer[T]) GetCurrTotalFree() int {
	cb.mu.RLock()
	defer cb.mu.RUnlock()
	return cb.getCurrTotalFree()
}

func (cb *CircularBuffer[T]) getCurrTotalFree() int {
	return cb.maxCapacity - cb.size
}

// SetWriteBlocking configures whether write operations should be blocking when the buffer is full.
func (cb *CircularBuffer[T]) SetWriteBlocking(blocking bool) {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	cb.blockWrite = blocking
}

// SetReadBlocking configures whether read operations should be blocking when the buffer is empty.
func (cb *CircularBuffer[T]) SetReadBlocking(blocking bool) {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	cb.blockRead = blocking
}

// Write writes data to the buffer with the currently configured blocking behavior.
func (cb *CircularBuffer[T]) Write(p []T) (int, error) {
	return cb.write(p, cb.blockWrite)
}

// WriteBlocking writes data to the buffer in a blocking manner, waiting if the buffer is full.
func (cb *CircularBuffer[T]) WriteBlocking(p []T) (int, error) {
	return cb.write(p, true)
}

// WriteNonBlocking writes data to the buffer without blocking, returning an error if the buffer is full.
func (cb *CircularBuffer[T]) WriteNonBlocking(p []T) (int, error) {
	return cb.write(p, false)
}

var (
	ErrBufferFullAfterWakeup      = errors.New("buffer was still full after wakeup")
	ErrBufferFullBlockingDisabled = errors.New("buffer full, blocking is disabled")
)

func (cb *CircularBuffer[T]) write(data []T, blocking bool) (int, error) {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	totalWritten := 0

	for len(data) > 0 {
		if cb.getCurrTotalFree() == 0 {
			if blocking {
				cb.canWrite.Wait()
				if cb.getCurrTotalFree() == 0 {
					return totalWritten, ErrBufferFullAfterWakeup
				}
			} else {
				return totalWritten, ErrBufferFullBlockingDisabled
			}
		}

		// Try resize if too small for data
		if cb.getCurrFree() < len(data) {
			newSize := cb.getCurrCapacity() + len(data) - cb.getCurrFree()
			if newSize > cb.maxCapacity {
				newSize = cb.maxCapacity
			}
			if newSize != cb.getCurrCapacity() {
				err := cb.resizeWithLock(newSize)
				if err != nil {
					return 0, fmt.Errorf("failed resizing: %w", err)
				}
			}
		}

		writeSpace := cb.exposeWriteSpace()
		n := copy(writeSpace, data)
		err := cb.commitWrite(n)

		cb.canRead.Signal() // Signal that there's data available for reading
		data = data[n:]
		totalWritten += n

		if err != nil {
			return totalWritten, err
		}
	}

	return totalWritten, nil
}

// Read reads data from the buffer with the currently configured blocking behavior.
func (cb *CircularBuffer[T]) Read(p []T) (int, error) {
	return cb.read(p, cb.blockRead)
}

// ReadBlocking reads data from the buffer in a blocking manner, waiting if the buffer is empty.
func (cb *CircularBuffer[T]) ReadBlocking(p []T) (int, error) {
	return cb.read(p, true)
}

// ReadNonBlocking reads data from the buffer without blocking, returning an error if there is not enough data.
func (cb *CircularBuffer[T]) ReadNonBlocking(p []T) (int, error) {
	return cb.read(p, false)
}

func (cb *CircularBuffer[T]) read(p []T, blocking bool) (int, error) {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	if cb.size == 0 {
		if blocking {
			for cb.size == 0 {
				cb.canRead.Wait()
			}
		} else {
			return 0, io.EOF
		}
	}

	readSpace := cb.exposeReadSpace()
	n := copy(p, readSpace)
	err := cb.commitRead(n)

	cb.canWrite.Signal() // Signal that there's space available for writing

	return n, err
}

// Flushes the buffer, freeing all data and waking up all goroutines waiting for read or write operations.
func (cb *CircularBuffer[T]) Flush() error {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	if _, err := cb.seekWithLock(0, io.SeekEnd); err != nil {
		return err
	}
	if err := cb.resizeWithLock(0); err != nil {
		return err
	}

	return cb.clear()
}

// Clears the buffer, waking up all goroutines waiting for read or write operations.
// Buffer size is kept
func (cb *CircularBuffer[T]) Clear() error {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	return cb.clear()
}

func (cb *CircularBuffer[T]) clear() error {
	cb.size = 0
	cb.readPos = 0
	cb.writePos = 0

	cb.canRead.Broadcast() // Signal all waiting goroutines
	cb.canWrite.Broadcast()

	return nil
}

// ResizeMaxCapacity adjusts the buffers max capacity to the new specified size, if current allocated capacity is higher, tries to shrink it too.
func (cb *CircularBuffer[T]) ResizeMaxCapacity(newMaxCapacity int) error {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	if newMaxCapacity < cb.getCurrCapacity() {
		if err := cb.resizeWithLock(newMaxCapacity); err != nil {
			return err
		}
	}

	cb.maxCapacity = newMaxCapacity

	return nil
}

// Resize adjusts the buffer's actual capacity to the new specified size if it is not less than the current amount of data.
func (cb *CircularBuffer[T]) Resize(newCapacity int) error {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	return cb.resizeWithLock(newCapacity)
}

func (cb *CircularBuffer[T]) resizeWithLock(newCapacity int) error {
	if newCapacity < cb.size {
		return ErrResizeTooSmall
	}

	// New slice for continuous memory allocation with better cache locality
	newBuffer := make([]T, newCapacity)

	if cb.readPos <= cb.writePos {
		// If the buffer is not wrapped around
		copy(newBuffer, cb.buffer[cb.readPos:cb.writePos])
	} else {
		// If the buffer is wrapped around
		n := copy(newBuffer, cb.buffer[cb.readPos:len(cb.buffer)])
		copy(newBuffer[n:], cb.buffer[:cb.writePos])
	}

	cb.buffer = newBuffer
	cb.readPos = 0
	cb.writePos = cb.size

	return nil
}

// Seek moves the read head to new position.
//
// SeekStart will seek relative to the write position, NOT the current readPosition.
// SeekCurrent will seek relative to the read position.
// SeekEnd will seek relative to the end of current capacity
func (cb *CircularBuffer[T]) Seek(offset int64, whence int) (int64, error) {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	return cb.seekWithLock(offset, whence)
}

func (cb *CircularBuffer[T]) seekWithLock(offset int64, whence int) (int64, error) {
	var newPos int

	switch whence {
	case io.SeekStart:
		newPos = cb.writePos + int(offset)
	case io.SeekCurrent:
		newPos = cb.readPos + int(offset)
	case io.SeekEnd:
		newPos = cb.size + int(offset)
	default:
		return 0, ErrSeekInvalid
	}

	if newPos < 0 || newPos > cb.size {
		return 0, ErrSeekOutOfBounds
	}

	newPos %= cb.getCurrCapacity()

	cb.readPos = newPos

	cb.size = cb.writePos - cb.readPos
	if cb.size < 0 {
		cb.size += cb.getCurrCapacity()
	}

	return int64(cb.readPos), nil
}

// ExposeWriteSpace returns a slice that represents the currently free continuous space in the buffer.
// It will lock the buffer and CommitWrite MUST be called after using ExposeWriteSpace!
// Note: The exposed space is meant for advanced users who ensure that the space is correctly filled
// and the writePos is adjusted after writing.
func (cb *CircularBuffer[T]) ExposeWriteSpace() []T {
	cb.mu.Lock()

	return cb.exposeWriteSpace()
}

func (cb *CircularBuffer[T]) exposeWriteSpace() []T {
	if cb.size == cb.getCurrCapacity() {
		// Buffer is full, no space to expose
		return nil
	}

	// Determine the start and end of free space
	start := cb.writePos % cb.getCurrCapacity()
	var end int
	if start >= cb.readPos {
		// Free space could be at the end plus possible wrap-around
		end = cb.getCurrCapacity()
	} else {
		// Free space is simply from writePos to readPos
		end = cb.readPos
	}

	// Expose the free space as a slice
	return cb.buffer[start:end]
}

var (
	ErrMutexNotLockedButRequredOnExpose = errors.New("mutex wasnt locked, but is expected from Expose")
	ErrCommtedMoreThanAvailable         = errors.New("committed more space than available")
)

// CommitWrite MUST be called after using ExposeWriteSpace to update the write position and release the lock.
// It takes the number of elements that have been written into the exposed space.
func (cb *CircularBuffer[T]) CommitWrite(written int) error {
	defer cb.mu.Unlock()
	if cb.mu.TryLock() {
		return ErrMutexNotLockedButRequredOnExpose
	}

	return cb.commitWrite(written)
}

func (cb *CircularBuffer[T]) commitWrite(written int) error {
	if written > cb.getCurrFree() {
		return ErrCommtedMoreThanAvailable
	}

	cb.writePos %= cb.getCurrCapacity()
	cb.writePos = (cb.writePos + written)
	cb.size += written
	cb.canRead.Signal() // Signal that there's data available for reading
	return nil
}

// ExposeReadSpace returns a slice that represents the currently readable continuous data in the buffer.
// It will lock the buffer and CommitRead MUST be called after using ExposeReadSpace!
// Note: The exposed space is meant for advanced users who ensure that the space is correctly read
// and the readPos is adjusted after reading.
func (cb *CircularBuffer[T]) ExposeReadSpace() []T {
	cb.mu.Lock()
	return cb.exposeReadSpace()
}

func (cb *CircularBuffer[T]) exposeReadSpace() []T {
	if cb.size == 0 {
		// Buffer is empty, no data to expose
		return []T{}
	}

	// Determine the start and end of readable data
	start := cb.readPos
	end := (start + cb.size) % cb.getCurrCapacity()
	if start <= end {
		// Readable data is in a single segment
		end = cb.writePos
	} else {
		// Readable data wraps around at the end of the buffer
		end = cb.getCurrCapacity()
	}

	// Expose the readable space as a slice
	return cb.buffer[start:end]
}

// CommitRead MUST be called after using ExposeReadSpace to update the read position and release the lock.
// It takes the number of elements that have been read from the exposed space.
func (cb *CircularBuffer[T]) CommitRead(read int) error {
	defer cb.mu.Unlock()
	if cb.mu.TryLock() {
		return ErrMutexNotLockedButRequredOnExpose
	}

	return cb.commitRead(read)
}

func (cb *CircularBuffer[T]) commitRead(read int) error {
	if read > cb.size {
		return ErrCommtedMoreThanAvailable
	}

	cb.readPos %= cb.getCurrCapacity()
	cb.readPos = (cb.readPos + read)
	cb.size -= read
	cb.canWrite.Signal() // Signal that there's space available for writing
	return nil
}
