package diskcache_test

import (
	"bytes"
	"strconv"
	"testing"
	"time"

	"git.ruekov.eu/ruakij/nzbStreamer/pkg/diskcache"
)

func benchCache(b *testing.B) *diskcache.Cache {
	b.Helper()
	cache, err := diskcache.NewCache(&diskcache.CacheOptions{CacheDir: b.TempDir()})
	if err != nil {
		b.Fatalf("failed creating cache: %v", err)
	}
	select {
	case <-cache.Indexed():
	case <-time.After(10 * time.Second):
		b.Fatal("cache did not finish indexing")
	}
	return cache
}

// BenchmarkCacheSet times a segment being written to the disk cache. Each
// iteration uses its own key, since the read path only ever stores a segment
// it did not have and an overwrite costs a different set of syscalls.
func BenchmarkCacheSet(b *testing.B) {
	cache := benchCache(b)
	blob := bytes.Repeat([]byte{7}, 1<<20)
	b.ReportAllocs()
	b.SetBytes(int64(len(blob)))
	i := 0
	for b.Loop() {
		if _, err := cache.Set(diskcache.Key{"bench", strconv.Itoa(i)}, blob); err != nil {
			b.Fatal(err)
		}
		i++
	}
}

// BenchmarkCacheGet times a segment being read back from the disk cache.
func BenchmarkCacheGet(b *testing.B) {
	cache := benchCache(b)
	blob := bytes.Repeat([]byte{7}, 1<<20)
	if _, err := cache.Set(diskcache.Key{"bench", "seg"}, blob); err != nil {
		b.Fatal(err)
	}

	buf := make([]byte, 1<<20)
	b.ReportAllocs()
	b.SetBytes(int64(len(blob)))
	for b.Loop() {
		f, size, err := cache.Open(diskcache.Key{"bench", "seg"})
		if err != nil {
			b.Fatal(err)
		}
		var read int64
		for {
			n, err := f.Read(buf)
			read += int64(n)
			if err != nil {
				break
			}
		}
		f.Close()
		if size != int64(len(blob)) || read != int64(len(blob)) {
			b.Fatalf("read %d of %d, metadata says %d", read, len(blob), size)
		}
	}
}
