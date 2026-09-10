package diskcache_test

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"git.ruekov.eu/ruakij/nzbStreamer/pkg/diskcache"
)

func newCache(t *testing.T, dir string) *diskcache.Cache {
	t.Helper()

	cache, err := diskcache.NewCache(&diskcache.CacheOptions{CacheDir: dir})
	if err != nil {
		t.Fatalf("failed creating cache: %v", err)
	}

	select {
	case <-cache.Indexed():
	case <-time.After(10 * time.Second):
		t.Fatal("cache did not finish indexing")
	}

	return cache
}

// newWriteBackCache builds a cache with write-back enabled
func newWriteBackCache(t *testing.T, dir string, writeBackSize int64) *diskcache.Cache {
	t.Helper()

	cache, err := diskcache.NewCache(&diskcache.CacheOptions{CacheDir: dir, WriteBackSize: writeBackSize})
	if err != nil {
		t.Fatalf("failed creating cache: %v", err)
	}

	select {
	case <-cache.Indexed():
	case <-time.After(10 * time.Second):
		t.Fatal("cache did not finish indexing")
	}

	return cache
}

// Storing while the startup index runs must not count the item twice, since
// both the walk and the write see it.
func TestAnItemStoredTwiceIsCountedOnce(t *testing.T) {
	dir := t.TempDir()
	cache := newCache(t, dir)

	for range 2 {
		if _, err := cache.Set(diskcache.Key{"an-nzb", "segment-a"}, []byte("payload")); err != nil {
			t.Fatalf("failed storing: %v", err)
		}
	}

	stats := cache.Stats()
	items, bytes := stats.Items, stats.Bytes
	if items != 1 || bytes != int64(len("payload")) {
		t.Errorf("got %d items of %d bytes, want 1 of %d", items, bytes, len("payload"))
	}
}

func TestAKeyOfSeveralPartsIsStoredAsADirectory(t *testing.T) {
	dir := t.TempDir()
	cache := newCache(t, dir)

	if _, err := cache.Set(diskcache.Key{"an-nzb", "segment-a"}, []byte("payload")); err != nil {
		t.Fatalf("failed storing: %v", err)
	}

	if _, err := os.Stat(filepath.Join(dir, "an-nzb", "segment-a")); err != nil {
		t.Fatalf("item is not under its nzbs directory: %v", err)
	}

	file, size, err := cache.Open(diskcache.Key{"an-nzb", "segment-a"})
	if err != nil {
		t.Fatalf("failed opening: %v", err)
	}
	defer file.Close()
	if size != int64(len("payload")) {
		t.Errorf("got size %d, want %d", size, len("payload"))
	}
}

// A restart has to find what an earlier process cached, and the walk that does
// it is the part a flat ReadDir would get wrong.
func TestExistingItemsAreFoundInSubdirectories(t *testing.T) {
	dir := t.TempDir()
	cache := newCache(t, dir)
	if _, err := cache.Set(diskcache.Key{"an-nzb", "segment-a"}, []byte("payload")); err != nil {
		t.Fatalf("failed storing: %v", err)
	}

	restarted := newCache(t, dir)

	stats := restarted.Stats()
	items, bytes := stats.Items, stats.Bytes
	if items != 1 || bytes != int64(len("payload")) {
		t.Errorf("got %d items of %d bytes, want 1 of %d", items, bytes, len("payload"))
	}
	if exists, _ := restarted.Exists(diskcache.Key{"an-nzb", "segment-a"}); !exists {
		t.Error("item stored by an earlier process was not found")
	}
}

func TestRemoveAllDropsEveryItemOfAnNzb(t *testing.T) {
	dir := t.TempDir()
	cache := newCache(t, dir)
	for _, key := range []diskcache.Key{{"an-nzb", "segment-a"}, {"an-nzb", "segment-b"}, {"other-nzb", "segment-c"}} {
		if _, err := cache.Set(key, []byte("payload")); err != nil {
			t.Fatalf("failed storing: %v", err)
		}
	}

	if err := cache.RemoveAll(diskcache.Key{"an-nzb"}); err != nil {
		t.Fatalf("failed removing: %v", err)
	}

	stats := cache.Stats()
	items, bytes := stats.Items, stats.Bytes
	if items != 1 || bytes != int64(len("payload")) {
		t.Errorf("got %d items of %d bytes, want the one item of the other nzb", items, bytes)
	}
	if _, err := os.Stat(filepath.Join(dir, "an-nzb")); !os.IsNotExist(err) {
		t.Errorf("directory of the removed nzb is still there: %v", err)
	}
}

// The last item of an nzb leaves an empty directory behind, which is the
// scaling problem this layout was meant to solve.
func TestEvictingTheLastItemRemovesItsDirectory(t *testing.T) {
	dir := t.TempDir()
	cache := newCache(t, dir)
	if _, err := cache.Set(diskcache.Key{"an-nzb", "segment-a"}, []byte("payload")); err != nil {
		t.Fatalf("failed storing: %v", err)
	}

	if err := cache.Remove(diskcache.Key{"an-nzb", "segment-a"}); err != nil {
		t.Fatalf("failed removing: %v", err)
	}

	if _, err := os.Stat(filepath.Join(dir, "an-nzb")); !os.IsNotExist(err) {
		t.Errorf("empty directory was left behind: %v", err)
	}
}

// Only a hit path can tell a hit from a miss, and only eviction separates the
// items the cache dropped for space from the ones a caller removed.
func TestStatsCountHitsMissesAndEvictions(t *testing.T) {
	dir := t.TempDir()
	cache, err := diskcache.NewCache(&diskcache.CacheOptions{CacheDir: dir, MaxSize: 10, MaxSizeEvictBlocking: true})
	if err != nil {
		t.Fatalf("failed creating cache: %v", err)
	}
	select {
	case <-cache.Indexed():
	case <-time.After(10 * time.Second):
		t.Fatal("cache did not finish indexing")
	}

	if _, err := cache.Set(diskcache.Key{"an-nzb", "segment-a"}, []byte("payload")); err != nil {
		t.Fatalf("failed storing: %v", err)
	}
	file, _, err := cache.Open(diskcache.Key{"an-nzb", "segment-a"})
	if err != nil {
		t.Fatalf("failed opening: %v", err)
	}
	file.Close()
	if _, _, err := cache.Open(diskcache.Key{"an-nzb", "segment-b"}); !errors.Is(err, diskcache.ErrItemNotFound) {
		t.Fatalf("got %v, want ErrItemNotFound", err)
	}

	// The limit leaves no room for a second item, so storing one drops the first
	if _, err := cache.Set(diskcache.Key{"an-nzb", "segment-b"}, []byte("payload")); err != nil {
		t.Fatalf("failed storing: %v", err)
	}

	stats := cache.Stats()
	if stats.Hits != 1 || stats.Misses != 1 || stats.Evictions != 1 {
		t.Errorf("got %d hits, %d misses, %d evictions, want 1 of each", stats.Hits, stats.Misses, stats.Evictions)
	}
}

func TestGroupsCountPerPrefix(t *testing.T) {
	cache := newCache(t, t.TempDir())

	for _, key := range []diskcache.Key{{"one", "a"}, {"one", "b"}, {"two", "a"}} {
		if _, err := cache.Set(key, []byte("payload")); err != nil {
			t.Fatalf("failed storing: %v", err)
		}
	}

	groups := cache.Groups()
	if groups["one"].Items != 2 || groups["one"].Bytes != 14 {
		t.Errorf("group one holds %d items of %d bytes, want 2 of 14", groups["one"].Items, groups["one"].Bytes)
	}
	if groups["two"].Items != 1 {
		t.Errorf("group two holds %d items, want 1", groups["two"].Items)
	}
}

// Both parts of a key come out of an nzb, so neither may address anything
// outside the cache directory.
func TestAKeyCannotEscapeTheCacheDir(t *testing.T) {
	dir := t.TempDir()
	cache := newCache(t, dir)

	for _, key := range []diskcache.Key{{"..", "escaped"}, {"an-nzb", ".."}, {}} {
		if _, err := cache.Set(key, []byte("payload")); !errors.Is(err, diskcache.ErrInvalidKey) {
			t.Errorf("key %v: got %v, want ErrInvalidKey", key, err)
		}
	}
}

// A segment admitted to write-back is served correctly before it has drained
// (from memory or from a just-written file, whichever the eager writer reached
// first), and drains to disk on Close.
func TestWriteBackServesFromMemoryAndDrainsOnClose(t *testing.T) {
	dir := t.TempDir()
	cache := newWriteBackCache(t, dir, 1<<20)
	defer cache.Close()

	data := []byte("payload data")
	key := diskcache.Key{"an-nzb", "segment-a"}
	if _, err := cache.Set(key, data); err != nil {
		t.Fatalf("failed storing: %v", err)
	}

	// Immediately after Set the item is visible and returns the exact data,
	// whether the eager writer has drained it to disk or it is still in memory
	if exists, _ := cache.Exists(key); !exists {
		t.Error("segment is not visible")
	}
	item, size, err := cache.Open(key)
	if err != nil {
		t.Fatalf("failed opening segment: %v", err)
	}
	if size != int64(len(data)) {
		t.Errorf("got size %d, want %d", size, len(data))
	}
	buf := make([]byte, len(data))
	if _, err := item.ReadAt(buf, 0); err != nil && !errors.Is(err, os.ErrClosed) {
		t.Fatalf("failed reading segment: %v", err)
	}
	if !bytes.Equal(buf, data) {
		t.Errorf("read = %q, want %q", buf, data)
	}

	if err := cache.Close(); err != nil {
		t.Fatalf("failed closing cache: %v", err)
	}

	// Drained: on disk, indexed, nothing left pending
	stats := cache.Stats()
	if stats.Items != 1 || stats.Bytes != int64(len(data)) || stats.WriteBackBytes != 0 || stats.WriteBackItems != 0 {
		t.Errorf("after close got items=%d bytes=%d write-back bytes=%d items=%d, want 1 of %d and 0", stats.Items, stats.Bytes, stats.WriteBackBytes, stats.WriteBackItems, len(data))
	}
	item, size, err = cache.Open(key)
	if err != nil {
		t.Fatalf("failed reopening drained segment: %v", err)
	}
	defer item.Close()
	if size != int64(len(data)) {
		t.Errorf("after close got size %d, want %d", size, len(data))
	}
	buf = make([]byte, len(data))
	if _, err := item.ReadAt(buf, 0); err != nil && !errors.Is(err, os.ErrClosed) {
		t.Fatalf("failed reading drained segment: %v", err)
	}
	if !bytes.Equal(buf, data) {
		t.Errorf("drained read = %q, want %q", buf, data)
	}
}

// A Set that would overflow the buffer blocks until the writer frees room.
// This uses an oversized first segment so draining it takes long enough to
// observe the second Set waiting.
func TestWriteBackAdmissionBackpressure(t *testing.T) {
	dir := t.TempDir()
	cache := newWriteBackCache(t, dir, 64)
	defer cache.Close()

	// Oversized admits on an empty buffer and keeps the writer draining long
	// enough to observe the second Set waiting
	big := bytes.Repeat([]byte{7}, 256<<20)
	if _, err := cache.Set(diskcache.Key{"nzb", "big"}, big); err != nil {
		t.Fatalf("failed storing big segment: %v", err)
	}

	entered := make(chan struct{})
	done := make(chan struct{})
	go func() {
		close(entered)
		if _, err := cache.Set(diskcache.Key{"nzb", "small"}, []byte("small payload")); err != nil {
			t.Errorf("failed storing small segment: %v", err)
		}
		close(done)
	}()

	<-entered
	// The small Set must be blocked on admission while the buffer is full
	select {
	case <-done:
		t.Fatal("second Set returned while the buffer was still full")
	case <-time.After(50 * time.Millisecond):
	}

	// Freeing room lets it admit
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("second Set never returned after room was made")
	}

	// The small one may still be pending, so both counts together hold it
	stats := cache.Stats()
	if held := stats.Items + stats.WriteBackItems; held != 2 {
		t.Errorf("got %d items, want 2", held)
	}
}

// A segment larger than the whole limit still admits (oversized) instead of
// deadlocking on an empty buffer, and is still served once drained.
func TestWriteBackOversizedSegmentAdmits(t *testing.T) {
	dir := t.TempDir()
	cache := newWriteBackCache(t, dir, 64)
	defer cache.Close()

	data := bytes.Repeat([]byte{1}, 1024)
	key := diskcache.Key{"nzb", "big"}
	if _, err := cache.Set(key, data); err != nil {
		t.Fatalf("oversized segment did not admit: %v", err)
	}
	if err := cache.Close(); err != nil {
		t.Fatalf("failed closing: %v", err)
	}

	item, size, err := cache.Open(key)
	if err != nil {
		t.Fatalf("failed opening oversized segment: %v", err)
	}
	defer item.Close()
	if size != int64(len(data)) {
		t.Errorf("got size %d, want %d", size, len(data))
	}
	buf := make([]byte, len(data))
	if _, err := item.ReadAt(buf, 0); err != nil && !errors.Is(err, os.ErrClosed) {
		t.Fatalf("failed reading oversized segment: %v", err)
	}
	if !bytes.Equal(buf, data) {
		t.Errorf("read = %q, want %q", buf, data)
	}
}

// Removing a segment while its write-back write may be in flight must leave
// the cache without the item and without a file, whatever the timing: if it
// drained before Remove the file is unlinked, otherwise the pending entry is
// dropped and the in-flight write is aborted at finalize.
func TestRemoveWhilePendingLeavesNoFile(t *testing.T) {
	dir := t.TempDir()
	cache := newWriteBackCache(t, dir, 1<<20)

	key := diskcache.Key{"an-nzb", "segment-a"}
	if _, err := cache.Set(key, []byte("payload")); err != nil {
		t.Fatalf("failed storing: %v", err)
	}
	if err := cache.Remove(key); err != nil {
		t.Fatalf("failed removing pending: %v", err)
	}
	if err := cache.Close(); err != nil {
		t.Fatalf("failed closing: %v", err)
	}

	if exists, _ := cache.Exists(key); exists {
		t.Error("removed pending segment still present")
	}
	if _, err := os.Stat(filepath.Join(dir, "an-nzb", "segment-a")); !os.IsNotExist(err) {
		t.Errorf("file exists for removed pending segment: %v", err)
	}
	if stats := cache.Stats(); stats.Items != 0 || stats.Bytes != 0 {
		t.Errorf("got %d items of %d bytes, want 0 of 0", stats.Items, stats.Bytes)
	}
}

// RemoveAll must drop pending entries too, including an nzb that has only
// pending segments and thus no on-disk directory.
func TestRemoveAllWhilePending(t *testing.T) {
	dir := t.TempDir()
	cache := newWriteBackCache(t, dir, 1<<20)

	for _, key := range []diskcache.Key{{"n", "a"}, {"n", "b"}, {"other", "c"}} {
		if _, err := cache.Set(key, []byte("payload")); err != nil {
			t.Fatalf("failed storing: %v", err)
		}
	}
	if err := cache.RemoveAll(diskcache.Key{"n"}); err != nil {
		t.Fatalf("failed removing all pending: %v", err)
	}
	if err := cache.Close(); err != nil {
		t.Fatalf("failed closing: %v", err)
	}

	stats := cache.Stats()
	if stats.Items != 1 || stats.Bytes != int64(len("payload")) {
		t.Errorf("got %d items of %d bytes, want the one remaining item", stats.Items, stats.Bytes)
	}
	if exists, _ := cache.Exists(diskcache.Key{"n", "a"}); exists {
		t.Error("pending entry under the removed prefix still present")
	}
}

// Re-setting a still-pending key must leave the latest data in effect, whether
// the writer flushed the older copy or aborted it.
func TestReSetWhilePendingWinsLatest(t *testing.T) {
	dir := t.TempDir()
	cache := newWriteBackCache(t, dir, 1<<20)

	key := diskcache.Key{"an-nzb", "segment-a"}
	if _, err := cache.Set(key, []byte("aaa")); err != nil {
		t.Fatalf("failed storing: %v", err)
	}
	if _, err := cache.Set(key, []byte("bbb")); err != nil {
		t.Fatalf("failed re-storing: %v", err)
	}
	if err := cache.Close(); err != nil {
		t.Fatalf("failed closing: %v", err)
	}

	stats := cache.Stats()
	if stats.Items != 1 {
		t.Errorf("got %d items, want 1", stats.Items)
	}
	item, _, err := cache.Open(key)
	if err != nil {
		t.Fatalf("failed opening: %v", err)
	}
	defer item.Close()
	buf := make([]byte, 3)
	if _, err := item.ReadAt(buf, 0); err != nil && !errors.Is(err, os.ErrClosed) {
		t.Fatalf("failed reading: %v", err)
	}
	if string(buf) != "bbb" {
		t.Errorf("got %q, want %q", buf, "bbb")
	}
}

// A Set after Close falls back to the synchronous store, so nothing is left
// pending behind a cache that stopped draining.
func TestWriteBackSetAfterCloseWritesThrough(t *testing.T) {
	dir := t.TempDir()
	cache := newWriteBackCache(t, dir, 1<<20)
	if err := cache.Close(); err != nil {
		t.Fatalf("failed closing: %v", err)
	}

	key := diskcache.Key{"an-nzb", "segment-a"}
	if _, err := cache.Set(key, []byte("payload")); err != nil {
		t.Fatalf("failed storing after close: %v", err)
	}

	stats := cache.Stats()
	if stats.WriteBackItems != 0 || stats.Bytes != int64(len("payload")) {
		t.Errorf("after-close set got write-back items %d and %d on-disk bytes, want 0 and %d", stats.WriteBackItems, stats.Bytes, len("payload"))
	}
	if _, err := os.Stat(filepath.Join(dir, "an-nzb", "segment-a")); err != nil {
		t.Errorf("file was not written through on after-close set: %v", err)
	}
}
