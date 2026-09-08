package main

import (
	"testing"
	"time"

	"git.ruekov.eu/ruakij/nzbStreamer/internal/service/nzbservice"
	"git.ruekov.eu/ruakij/nzbStreamer/pkg/diskcache"
)

// A wire-counted nzb hints at more bytes than a segment decodes to, so a size
// summed from the hints would leave a fully cached file short of 100%.
func TestCachedSegmentsWeighWhatTheCacheStored(t *testing.T) {
	cache, err := diskcache.NewCache(&diskcache.CacheOptions{CacheDir: t.TempDir()})
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

	file := nzbservice.PostedFile{Name: "a.rar", Segments: []nzbservice.PostedSegment{
		{ID: "segment-a", Bytes: 9, Exact: false},
		{ID: "segment-b", Bytes: 4, Exact: true},
	}}

	stat := statFile(cache, "an-nzb", file)

	if stat.CachedBytes != 7 || stat.CachedSegments != 1 {
		t.Errorf("cached = %d bytes in %d segments, want 7 in 1", stat.CachedBytes, stat.CachedSegments)
	}
	if stat.Size != 11 {
		t.Errorf("size = %d, want 11: the stored 7 and the estimated 4", stat.Size)
	}
	if !stat.Exact {
		t.Error("size reported as an estimate, though the only estimated segment is cached")
	}

	// The uncached one is what decides it now
	file.Segments[1].Exact = false
	if statFile(cache, "an-nzb", file).Exact {
		t.Error("size reported as exact with an uncached segment only estimated")
	}
}
