package CircularBuffer

import (
	"errors"
	"io"
	"sync"
)

var (
	ErrSeekInvalid     = errors.New("invalid value for whence")
	ErrSeekOutOfBounds = errors.New("seek position out of bounds")
	ErrResizeTooSmall  = errors.New("new capacity cannot be less than the current size")
)

type CircularBuffer[T any] struct {
	sync.Mutex
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
	cb.canRead = sync.NewCond(&cb.Mutex)
	cb.canWrite = sync.NewCond(&cb.Mutex)
	return cb
}

func (cb *CircularBuffer[T]) GetSize() int {
	return cb.size
}

func (cb *CircularBuffer[T]) GetMaxCapacity() int {
	return cb.maxCapacity
}

func (cb *CircularBuffer[T]) GetReadPos() int {
	return cb.readPos
}

func (cb *CircularBuffer[T]) GetWritePos() int {
	return cb.writePos
}

func (cb *CircularBuffer[T]) GetCurrCapacity() int {
	return len(cb.buffer)
}

// Get currently free space in buffer
func (cb *CircularBuffer[T]) GetCurrFree() int {
	return cb.GetCurrCapacity() - cb.size
}

// Get currently theoretical total free space in buffer
func (cb *CircularBuffer[T]) GetCurrTotalFree() int {
	return cb.GetMaxCapacity() - cb.size
}

// SetWriteBlocking configures whether write operations should be blocking when the buffer is full.
func (cb *CircularBuffer[T]) SetWriteBlocking(blocking bool) {
	cb.Lock()
	defer cb.Unlock()
	cb.blockWrite = blocking
}

// SetReadBlocking configures whether read operations should be blocking when the buffer is empty.
func (cb *CircularBuffer[T]) SetReadBlocking(blocking bool) {
	cb.Lock()
	defer cb.Unlock()
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

func (cb *CircularBuffer[T]) write(data []T, blocking bool) (int, error) {
	cb.Lock()
	defer cb.Unlock()

	totalWritten := 0

	for len(data) > 0 {
		if cb.GetCurrTotalFree() == 0 {
			if blocking {
				cb.canWrite.Wait()
				if cb.GetCurrTotalFree() == 0 {
					return totalWritten, errors.New("buffer was still full after wakeup")
				}
			} else {
				return totalWritten, errors.New("buffer full, blocking is disabled")
			}
		}

		// Try resize if too small for data
		if cb.GetCurrFree() < len(data) {
			newSize := cb.GetCurrCapacity() + len(data) - cb.GetCurrFree()
			if newSize > cb.maxCapacity {
				newSize = cb.maxCapacity
			}
			if newSize != cb.GetCurrCapacity() {
				cb.resizeWithLock(newSize)
			}
		}

		// Determine how much can be written in this iteration
		n := len(data)
		free := cb.GetCurrFree()
		if n > free {
			n = free
		}

		// In case write head is after last byte on the right
		cb.writePos = cb.writePos % cb.GetCurrCapacity()
		writeEnd := (cb.writePos + n) % cb.GetCurrCapacity()

		if cb.writePos < writeEnd || writeEnd == 0 {
			// We can write in one go
			copy(cb.buffer[cb.writePos:], data[:n])
		} else {
			// We need to write in a wrap-around way
			part1 := cb.GetCurrCapacity() - cb.writePos
			copy(cb.buffer[cb.writePos:], data[:part1])
			copy(cb.buffer[:writeEnd], data[part1:])
		}

		// Ensure, write head is after last byte on the right
		cb.writePos = (cb.writePos + n)
		cb.size += n

		cb.canRead.Signal() // Signal that there's data available for reading
		data = data[n:]
		totalWritten += n
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
	cb.Lock()
	defer cb.Unlock()

	n := len(p)
	totalRead := 0

	//for totalRead < n {
	if cb.size == 0 {
		if blocking {
			cb.canRead.Wait()
			if cb.size == 0 {
				return totalRead, errors.New("no data after wakeup")
			}
		} else {
			return totalRead, io.EOF
		}
	}

	// Calculate how much we can read in this iteration
	end := cb.readPos + (n - totalRead)
	if end > cb.GetCurrCapacity() {
		end = cb.GetCurrCapacity()
	}
	available := end - cb.readPos

	if available > cb.size {
		available = cb.size
	}

	// Copy data into p
	copy(p[totalRead:], cb.buffer[cb.readPos:end])
	cb.readPos = cb.readPos % cb.GetCurrCapacity()
	cb.readPos = (cb.readPos + available) % cb.GetCurrCapacity()
	cb.size -= available
	totalRead += available

	cb.canWrite.Signal() // Signal that there's space available for writing
	//}

	return totalRead, nil
}

// Flushes the buffer, freeing all data and waking up all goroutines waiting for read or write operations.
func (cb *CircularBuffer[T]) Flush() (err error) {
	cb.Lock()
	defer cb.Unlock()

	if _, err = cb.seekWithLock(0, io.SeekEnd); err != nil {
		return
	}
	if err = cb.resizeWithLock(0); err != nil {
		return
	}

	cb.canRead.Broadcast() // Signal all waiting goroutines
	cb.canWrite.Broadcast()

	return
}

// ResizeMaxCapacity adjusts the buffers max capacity to the new specified size, if current allocated capacity is higher, tries to shrink it too.
func (cb *CircularBuffer[T]) ResizeMaxCapacity(newMaxCapacity int) error {
	cb.Lock()
	defer cb.Unlock()

	if newMaxCapacity < cb.GetCurrCapacity() {
		if err := cb.resizeWithLock(newMaxCapacity); err != nil {
			return err
		}
	}

	cb.maxCapacity = newMaxCapacity

	return nil
}

// Resize adjusts the buffer's actual capacity to the new specified size if it is not less than the current amount of data.
func (cb *CircularBuffer[T]) Resize(newCapacity int) error {
	cb.Lock()
	defer cb.Unlock()

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

// Seek moves the read head to new position if possible
func (cb *CircularBuffer[T]) Seek(offset int64, whence int) (int64, error) {
	cb.Lock()
	defer cb.Unlock()

	return cb.seekWithLock(offset, whence)
}

func (cb *CircularBuffer[T]) seekWithLock(offset int64, whence int) (int64, error) {
	var newPos int

	switch whence {
	case io.SeekStart:
		newPos = int(offset)
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

	cb.readPos = newPos

	cb.size = cb.writePos - cb.readPos
	if cb.size < 0 {
		cb.size += cb.GetCurrCapacity()
	}

	return int64(cb.readPos), nil
}

// ExposeWriteSpace returns a slice that represents the currently free continuous space in the buffer.
// It will lock the buffer and CommitWrite MUST be called after using ExposeWriteSpace!
// Note: The exposed space is meant for advanced users who ensure that the space is correctly filled
// and the writePos is adjusted after writing.
func (cb *CircularBuffer[T]) ExposeWriteSpace() []T {
	cb.Lock()

	if cb.size == cb.GetCurrCapacity() {
		// Buffer is full, no space to expose
		return nil
	}

	// Determine the start and end of free space
	start := cb.writePos % cb.GetCurrCapacity()
	end := start
	if start >= cb.readPos {
		// Free space could be at the end plus possible wrap-around
		end = cb.GetCurrCapacity()
	} else {
		// Free space is simply from writePos to readPos
		end = cb.readPos
	}

	// Expose the free space as a slice
	return cb.buffer[start:end]
}

// CommitWrite MUST be called after using ExposeWriteSpace to update the write position and release the lock.
// It takes the number of elements that have been written into the exposed space.
func (cb *CircularBuffer[T]) CommitWrite(written int) error {
	defer cb.Unlock()
	if cb.TryLock() {
		return errors.New("Mutex wasnt locked, but expected it to be from Expose!")
	}

	if written > cb.GetCurrFree() {
		return errors.New("committed more space than available")
	}

	cb.writePos = cb.writePos % cb.GetCurrCapacity()
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
	cb.Lock()

	if cb.size == 0 {
		// Buffer is empty, no data to expose
		return nil
	}

	// Determine the start and end of readable data
	start := cb.readPos % cb.GetCurrCapacity()
	end := start
	if start <= cb.writePos {
		// Readable data is in a single segment
		end = cb.writePos
	} else {
		// Readable data wraps around at the end of the buffer
		end = cb.GetCurrCapacity()
	}

	// Expose the readable space as a slice
	return cb.buffer[start:end]
}

// CommitRead MUST be called after using ExposeReadSpace to update the read position and release the lock.
// It takes the number of elements that have been read from the exposed space.
func (cb *CircularBuffer[T]) CommitRead(read int) error {
	defer cb.Unlock()
	if cb.TryLock() {
		return errors.New("Mutex wasnt locked, but expected it to be from Expose!")
	}

	if read > cb.size {
		return errors.New("committed more space than available")
	}

	cb.readPos = cb.readPos % cb.GetCurrCapacity()
	cb.readPos = (cb.readPos + read) % cb.GetCurrCapacity()
	cb.size -= read
	cb.canWrite.Signal() // Signal that there's space available for writing
	return nil
}
