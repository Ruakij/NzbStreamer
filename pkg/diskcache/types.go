package diskcache

import (
	"sync"
	"sync/atomic"
	"time"
)

type CacheItemHeader struct {
	ModTime time.Time
	Size    int64
}

type CacheEvictPolicyHook func(entries map[string]CacheItemHeader) string

type Cache struct {
	options     *CacheOptions
	mu          *sync.RWMutex
	items       map[string]CacheItemHeader
	currentSize int64
	indexed     chan struct{}

	// Only a hit path can keep these, so they are counted rather than derived
	hits      atomic.Int64
	misses    atomic.Int64
	evictions atomic.Int64
}

// Stats is what the cache holds against what it may hold, and what it has done
// since the process started.
type Stats struct {
	Items    int
	Bytes    int64
	MaxBytes int64

	Hits      int64
	Misses    int64
	Evictions int64
}

// GroupStats is what one key prefix holds.
type GroupStats struct {
	Items    int
	Bytes    int64
	LastRead time.Time
}

type CacheOptions struct {
	// Location to store cache items
	CacheDir string
	// Location to store temporary items
	TmpCacheDir string
	// Max total size of cache on disk in bytes
	MaxSize int64
	// If eviction, due to missing free space, should block adding a new item; false also disables error return when evict failed
	MaxSizeEvictBlocking bool
	// Max size an item can be before its rejected
	ItemMaxSize int64
	// Called when eviction is required e.g. due to missing free space
	EvictPolicyHook CacheEvictPolicyHook
}

var defaultCacheOptions CacheOptions = CacheOptions{
	EvictPolicyHook: EvictLRU,
}
