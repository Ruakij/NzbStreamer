package diskcache_test

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"git.ruekov.eu/ruakij/nzbStreamer/pkg/diskcache"
)

func newCache(t *testing.T, dir string) *diskcache.Cache {
	t.Helper()

	cache, err := diskcache.NewCache(&diskcache.CacheOptions{CacheDir: dir})
	if err != nil {
		t.Fatalf("failed creating cache: %v", err)
	}
	return cache
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

	items, bytes, _ := restarted.Stats()
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

	items, bytes, _ := cache.Stats()
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
