package fullcacheresource_test

import (
	"bytes"
	"errors"
	"io"
	"sync/atomic"
	"testing"

	"git.ruekov.eu/ruakij/nzbStreamer/pkg/diskcache"
	"git.ruekov.eu/ruakij/nzbStreamer/pkg/resource/fullcacheresource"
)

// countingResource serves fixed content and counts how often it was opened,
// which tells us how often the segment had to be fetched.
type countingResource struct {
	content []byte
	opens   atomic.Int64
}

func (r *countingResource) Open() (io.ReadCloser, error) {
	r.opens.Add(1)
	return io.NopCloser(bytes.NewReader(r.content)), nil
}

func (r *countingResource) SizeHint() (int64, error) { return int64(len(r.content)), nil }

func newTestResource(t *testing.T, key string, content []byte) (*fullcacheresource.FullCacheResource, *countingResource, *diskcache.Cache) {
	t.Helper()

	cache, err := diskcache.NewCache(&diskcache.CacheOptions{CacheDir: t.TempDir()})
	if err != nil {
		t.Fatalf("failed creating cache: %v", err)
	}

	underlying := &countingResource{content: content}
	return fullcacheresource.NewFullCacheResource(underlying, diskcache.Key{key}, cache, &fullcacheresource.FullCacheResourceOptions{}), underlying, cache
}

func TestReadFetchesOnceAcrossManyReads(t *testing.T) {
	content := []byte("0123456789abcdefghij")
	res, underlying, _ := newTestResource(t, "segment-a", content)

	reader, err := res.Open()
	if err != nil {
		t.Fatalf("failed opening: %v", err)
	}
	defer reader.Close()

	got, err := io.ReadAll(reader)
	if err != nil && !errors.Is(err, io.EOF) {
		t.Fatalf("failed reading: %v", err)
	}
	if !bytes.Equal(got, content) {
		t.Fatalf("got %q, want %q", got, content)
	}

	// io.ReadAll issues many small Reads; all but the first must be served from the open file
	if opens := underlying.opens.Load(); opens != 1 {
		t.Errorf("underlying resource opened %d times, want 1", opens)
	}
}

func TestSeekReadsFromPosition(t *testing.T) {
	content := []byte("0123456789abcdefghij")
	res, _, _ := newTestResource(t, "segment-b", content)

	reader, err := res.Open()
	if err != nil {
		t.Fatalf("failed opening: %v", err)
	}
	defer reader.Close()

	if _, err := reader.Seek(10, io.SeekStart); err != nil {
		t.Fatalf("failed seeking: %v", err)
	}

	buf := make([]byte, 5)
	n, err := reader.Read(buf)
	if err != nil && !errors.Is(err, io.EOF) {
		t.Fatalf("failed reading: %v", err)
	}
	if want := []byte("abcde"); !bytes.Equal(buf[:n], want) {
		t.Errorf("got %q, want %q", buf[:n], want)
	}
}

// An open reader must survive its cache-item being evicted underneath it, and
// refetch once it needs a new file.
func TestReadAfterEvictionRefetches(t *testing.T) {
	content := []byte("0123456789abcdefghij")
	res, underlying, cache := newTestResource(t, "segment-c", content)

	reader, err := res.Open()
	if err != nil {
		t.Fatalf("failed opening: %v", err)
	}
	defer reader.Close()

	buf := make([]byte, 5)
	if _, err := reader.Read(buf); err != nil && !errors.Is(err, io.EOF) {
		t.Fatalf("failed first read: %v", err)
	}

	if err := cache.Remove(diskcache.Key{"segment-c"}); err != nil {
		t.Fatalf("failed evicting: %v", err)
	}

	// Existing descriptor still resolves, unlink does not invalidate it
	if _, err := reader.Read(buf); err != nil && !errors.Is(err, io.EOF) {
		t.Fatalf("failed read after eviction: %v", err)
	}
	if opens := underlying.opens.Load(); opens != 1 {
		t.Errorf("underlying resource opened %d times, want 1", opens)
	}

	// A fresh reader has to refetch
	reader2, err := res.Open()
	if err != nil {
		t.Fatalf("failed reopening: %v", err)
	}
	defer reader2.Close()

	got, err := io.ReadAll(reader2)
	if err != nil && !errors.Is(err, io.EOF) {
		t.Fatalf("failed reading after eviction: %v", err)
	}
	if !bytes.Equal(got, content) {
		t.Fatalf("got %q, want %q", got, content)
	}
	if opens := underlying.opens.Load(); opens != 2 {
		t.Errorf("underlying resource opened %d times, want 2", opens)
	}
}
