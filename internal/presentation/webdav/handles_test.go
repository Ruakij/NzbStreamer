package webdav

import (
	"bytes"
	"io"
	"sync"
	"testing"
	"time"

	"git.ruekov.eu/ruakij/nzbStreamer/internal/presentation"
)

// countingOpenable is a file whose readers say how many were ever opened, which
// is what reuse is meant to keep down.
type countingOpenable struct {
	data []byte
	// the reaper closes readers on its own goroutine
	mu     sync.Mutex
	opened int
	live   int
}

func (o *countingOpenable) Open() (io.ReadSeekCloser, error) {
	o.mu.Lock()
	defer o.mu.Unlock()

	o.opened++
	o.live++
	return &countingReader{Reader: bytes.NewReader(o.data), owner: o}, nil
}

func (o *countingOpenable) SizeHint() (int64, error) { return int64(len(o.data)), nil }

func (o *countingOpenable) counts() (opened, live int) {
	o.mu.Lock()
	defer o.mu.Unlock()

	return o.opened, o.live
}

type countingReader struct {
	*bytes.Reader
	owner  *countingOpenable
	closed bool
}

func (r *countingReader) Close() error {
	r.owner.mu.Lock()
	defer r.owner.mu.Unlock()

	if !r.closed {
		r.closed = true
		r.owner.live--
	}
	return nil
}

// streamOpenable is a compressed archive member: no addressable offsets.
type streamOpenable struct{ *countingOpenable }

func (o *streamOpenable) Open() (io.ReadSeekCloser, error) {
	reader, err := o.countingOpenable.Open()
	if err != nil {
		return nil, err
	}
	return struct{ io.ReadSeekCloser }{reader}, nil
}

func newOpenable(size int) *countingOpenable {
	data := make([]byte, size)
	for i := range data {
		data[i] = byte(i)
	}
	return &countingOpenable{data: data}
}

func TestCacheReusesAReaderAcrossRequests(t *testing.T) {
	openable := newOpenable(4096)
	cache := newHandleCache(time.Minute, 4)
	defer cache.Close()

	for range 3 {
		h, err := cache.acquire(openable, 0)
		if err != nil {
			t.Fatal(err)
		}
		if err := cache.release(h); err != nil {
			t.Fatal(err)
		}
	}

	if opened, _ := openable.counts(); opened != 1 {
		t.Errorf("opened %d readers, want the one being reused", opened)
	}
}

// park fills the cache with idle readers at the given positions.
func park(t *testing.T, cache *handleCache, openable presentation.Openable, positions ...int64) []*handle {
	t.Helper()

	parked := make([]*handle, 0, len(positions))
	for _, position := range positions {
		h, err := cache.acquire(openable, 0)
		if err != nil {
			t.Fatal(err)
		}
		h.position = position
		parked = append(parked, h)
	}
	for _, h := range parked {
		if err := cache.release(h); err != nil {
			t.Fatal(err)
		}
	}
	return parked
}

// Only a reader behind the offset can serve it warm, and the shorter the run up
// to it the more of its window covers it. One past the offset has to fetch it
// either way, so it comes last.
func TestCacheTakesTheShortestRunUpToTheOffset(t *testing.T) {
	openable := newOpenable(4096)
	cache := newHandleCache(time.Minute, 4)
	defer cache.Close()

	parked := park(t, cache, openable, 900, 100, 1000)
	near, far, ahead := parked[0], parked[1], parked[2]

	for _, want := range []*handle{near, far, ahead} {
		got, err := cache.acquire(openable, 940)
		if err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Fatalf("took the reader at %d, want the one at %d", got.position, want.position)
		}
	}
}

// A decoder stream reaches an offset behind it only by decoding from zero, so a
// fresh reader is the cheaper one.
func TestCacheLeavesAStreamPastTheOffset(t *testing.T) {
	openable := &streamOpenable{newOpenable(4096)}
	cache := newHandleCache(time.Minute, 4)
	defer cache.Close()

	ahead := park(t, cache, openable, 1000)[0]

	got, err := cache.acquire(openable, 940)
	if err != nil {
		t.Fatal(err)
	}
	if got == ahead {
		t.Fatal("took the stream at 1000, want a fresh one")
	}
}

func TestCacheClosesTheOldestOverTheCap(t *testing.T) {
	openable := newOpenable(4096)
	cache := newHandleCache(time.Minute, 2)
	defer cache.Close()

	handles := make([]*handle, 0, 3)
	for range 3 {
		h, err := cache.acquire(openable, 0)
		if err != nil {
			t.Fatal(err)
		}
		handles = append(handles, h)
	}
	for _, h := range handles {
		if err := cache.release(h); err != nil {
			t.Fatal(err)
		}
	}

	if _, live := openable.counts(); live != 2 {
		t.Errorf("%d readers left open, want the cap of 2", live)
	}
}

func TestCacheOffClosesEveryReader(t *testing.T) {
	openable := newOpenable(4096)
	var cache *handleCache // what a zero idle timeout builds

	h, err := cache.acquire(openable, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := cache.release(h); err != nil {
		t.Fatal(err)
	}

	if _, live := openable.counts(); live != 0 {
		t.Errorf("%d readers left open, want none", live)
	}
}

func TestCacheReapsWhatGoesIdle(t *testing.T) {
	openable := newOpenable(4096)
	cache := newHandleCache(20*time.Millisecond, 4)
	defer cache.Close()

	h, err := cache.acquire(openable, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := cache.release(h); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(2 * time.Second)
	live := 1
	for live != 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
		_, live = openable.counts()
	}
	if live != 0 {
		t.Error("the idle reader was never reaped")
	}
}
