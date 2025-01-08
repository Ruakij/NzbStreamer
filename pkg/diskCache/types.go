package diskCache

import (
	"sync"
	"time"
)

type CacheItemHeader struct {
	lock    *sync.RWMutex
	ModTime time.Time
	Size    int64
}

type CacheEvictPolicyHook func(entries map[string]CacheItemHeader) string

type Cache struct {
	options     *CacheOptions
	mu          *sync.RWMutex
	items       map[string]CacheItemHeader
	currentSize int64
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
