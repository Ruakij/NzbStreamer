package CircularBuffer

import (
	"errors"
	"io"
	"testing"
	"time"
)

func TestWrite(t *testing.T) {
	cb := NewCircularBuffer[byte](0, 10)

	data := []byte("hello")
	n, err := cb.Write(data)
	if err != nil {
		t.Errorf("unexpected error writing data: %v", err)
	}

	if n != len(data) {
		t.Errorf("expected to write %d bytes, wrote %d", len(data), n)
	}
}

func TestWrappedWrite(t *testing.T) {
	cb := NewCircularBuffer[byte](0, 6)

	// Normal write
	data := []byte("hello")
	n, err := cb.Write(data)
	if err != nil {
		t.Errorf("unexpected error writing data: %v", err)
	}
	if n != len(data) {
		t.Errorf("expected to write %d bytes, wrote %d", len(data), n)
	}

	// Normal read
	readData := make([]byte, len(data))
	n, err = cb.Read(readData)
	if err != nil {
		t.Errorf("unexpected error reading data: %v", err)
	}
	if n != len(data) {
		t.Errorf("expected to read %d bytes, read %d", len(data), n)
	}

	// Wrapped write
	n, err = cb.Write(data)
	if err != nil {
		t.Errorf("unexpected error writing data: %v", err)
	}
	if n != len(data) {
		t.Errorf("expected to write %d bytes, wrote %d", len(data), n)
	}

	// Wrapped read
	readData = make([]byte, len(data))
	n, err = cb.Read(readData)
	if err != nil {
		t.Errorf("unexpected error reading data: %v", err)
	}
	if n != len(data) {
		t.Errorf("expected to read %d bytes, read %d", len(data), n)
	}
}

func TestWrappedResizeWrite(t *testing.T) {
	cb := NewCircularBuffer[byte](6, 6)

	// Normal write
	data := []byte("hello")
	n, err := cb.Write(data)
	if err != nil {
		t.Errorf("unexpected error writing data: %v", err)
	}
	if n != len(data) {
		t.Errorf("expected to write %d bytes, wrote %d", len(data), n)
	}

	// Normal read
	readData := make([]byte, len(data))
	n, err = cb.Read(readData)
	if err != nil {
		t.Errorf("unexpected error reading data: %v", err)
	}
	if n != len(data) {
		t.Errorf("expected to read %d bytes, read %d", len(data), n)
	}

	// Wrapped resizing write
	n, err = cb.Write(data)
	if err != nil {
		t.Errorf("unexpected error writing data: %v", err)
	}
	if n != len(data) {
		t.Errorf("expected to write %d bytes, wrote %d", len(data), n)
	}

	// Wrapped read
	readData = make([]byte, len(data))
	n, err = cb.Read(readData)
	if err != nil {
		t.Errorf("unexpected error reading data: %v", err)
	}
	if n != len(data) {
		t.Errorf("expected to read %d bytes, read %d", len(data), n)
	}
}

func TestRead(t *testing.T) {
	cb := NewCircularBuffer[byte](2, 10)

	data := []byte("hello")
	cb.Write(data)

	readData := make([]byte, len(data))
	n, err := cb.Read(readData)
	if err != nil {
		t.Errorf("unexpected error reading data: %v", err)
	}

	if n != len(data) {
		t.Errorf("expected to read %d bytes, read %d", len(data), n)
	}

	if string(readData) != "hello" {
		t.Errorf("expected data 'hello', got '%s'", string(readData))
	}
}

func TestBlockingWrite(t *testing.T) {
	cb := NewCircularBuffer[byte](0, 3)
	cb.SetWriteBlocking(true)

	done := make(chan struct{})

	go func() {
		data := []byte("abc")
		cb.Write(data)

		// Attempt to write to a full buffer
		cb.Write([]byte("d"))
		close(done)
	}()

	// Ensure that the write blocks until space is available
	time.Sleep(10 * time.Millisecond)

	select {
	case <-done:
		t.Errorf("blocking write should have been blocked, but completed prematurely")
	default:
	}
}

func TestBlockingRead(t *testing.T) {
	cb := NewCircularBuffer[byte](3, 3)
	cb.SetReadBlocking(true)

	done := make(chan struct{})

	go func() {
		readData := make([]byte, 1)

		// Attempt to read from an empty buffer
		cb.Read(readData)
		close(done)
	}()

	// Ensure that the read blocks until data is available
	time.Sleep(10 * time.Millisecond)

	select {
	case <-done:
		t.Errorf("blocking read should have been blocked, but completed prematurely")
	default:
	}
}

func TestNonBlockingWriteFullBuffer(t *testing.T) {
	cb := NewCircularBuffer[byte](3, 3)

	data := []byte("abc")
	cb.Write(data)

	n, err := cb.WriteNonBlocking([]byte("d"))
	if err == nil || err.Error() != "buffer full, blocking is disabled" {
		t.Errorf("expected error 'buffer full, blocking is disabled', got: %v", err)
	}
	if n != 0 {
		t.Errorf("expected to write 0, got: %v", n)
	}
}

func TestNonBlockingReadEmptyBuffer(t *testing.T) {
	cb := NewCircularBuffer[byte](3, 3)

	readData := make([]byte, 1)
	_, err := cb.ReadNonBlocking(readData)
	if err == nil || err != io.EOF {
		t.Errorf("expected error '%v', got: %v", io.EOF, err)
	}
}

func TestResizeSuccess(t *testing.T) {
	cb := NewCircularBuffer[byte](5, 5)

	data := []byte("hello")
	cb.Write(data)

	err := cb.Resize(10)
	if err != nil {
		t.Errorf("unexpected error resizing buffer: %v", err)
	}
}

func TestResizeTooSmall(t *testing.T) {
	cb := NewCircularBuffer[byte](5, 5)

	data := []byte("hello")
	cb.Write(data)

	err := cb.Resize(3)
	if !errors.Is(err, ErrResizeTooSmall) {
		t.Errorf("expected error 'ErrResizeTooSmall', got: %v", err)
	}
}

func TestFlush(t *testing.T) {
	cb := NewCircularBuffer[byte](5, 5)

	data := []byte("hello")
	cb.Write(data)

	err := cb.Flush()
	if err != nil {
		t.Errorf("unexpected error flushing buffer: %v", err)
	}

	readData := make([]byte, len(data))
	n, err := cb.Read(readData)
	if n != 0 {
		t.Errorf("expected to read 0 bytes after flush, read %d", n)
	}
}

func TestSeekStart(t *testing.T) {
	cb := NewCircularBuffer[byte](10, 10)
	data := []byte("abcdefghij")
	cb.Write(data)

	pos, err := cb.Seek(5, io.SeekStart)
	if err != nil {
		t.Errorf("unexpected error seeking: %v", err)
	}

	if pos != 5 {
		t.Errorf("expected position 5, got %d", pos)
	}
}

func TestSeekCurrent(t *testing.T) {
	cb := NewCircularBuffer[byte](10, 10)
	data := []byte("abcdefghij")
	cb.Write(data)

	cb.Seek(5, io.SeekStart)
	pos, err := cb.Seek(-2, io.SeekCurrent)
	if err != nil {
		t.Errorf("unexpected error seeking: %v", err)
	}

	if pos != 3 {
		t.Errorf("expected position 3, got %d", pos)
	}
}

func TestSeekEnd(t *testing.T) {
	cb := NewCircularBuffer[byte](10, 10)
	data := []byte("abcdefghij")
	cb.Write(data)

	pos, err := cb.Seek(-2, io.SeekEnd)
	if err != nil {
		t.Errorf("unexpected error seeking: %v", err)
	}

	if pos != 8 {
		t.Errorf("expected position 8, got %d", pos)
	}
}

func TestSeekInvalid(t *testing.T) {
	cb := NewCircularBuffer[byte](10, 10)
	data := []byte("abcdefghij")
	cb.Write(data)

	_, err := cb.Seek(100, -1)
	if !errors.Is(err, ErrSeekInvalid) {
		t.Errorf("expected error '%s', got: %v", ErrSeekInvalid, err)
	}
}

func TestSeekOutOfBounds(t *testing.T) {
	cb := NewCircularBuffer[byte](10, 10)
	data := []byte("abcdefghij")
	cb.Write(data)

	_, err := cb.Seek(100, io.SeekStart)
	if !errors.Is(err, ErrSeekOutOfBounds) {
		t.Errorf("expected error '%s', got: %v", ErrSeekOutOfBounds, err)
	}
}

func TestExposeWriteSpaceAndCommitWrite(t *testing.T) {
	cb := NewCircularBuffer[int](5, 10)

	// Test exposing write space and committing write
	writeSpace := cb.ExposeWriteSpace()
	if len(writeSpace) != 5 {
		t.Fatalf("Expected write space length 5, got %d", len(writeSpace))
	}

	// Simulate writing data
	for i := 0; i < len(writeSpace); i++ {
		writeSpace[i] = i
	}

	// Correct commit
	if err := cb.CommitWrite(len(writeSpace)); err != nil {
		t.Fatalf("Unexpected error during CommitWrite: %v", err)
	}

	// Try exposing more space than available, should return slice up to capacity
	writeSpace = cb.ExposeWriteSpace()
	if writeSpace != nil {
		t.Fatalf("Expected nil write space as the buffer is full, got %d", len(writeSpace))
	}

	// Wrong commit: Committing more than available should return an error
	if err := cb.CommitWrite(1); err == nil {
		t.Fatal("Expected error when committing more space than available, got nil")
	}
}

func TestExposeReadSpaceAndCommitRead(t *testing.T) {
	cb := NewCircularBuffer[int](5, 10)

	// Prepare buffer with initial data
	data := []int{1, 2, 3, 4, 5}
	n, err := cb.Write(data)
	if err != nil || n != len(data) {
		t.Fatalf("Unexpected error during Write: %v or written bytes %d", err, n)
	}

	// Correct read expose and commit
	readSpace := cb.ExposeReadSpace()
	if len(readSpace) != 5 {
		t.Fatalf("Expected read space length 5, got %d", len(readSpace))
	}

	for i, val := range readSpace {
		if val != data[i] {
			t.Errorf("Mismatch: expected %d, got %d", data[i], val)
		}
	}

	if err := cb.CommitRead(len(readSpace)); err != nil {
		t.Fatalf("Unexpected error during CommitRead: %v", err)
	}

	// No space to expose after flushing
	readSpace = cb.ExposeReadSpace()
	if readSpace != nil {
		t.Fatalf("Expected nil read space after committing, got %d", len(readSpace))
	}
}

// Test for incorrect usage of ExposeWriteSpace and CommitWrite
func TestIncorrectUsageExposeWriteAndCommitWrite(t *testing.T) {
	cb := NewCircularBuffer[int](5, 10)

	// Trying to commit write without exposing
	if err := cb.CommitWrite(1); err == nil {
		t.Fatal("Expected error when committing write without exposing, got nil")
	}

	// Fill buffer and try to expose more write space than available
	data := []int{1, 2, 3, 4, 5}
	_, err := cb.Write(data)
	if err != nil {
		t.Fatalf("Unexpected error during Write: %v", err)
	}

	writeSpace := cb.ExposeWriteSpace()
	if writeSpace != nil {
		t.Fatalf("Expected nil when exposing write space on full buffer, got %d", len(writeSpace))
	}

	// Also try to commit additional write wrongly
	if err := cb.CommitWrite(len(data) + 1); err == nil {
		t.Fatal("Expected error when overcommitting write space, got nil")
	}
}

// Test for incorrect usage of ExposeReadSpace and CommitRead
func TestIncorrectUsageExposeReadAndCommitRead(t *testing.T) {
	cb := NewCircularBuffer[int](5, 10)

	// Prepare buffer with initial data
	data := []int{1, 2, 3, 4, 5}
	n, err := cb.Write(data)
	if err != nil || n != len(data) {
		t.Fatalf("Unexpected error during Write: %v or written bytes %d", err, n)
	}

	// Commit read without exposing
	if err := cb.CommitRead(1); err == nil {
		t.Fatal("Expected error when committing read without exposing, got nil")
	}

	// Expose read space but commit more than available
	readSpace := cb.ExposeReadSpace()
	if err := cb.CommitRead(len(readSpace) + 1); err == nil {
		t.Fatal("Expected error when overcommitting read space, got nil")
	}

	// After full read, buffer is empty - no exposed read space should be available
	readSpace = cb.ExposeReadSpace()
	_ = cb.CommitRead(len(readSpace))
	readSpace = cb.ExposeReadSpace()
	if readSpace != nil {
		t.Fatalf("Expected nil read space after exhausting buffer, got %d", len(readSpace))
	}
}
