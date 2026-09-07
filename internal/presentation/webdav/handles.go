package webdav

import (
	"container/list"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"time"

	"git.ruekov.eu/ruakij/nzbStreamer/internal/presentation"
)

// A Range request is a whole http request, so a client reading a file in spans
// opens the file once per span and the readahead window each open builds is
// thrown away with it. handleCache keeps a finished reader for idle, so the next
// span of the same file is served by a window that is already warm.
//
// Which reader serves a span is decided by where each one sits: the one just
// behind the offset holds it in its window, while one far ahead has to fetch it
// and, because the window's anchor only advances, keeps nothing it had.
type handleCache struct {
	idle    time.Duration
	maxIdle int

	mu sync.Mutex
	// free lists the idle readers per file, and order is every one of them
	// oldest-first, so the reaper and the cap both look at one end
	free  map[presentation.Openable][]*handle
	order *list.List
	timer *time.Timer
}

type handle struct {
	reader   io.ReadSeekCloser
	openable presentation.Openable
	// position is where the reader sits, which is what makes it worth reusing
	position int64
	idleAt   time.Time
	element  *list.Element
}

func newHandleCache(idle time.Duration, maxIdle int) *handleCache {
	if idle <= 0 || maxIdle <= 0 {
		return nil
	}
	return &handleCache{
		idle:    idle,
		maxIdle: maxIdle,
		free:    make(map[presentation.Openable][]*handle),
		order:   list.New(),
	}
}

// acquire takes the idle reader best placed to serve off, or opens one. A nil
// cache always opens, which is reuse switched off.
func (c *handleCache) acquire(openable presentation.Openable, off int64) (*handle, error) {
	if c != nil {
		if h := c.take(openable, off); h != nil {
			return h, nil
		}
	}

	reader, err := openable.Open()
	if err != nil {
		return nil, fmt.Errorf("open file: %w", err)
	}
	return &handle{reader: reader, openable: openable}, nil
}

// take removes the idle reader best placed for off, or reports nil.
func (c *handleCache) take(openable presentation.Openable, off int64) *handle {
	c.mu.Lock()
	defer c.mu.Unlock()

	idle := c.free[openable]
	best, bestRank := -1, rank{}
	for i, h := range idle {
		r, ok := rankFor(h, off)
		if ok && (best < 0 || r.beats(bestRank)) {
			best, bestRank = i, r
		}
	}
	if best < 0 {
		return nil
	}

	h := idle[best]
	c.free[openable] = append(idle[:best], idle[best+1:]...)
	if len(c.free[openable]) == 0 {
		delete(c.free, openable)
	}
	c.order.Remove(h.element)
	h.element = nil
	return h
}

// rank is what a reader is worth for an offset. Reaching it from behind is the
// only move that can be served warm, and the shorter that run is the more of the
// window covers it. Reaching it from ahead costs whatever a fresh reader costs,
// however far ahead it sits, so those rank alike and only save the open.
type rank struct {
	ahead    bool
	distance int64
}

func rankFor(h *handle, off int64) (rank, bool) {
	if h.position <= off {
		return rank{distance: off - h.position}, true
	}

	// A decoder stream reaches an earlier offset only by decoding from zero
	_, addressable := h.reader.(io.ReaderAt)
	return rank{ahead: true}, addressable
}

func (r rank) beats(other rank) bool {
	if r.ahead != other.ahead {
		return other.ahead
	}
	return r.distance < other.distance
}

// release parks a reader for reuse, closing it where there is no cache or no
// room. It is never the caller's to close afterwards.
func (c *handleCache) release(h *handle) error {
	if c == nil {
		return h.reader.Close()
	}

	c.mu.Lock()
	h.idleAt = time.Now()
	h.element = c.order.PushBack(h)
	c.free[h.openable] = append(c.free[h.openable], h)

	var evicted []*handle
	for c.order.Len() > c.maxIdle {
		evicted = append(evicted, c.removeOldestLocked())
	}
	c.scheduleLocked()
	c.mu.Unlock()

	return closeAll(evicted)
}

// Requires mu, and that the list is not empty.
func (c *handleCache) removeOldestLocked() *handle {
	oldest, _ := c.order.Remove(c.order.Front()).(*handle)
	oldest.element = nil

	idle := c.free[oldest.openable]
	for i, h := range idle {
		if h == oldest {
			c.free[oldest.openable] = append(idle[:i], idle[i+1:]...)
			break
		}
	}
	if len(c.free[oldest.openable]) == 0 {
		delete(c.free, oldest.openable)
	}
	return oldest
}

// scheduleLocked arms the reaper for when the oldest reader expires, so nothing
// ticks while the cache is empty. Requires mu.
func (c *handleCache) scheduleLocked() {
	front := c.order.Front()
	if front == nil {
		if c.timer != nil {
			c.timer.Stop()
			c.timer = nil
		}
		return
	}

	oldest, _ := front.Value.(*handle)
	in := max(time.Until(oldest.idleAt.Add(c.idle)), time.Millisecond)
	if c.timer == nil {
		c.timer = time.AfterFunc(in, c.reap)
		return
	}
	c.timer.Reset(in)
}

func (c *handleCache) reap() {
	c.mu.Lock()
	var expired []*handle
	deadline := time.Now().Add(-c.idle)
	for front := c.order.Front(); front != nil; front = c.order.Front() {
		oldest, _ := front.Value.(*handle)
		if oldest.idleAt.After(deadline) {
			break
		}
		expired = append(expired, c.removeOldestLocked())
	}
	c.scheduleLocked()
	c.mu.Unlock()

	if err := closeAll(expired); err != nil {
		slog.Error("Failed closing an idle webdav reader", "error", err)
	}
}

// discard closes every idle reader of one file, for a file leaving the tree.
func (c *handleCache) discard(openable presentation.Openable) error {
	if c == nil {
		return nil
	}

	c.mu.Lock()
	dropped := c.free[openable]
	delete(c.free, openable)
	for _, h := range dropped {
		c.order.Remove(h.element)
		h.element = nil
	}
	c.scheduleLocked()
	c.mu.Unlock()

	return closeAll(dropped)
}

// Close drops every idle reader, which is what a file leaving the tree needs.
func (c *handleCache) Close() error {
	if c == nil {
		return nil
	}

	c.mu.Lock()
	var all []*handle
	for c.order.Len() > 0 {
		all = append(all, c.removeOldestLocked())
	}
	c.scheduleLocked()
	c.mu.Unlock()

	return closeAll(all)
}

func closeAll(handles []*handle) error {
	var err error
	for _, h := range handles {
		if e := h.reader.Close(); e != nil && err == nil {
			err = e
		}
	}
	return err
}
