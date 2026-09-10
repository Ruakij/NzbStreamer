package diskcache

import (
	"io"
	"os"
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
	options *CacheOptions
	mu      *sync.RWMutex
	// items is what is on disk, and currentSize its bytes. A segment awaiting
	// its write is in writeBack alone, so nothing on the disk side has to
	// remember to tell the two apart.
	items       map[string]CacheItemHeader
	currentSize int64
	indexed     chan struct{}

	// Only a hit path can keep these, so they are counted rather than derived
	hits      atomic.Int64
	misses    atomic.Int64
	evictions atomic.Int64

	// Write-back state. Only used when options.WriteBackSize > 0.
	// All protected by mu.
	writeBack      map[string]pendingEntry
	writeBackBytes int64
	writeBackGen   uint64
	writeBackCond  *sync.Cond
	writeWG        sync.WaitGroup
	closed         bool
	writeBytes     atomic.Int64
	writes         atomic.Int64
	evictedBytes   atomic.Int64
}

// pendingEntry is a segment admitted into the write-back buffer but not yet on
// disk. gen names this admission of the key, so the writer can tell whether
// what it wrote is still the entry to rename into place; the bytes cannot
// answer that, since a caller may Set a buffer it has reused.
type pendingEntry struct {
	data    []byte
	gen     uint64
	modTime time.Time
}

func (e pendingEntry) header() CacheItemHeader {
	return CacheItemHeader{ModTime: e.modTime, Size: int64(len(e.data))}
}

// Stats is what the cache holds against what it may hold, and what it has done
// since the process started.
type Stats struct {
	// Items and Bytes are what is on disk, which is what MaxBytes limits;
	// what is still in the write-back buffer is WriteBackItems and
	// WriteBackBytes
	Items    int
	Bytes    int64
	MaxBytes int64

	// Write-back state: pending (not yet on disk) segments and their bytes
	WriteBackItems int
	WriteBackBytes int64

	// Disk work done by the write-back drain
	WriteBytes int64
	Writes     int64

	Hits      int64
	Misses    int64
	Evictions int64
	// EvictedBytes is bytes freed by eviction of disk-resident items, not by
	// callers dropping or re-setting their own entries
	EvictedBytes int64
}

// Item is an open handle on a cached item: the bytes of a segment still in
// write-back, or the file it was written to. Both answer positional reads, so a
// caller does not need to tell them apart.
type Item struct {
	file *os.File // nil while the item is still in write-back
	data []byte   // the bytes Set was given, while it is in write-back
}

func (i *Item) ReadAt(p []byte, off int64) (int, error) {
	// Checked for both, so an item behaves the same whether it is still pending
	// or already on disk
	if off < 0 {
		return 0, ErrNegativeOffset
	}
	if i.file != nil {
		return i.file.ReadAt(p, off)
	}
	if off >= int64(len(i.data)) {
		return 0, io.EOF
	}
	n := copy(p, i.data[off:])
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

func (i *Item) Close() error {
	if i.file != nil {
		return i.file.Close()
	}
	return nil
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
	// Max bytes of segment data held in memory awaiting a background write to
	// disk. 0 disables write-back and stores each item synchronously.
	WriteBackSize int64
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
