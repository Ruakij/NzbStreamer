package diskcache

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// ReadAt past the end of an in-memory (write-back) item must report io.EOF,
// as the file-backed path does, so a reader cannot mistake a short read for
// success.
func TestItemReadAtPastEndReturnsEOF(t *testing.T) {
	item := &Item{data: []byte("abc")}

	buf := make([]byte, 8)
	n, err := item.ReadAt(buf, 0)
	if !errors.Is(err, io.EOF) || n != 3 {
		t.Errorf("got n=%d err=%v, want 3 io.EOF", n, err)
	}
	if _, err := item.ReadAt(buf, 3); !errors.Is(err, io.EOF) {
		t.Errorf("read at end got %v, want io.EOF", err)
	}
}

// A negative offset is an error on both paths, so the behaviour does not depend
// on whether the entry has drained yet.
func TestItemReadAtNegativeOffset(t *testing.T) {
	file, err := os.CreateTemp(t.TempDir(), "")
	if err != nil {
		t.Fatalf("failed creating temp file: %v", err)
	}
	defer file.Close()

	for name, item := range map[string]*Item{
		"memory": {data: []byte("abc")},
		"file":   {file: file},
	} {
		if _, err := item.ReadAt(make([]byte, 4), -1); err == nil {
			t.Errorf("%s: negative offset did not error", name)
		}
	}
}

// An entry whose finalize can never succeed is dropped after its retries rather
// than kept pending, which the writer would otherwise pick up and rewrite in a
// tight loop forever.
func TestWritePendingDropsOnFinalizeFailure(t *testing.T) {
	cache := newBareWriteBackCache(t)

	// A regular file where the key's directory belongs makes finalize fail on
	// every attempt
	if err := os.WriteFile(filepath.Join(cache.options.CacheDir, "nzb"), nil, 0o600); err != nil {
		t.Fatalf("failed blocking the key directory: %v", err)
	}

	key := Key{"nzb", "seg"}
	data := []byte("payload")
	if _, err := cache.admit(key, data); err != nil {
		t.Fatalf("failed admitting: %v", err)
	}

	cache.mu.Lock()
	entry := cache.writeBack[key.String()]
	cache.mu.Unlock()
	cache.writePending(key.String(), entry)

	cache.mu.Lock()
	defer cache.mu.Unlock()
	if len(cache.writeBack) != 0 {
		t.Error("entry still pending after its write attempts")
	}
	if cache.writeBackBytes != 0 {
		t.Errorf("got %d write-back bytes, want 0", cache.writeBackBytes)
	}
	if _, exists := cache.items[key.String()]; exists {
		t.Error("dropped entry is still in the index")
	}
}

// A re-admitted key is a new entry, so a write still in flight for the one it
// replaced must not rename its bytes into place or drop the new one, even when
// the caller handed back the same buffer.
func TestFinalizeIgnoresReplacedEntry(t *testing.T) {
	cache := newBareWriteBackCache(t)

	key := Key{"nzb", "seg"}
	buffer := []byte("first")
	if _, err := cache.admit(key, buffer); err != nil {
		t.Fatalf("failed admitting: %v", err)
	}
	cache.mu.Lock()
	inflight := cache.writeBack[key.String()]
	cache.mu.Unlock()

	// The same backing array, re-Set: only the generation tells them apart
	copy(buffer, "sixth")
	if _, err := cache.admit(key, buffer); err != nil {
		t.Fatalf("failed re-admitting: %v", err)
	}

	temp, err := cache.writeTemp(inflight.data, int64(len(inflight.data)))
	if err != nil {
		t.Fatalf("failed writing temp: %v", err)
	}
	if err := cache.finalize(key.String(), inflight, temp); err != nil {
		t.Fatalf("failed finalizing: %v", err)
	}

	cache.mu.Lock()
	defer cache.mu.Unlock()
	if _, stillPending := cache.writeBack[key.String()]; !stillPending {
		t.Error("the replacing entry was dropped by the stale finalize")
	}
	if _, onDisk := cache.items[key.String()]; onDisk {
		t.Error("the stale entry was renamed into place")
	}
	if cache.currentSize != 0 {
		t.Errorf("got %d bytes counted, want 0", cache.currentSize)
	}
	if _, err := os.Stat(temp); !os.IsNotExist(err) {
		t.Error("the stale temp file was left behind")
	}
}

// A Set arriving after Close has no writer left to drain it, so it stores
// synchronously and the pending copy it replaces goes with it instead of being
// left behind, uncounted and served forever.
func TestAdmitAfterCloseReplacesPending(t *testing.T) {
	cache := newBareWriteBackCache(t)

	key := Key{"nzb", "seg"}
	if _, err := cache.admit(key, []byte("old")); err != nil {
		t.Fatalf("failed admitting: %v", err)
	}

	cache.mu.Lock()
	cache.closed = true
	cache.mu.Unlock()

	newData := []byte("new payload")
	if _, err := cache.admit(key, newData); err != nil {
		t.Fatalf("failed storing after close: %v", err)
	}

	stats := cache.Stats()
	if stats.WriteBackItems != 0 || stats.WriteBackBytes != 0 {
		t.Errorf("got %d pending items / %d bytes, want none", stats.WriteBackItems, stats.WriteBackBytes)
	}
	if stats.Items != 1 || stats.Bytes != int64(len(newData)) {
		t.Errorf("got %d items / %d bytes, want 1 / %d", stats.Items, stats.Bytes, len(newData))
	}

	item, size, err := cache.Open(key)
	if err != nil {
		t.Fatalf("failed opening: %v", err)
	}
	defer item.Close()
	buf := make([]byte, size)
	if _, err := item.ReadAt(buf, 0); err != nil && !errors.Is(err, io.EOF) {
		t.Fatalf("failed reading: %v", err)
	}
	if string(buf) != string(newData) {
		t.Errorf("got %q, want %q", buf, newData)
	}
}

// newBareWriteBackCache is a write-back cache without the writer goroutine, so
// a test drives the drain itself rather than racing it.
func newBareWriteBackCache(t *testing.T) *Cache {
	t.Helper()

	dir := t.TempDir()
	tmp := filepath.Join(dir, ".tmp")
	if err := os.MkdirAll(tmp, 0o700); err != nil {
		t.Fatalf("failed creating temp dir: %v", err)
	}

	cache := &Cache{
		mu:        &sync.RWMutex{},
		options:   &CacheOptions{CacheDir: dir, TmpCacheDir: tmp, WriteBackSize: 1 << 20},
		items:     make(map[string]CacheItemHeader),
		indexed:   make(chan struct{}),
		writeBack: make(map[string]pendingEntry),
	}
	cache.writeBackCond = sync.NewCond(cache.mu)
	close(cache.indexed)

	return cache
}
