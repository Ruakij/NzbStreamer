package readeratwrapper

import (
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeSeekCloser records how the worker moves across it, for direct asserts.
type fakeSeekCloser struct {
	data []byte

	mu        sync.Mutex
	position  int64
	seekCount int
	order     []int64
	closed    bool

	gatedRead chan struct{} // when set, Read blocks until it is closed
	readErr   error         // returned once instead of nil
	shortRead bool          // return at most one byte per Read

	active atomic.Int64
	peak   atomic.Int64
}

func (f *fakeSeekCloser) Read(p []byte) (int, error) {
	active := f.active.Add(1)
	defer f.active.Add(-1)
	for {
		peak := f.peak.Load()
		if active <= peak || f.peak.CompareAndSwap(peak, active) {
			break
		}
	}

	if f.gatedRead != nil {
		<-f.gatedRead
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	var err error
	if f.position >= int64(len(f.data)) {
		return 0, io.EOF
	}
	n := len(p)
	if f.shortRead && n > 1 {
		n = 1
	}
	end := f.position + int64(n)
	if end > int64(len(f.data)) {
		end = int64(len(f.data))
	}
	n = int(end - f.position)
	copy(p, f.data[f.position:end])
	f.order = append(f.order, f.position)
	f.position += int64(n)
	if err = f.readErr; err != nil {
		f.readErr = nil
		return n, err
	}
	if f.position >= int64(len(f.data)) {
		return n, io.EOF
	}
	return n, nil
}

func (f *fakeSeekCloser) Seek(offset int64, whence int) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	position := f.position
	switch whence {
	case io.SeekStart:
		position = offset
	case io.SeekCurrent:
		position += offset
	case io.SeekEnd:
		position = int64(len(f.data)) + offset
	default:
		return 0, errors.New("bad whence")
	}
	if position < 0 {
		return 0, errors.New("negative seek")
	}
	f.position = position
	f.seekCount++
	return position, nil
}

func (f *fakeSeekCloser) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return errors.New("closed twice")
	}
	f.closed = true
	return nil
}

func newBatched(t *testing.T, f *fakeSeekCloser, delay time.Duration) *ReadSeekerBatchedAt {
	t.Helper()
	b := NewReadSeekerBatchedAt(f, delay)
	t.Cleanup(func() { b.Close() })
	return b
}

func TestReadSeekerBatchedAtReadsInterior(t *testing.T) {
	f := &fakeSeekCloser{data: []byte("0123456789")}
	b := newBatched(t, f, time.Millisecond)

	buf := make([]byte, 4)
	n, err := b.ReadAt(buf, 3)
	if err != nil {
		t.Fatalf("ReadAt: %v", err)
	}
	if n != 4 || string(buf) != "3456" {
		t.Fatalf("read %d %q", n, buf)
	}
}

func TestReadSeekerBatchedAtShortReadIsEOF(t *testing.T) {
	f := &fakeSeekCloser{data: []byte("0123456789")}
	b := newBatched(t, f, time.Millisecond)

	n, err := b.ReadAt(make([]byte, 16), 7)
	if !errors.Is(err, io.EOF) {
		t.Fatalf("want io.EOF, got %v", err)
	}
	if n != 3 {
		t.Fatalf("read %d bytes, want 3", n)
	}
}

func TestReadSeekerBatchedAtAtAndBeyondEOF(t *testing.T) {
	f := &fakeSeekCloser{data: []byte("0123")}
	b := newBatched(t, f, time.Millisecond)

	for _, off := range []int64{4, 100} {
		if n, err := b.ReadAt(make([]byte, 4), off); !errors.Is(err, io.EOF) || n != 0 {
			t.Fatalf("off %d: n=%d err=%v, want 0 with io.EOF", off, n, err)
		}
	}
}

func TestReadSeekerBatchedAtZeroLength(t *testing.T) {
	f := &fakeSeekCloser{data: []byte("0123")}
	b := newBatched(t, f, time.Millisecond)

	if n, err := b.ReadAt(nil, 0); n != 0 || err != nil {
		t.Fatalf("nil read: n=%d err=%v", n, err)
	}
	if f.seekCount != 0 {
		t.Fatalf("zero-length read touched the stream %d times", f.seekCount)
	}
}

func TestReadSeekerBatchedAtNegativeOffset(t *testing.T) {
	f := &fakeSeekCloser{data: []byte("0123")}
	b := newBatched(t, f, time.Millisecond)

	if _, err := b.ReadAt(make([]byte, 4), -1); !errors.Is(err, ErrInvalidOffset) {
		t.Fatalf("want ErrInvalidOffset, got %v", err)
	}
	if f.seekCount != 0 {
		t.Fatalf("negative read touched the stream %d times", f.seekCount)
	}
}

func TestReadSeekerBatchedAtLoopsOnShortReads(t *testing.T) {
	f := &fakeSeekCloser{data: []byte("0123456789"), shortRead: true}
	b := newBatched(t, f, time.Millisecond)

	buf := make([]byte, 5)
	n, err := b.ReadAt(buf, 0)
	if err != nil {
		t.Fatalf("ReadAt: %v", err)
	}
	if n != 5 || string(buf) != "01234" {
		t.Fatalf("read %d %q", n, buf)
	}
}

// Out-of-order kernel reads reach the stream sorted by offset.
func TestReadSeekerBatchedAtSortsConcurrentRequests(t *testing.T) {
	f := &fakeSeekCloser{data: make([]byte, 1024)}
	b := newBatched(t, f, 200*time.Millisecond)

	offsets := []int64{300, 100, 500, 0, 400, 200}
	var wg sync.WaitGroup
	for _, off := range offsets {
		wg.Add(1)
		go func(off int64) {
			defer wg.Done()
			if _, err := b.ReadAt(make([]byte, 8), off); err != nil {
				t.Errorf("ReadAt(%d): %v", off, err)
			}
		}(off)
	}
	wg.Wait()

	want := []int64{0, 100, 200, 300, 400, 500}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.order) != len(want) {
		t.Fatalf("stream saw %d reads %v, want %d", len(f.order), f.order, len(want))
	}
	for i := range want {
		if f.order[i] != want[i] {
			t.Fatalf("stream order %v, want %v", f.order, want)
		}
	}
}

// A cursor-matching read runs immediately and without a seek.
func TestReadSeekerBatchedAtSequentialReadSkipsSeek(t *testing.T) {
	f := &fakeSeekCloser{data: make([]byte, 1024)}
	b := newBatched(t, f, time.Second)

	if _, err := b.ReadAt(make([]byte, 16), 0); err != nil {
		t.Fatal(err)
	}
	seeks := f.seekCount

	done := make(chan struct{})
	go func() {
		if _, err := b.ReadAt(make([]byte, 16), 16); err != nil {
			t.Error(err)
		}
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("cursor-matching read did not run immediately")
	}
	if f.seekCount != seeks {
		t.Fatalf("sequential read sought: %d seeks, want %d", f.seekCount, seeks)
	}
}

// The single worker means the stream never sees concurrent access.
func TestReadSeekerBatchedAtNeverReadsConcurrently(t *testing.T) {
	f := &fakeSeekCloser{data: make([]byte, 4096)}
	b := newBatched(t, f, 10*time.Millisecond)

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if _, err := b.ReadAt(make([]byte, 64), int64(i*64)); err != nil {
				t.Error(err)
			}
		}(i)
	}
	wg.Wait()

	if peak := f.peak.Load(); peak != 1 {
		t.Fatalf("peak concurrent stream reads = %d, want 1", peak)
	}
}

func TestReadSeekerBatchedAtReadErrorIsReported(t *testing.T) {
	sentinel := errors.New("boom") // a full read would swallow the error
	f := &fakeSeekCloser{data: []byte("0123456789"), shortRead: true, readErr: sentinel}
	b := newBatched(t, f, time.Millisecond)

	n, err := b.ReadAt(make([]byte, 4), 0)
	if err == nil || !errors.Is(err, sentinel) {
		t.Fatalf("want sentinel error, got n=%d err=%v", n, err)
	}
	if n != 1 {
		t.Fatalf("read %d bytes, want the short read's 1", n)
	}
}

func TestReadSeekerBatchedAtCloseUnblocksPendingReads(t *testing.T) {
	f := &fakeSeekCloser{data: make([]byte, 1024), gatedRead: make(chan struct{})}
	b := NewReadSeekerBatchedAt(f, time.Millisecond)

	// First read holds the worker in the gated read.
	blocking := make(chan error, 1)
	go func() {
		_, err := b.ReadAt(make([]byte, 16), 0)
		blocking <- err
	}()
	deadline := time.After(time.Second)
	for f.active.Load() == 0 {
		select {
		case <-deadline:
			t.Fatal("worker never started the read")
		case <-time.After(time.Millisecond):
		}
	}

	// Second read queues behind it; Close is issued.
	queued := make(chan error, 1)
	go func() {
		_, err := b.ReadAt(make([]byte, 16), 16)
		queued <- err
	}()
	closed := make(chan error, 1)
	go func() { closed <- b.Close() }()

	// Nothing completes until the in-flight read is released.
	select {
	case err := <-blocking:
		t.Fatalf("in-flight read finished early: %v", err)
	case <-time.After(50 * time.Millisecond):
	}

	close(f.gatedRead)

	// In-flight read reports its data; Close rejects the queued read.
	select {
	case err := <-blocking:
		if err != nil {
			t.Fatalf("in-flight read got %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("in-flight read never finished")
	}
	select {
	case err := <-queued:
		if !errors.Is(err, ErrClosed) {
			t.Fatalf("queued read got %v, want ErrClosed", err)
		}
	case <-time.After(time.Second):
		t.Fatal("queued read never unblocked")
	}
	select {
	case err := <-closed:
		if err != nil {
			t.Fatalf("Close: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Close never returned")
	}

	if !f.closed {
		t.Fatal("underlying reader was not closed")
	}
}

func TestReadSeekerBatchedAtReadAfterClose(t *testing.T) {
	f := &fakeSeekCloser{data: make([]byte, 64)}
	b := NewReadSeekerBatchedAt(f, time.Millisecond)
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := b.ReadAt(make([]byte, 4), 0); !errors.Is(err, ErrClosed) {
		t.Fatalf("read after close got %v, want ErrClosed", err)
	}
}

func TestReadSeekerBatchedAtCloseIsIdempotent(t *testing.T) {
	f := &fakeSeekCloser{data: make([]byte, 64)}
	b := newBatched(t, f, time.Millisecond)

	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := b.Close(); err != nil {
				t.Errorf("Close: %v", err)
			}
		}()
	}
	wg.Wait()

	// Closing twice on the fake is an error, so a duplicate underlying close
	// would surface here.
	if err := b.Close(); err != nil {
		t.Fatalf("Close again: %v", err)
	}
}
