// Package diskcache stores byte blobs on disk under path-like keys, with
// pluggable lru or fifo eviction.
package diskcache

import (
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"git.ruekov.eu/ruakij/nzbStreamer/pkg/bytesize"
)

var ErrInvalidCacheOptions = errors.New("invalid cache settings")

func NewCache(options *CacheOptions) (*Cache, error) {
	if options.MaxSize < 0 || options.ItemMaxSize < 0 || options.WriteBackSize < 0 || options.CacheDir == "" {
		return nil, ErrInvalidCacheOptions
	}

	if err := ensureDirExists(options.CacheDir); err != nil {
		return nil, fmt.Errorf("failed creating all dirs: %w", err)
	}

	if options.TmpCacheDir == "" {
		options.TmpCacheDir = filepath.Join(options.CacheDir, ".tmp")
	}
	if err := ensureDirExists(options.TmpCacheDir); err != nil {
		return nil, err
	}
	if err := clearDirectory(options.TmpCacheDir); err != nil {
		return nil, err
	}

	if options.EvictPolicyHook == nil {
		options.EvictPolicyHook = defaultCacheOptions.EvictPolicyHook
	}

	cache := &Cache{
		mu:      &sync.RWMutex{},
		options: options,
		items:   make(map[string]CacheItemHeader),
		indexed: make(chan struct{}),
	}

	if options.WriteBackSize > 0 {
		cache.writeBack = make(map[string]pendingEntry)
		cache.writeBackCond = sync.NewCond(cache.mu)
		cache.writeWG.Add(1)
		go cache.writeBackLoop()
	}

	go cache.index()

	return cache, nil
}

// Indexed is closed once what was on disk at startup has been counted. Until
// then the cache serves and stores normally; it just knows about less than it
// holds, so it evicts less than it could.
func (c *Cache) Indexed() <-chan struct{} {
	return c.indexed
}

func (c *Cache) index() {
	defer close(c.indexed)

	slog.Debug("Indexing cache", "dir", c.options.CacheDir)
	start := time.Now()

	if err := c.loadExistingItems(); err != nil {
		slog.Error("Failed indexing cache", "dir", c.options.CacheDir, "error", err)
		return
	}

	stats := c.Stats()
	slog.Info("Cache indexed", "items", stats.Items, "bytes", bytesize.Bytes(stats.Bytes), "max bytes", bytesize.Bytes(stats.MaxBytes), "took", time.Since(start))

	if stats.MaxBytes > 0 && stats.Bytes > stats.MaxBytes {
		c.mu.Lock()
		defer c.mu.Unlock()

		if err := c.maxSizeEvict(0); err != nil {
			slog.Error("Failed evicting down to the cache size limit", "bytes", bytesize.Bytes(stats.Bytes), "max bytes", bytesize.Bytes(stats.MaxBytes), "error", err)
		}
	}
}

// loadExistingItems walks the cache dir, since a key may name a subdirectory.
// The key of an item is its path relative to the cache dir. It takes the lock
// per item rather than for the whole walk, so a read does not wait for it.
func (c *Cache) loadExistingItems() error {
	err := filepath.WalkDir(c.options.CacheDir, func(itemPath string, entry fs.DirEntry, err error) error {
		if err != nil {
			// Something removed while the walk runs is not an error
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		if entry.IsDir() {
			if itemPath == c.options.TmpCacheDir {
				return fs.SkipDir
			}
			return nil
		}

		info, err := entry.Info()
		if err != nil {
			return nil //nolint:nilerr
		}

		key := filepath.ToSlash(strings.TrimPrefix(itemPath, c.options.CacheDir+string(filepath.Separator)))

		c.mu.Lock()
		defer c.mu.Unlock()

		// One written while the walk runs is already counted, and one still
		// pending is counted by the finalize that will rename over this file
		if _, exists := c.items[key]; exists {
			return nil
		}
		if _, pending := c.writeBack[key]; pending {
			return nil
		}
		c.items[key] = CacheItemHeader{
			ModTime: info.ModTime(),
			Size:    info.Size(),
		}
		c.currentSize += info.Size()
		return nil
	})
	if err != nil {
		return fmt.Errorf("failed reading dir: %w", err)
	}

	return nil
}

var (
	ErrCouldNotMakeEnoughSpace = errors.New("could not make required space")
	ErrItemNotFound            = errors.New("item not found")
	ErrNegativeOffset          = errors.New("negative offset")
)

// maxSizeEvict frees requiredSpace. One new item usually displaces one old one,
// which a scan for the oldest answers; anything wanting more than that orders
// the whole map once rather than scanning it again per victim.
func (c *Cache) maxSizeEvict(requiredSpace int64) error {
	if c.options.MaxSize-c.currentSize >= requiredSpace {
		return nil
	}

	key := c.options.EvictPolicyHook(c.items)
	if key == "" {
		return ErrCouldNotMakeEnoughSpace
	}
	if err := c.evict(key); err != nil {
		return err
	}

	if c.options.MaxSize-c.currentSize >= requiredSpace {
		return nil
	}

	keys := slices.SortedFunc(maps.Keys(c.items), func(a, b string) int {
		return c.items[a].ModTime.Compare(c.items[b].ModTime)
	})
	for _, key := range keys {
		if err := c.evict(key); err != nil {
			return err
		}
		if c.options.MaxSize-c.currentSize >= requiredSpace {
			return nil
		}
	}

	return ErrCouldNotMakeEnoughSpace
}

// evict removes an item to make room, which is the removal worth counting: a
// caller dropping what it stored itself is not the cache running out of space.
func (c *Cache) evict(key string) error {
	evicted := c.items[key].Size
	if err := c.removeFile(key); err != nil {
		return err
	}
	c.evictions.Add(1)
	c.evictedBytes.Add(evicted)
	return nil
}

// evictFor makes room for size bytes, blocking or in the background as
// configured. A cache without a limit evicts nothing.
func (c *Cache) evictFor(size int64) error {
	if c.options.MaxSize <= 0 {
		return nil
	}
	if !c.options.MaxSizeEvictBlocking {
		go func() {
			c.mu.Lock()
			err := c.maxSizeEvict(size)
			c.mu.Unlock()
			if err != nil {
				slog.Error("Couldnt evict for item", "wanted space", bytesize.Bytes(size), "error", err)
			}
		}()

		return nil
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	return c.maxSizeEvict(size)
}

// Set stores data as it is, so a caller that already holds the whole item does
// not copy it through a read buffer first. With write-back enabled it admits
// the segment into memory and returns immediately; otherwise it writes to disk
// synchronously.
//
// data belongs to the cache from here on: until it reaches disk it is what
// Open serves, so a caller must not write to the buffer it passed.
func (c *Cache) Set(key Key, data []byte) (int64, error) {
	if c.options.WriteBackSize > 0 {
		return c.admit(key, data)
	}
	return c.store(key, func(file *os.File) (int64, error) {
		if err := c.evictFor(int64(len(data))); err != nil {
			return 0, err
		}
		n, err := file.Write(data)
		if err != nil {
			return int64(n), fmt.Errorf("failed writing item: %w", err)
		}

		return int64(n), nil
	})
}

// store writes an item through a temp file and renames it into place, so a
// reader never sees a partial one.
func (c *Cache) store(key Key, write func(*os.File) (int64, error)) (int64, error) {
	finalFilePath, err := key.path(c.options.CacheDir)
	if err != nil {
		return 0, err
	}

	// The temp file is named by the cache rather than by the key, which may name
	// a subdirectory the tmp dir does not have
	file, err := os.CreateTemp(c.options.TmpCacheDir, "")
	if err != nil {
		return 0, fmt.Errorf("failed creating temp file: %w", err)
	}
	tempFilePath := file.Name()
	defer func() {
		file.Close()
		// Clean up the temporary file in case of an error
		if err != nil {
			os.Remove(tempFilePath)
		}
	}()

	totalWritten, err := write(file)
	if err != nil {
		return totalWritten, err
	}

	if err := file.Sync(); err != nil {
		return totalWritten, fmt.Errorf("failed syncing file: %w", err)
	}

	if err = ensureDirExists(filepath.Dir(finalFilePath)); err != nil {
		return totalWritten, err
	}
	err = os.Rename(tempFilePath, finalFilePath)
	if err != nil {
		return totalWritten, fmt.Errorf("faile drenaming file: %w", err)
	}

	// Successfully updated, update header
	c.mu.Lock()
	header, exists := c.items[key.String()]
	if !exists {
		header = CacheItemHeader{ModTime: time.Now()}
	} else {
		// Replacing what is already counted, which the startup index may have
		// been what counted
		c.currentSize -= header.Size
	}
	header.Size = totalWritten
	c.items[key.String()] = header
	c.currentSize += totalWritten
	c.mu.Unlock()

	return totalWritten, nil
}

func (c *Cache) Remove(key Key) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	keyStr := key.String()
	if pending, exists := c.writeBack[keyStr]; exists {
		c.dropPendingLocked(keyStr, pending)
		return nil
	}

	header, exists := c.items[keyStr]
	if !exists {
		return ErrItemNotFound
	}

	return c.removeFileLocked(keyStr, header)
}

// RemoveAll drops every item whose key sits under prefix
func (c *Cache) RemoveAll(prefix Key) error {
	dirPath, err := prefix.path(c.options.CacheDir)
	if err != nil {
		return err
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	// os.RemoveAll on a path that does not exist is not an error, so a prefix
	// that has only pending (never yet written) entries still succeeds; it has
	// already removed every disk file, so the loop only has to drop the index
	// and any pending copies
	if err := os.RemoveAll(dirPath); err != nil {
		return fmt.Errorf("removing dir failed: %w", err)
	}

	for key, header := range c.items {
		if strings.HasPrefix(key, prefix.String()+"/") {
			c.currentSize -= header.Size
			delete(c.items, key)
		}
	}
	for key, pending := range c.writeBack {
		if strings.HasPrefix(key, prefix.String()+"/") {
			c.dropPendingLocked(key, pending)
		}
	}

	return nil
}

// removeFile takes the joined key of the items map, which is what the eviction
// policy hook picks from.
func (c *Cache) removeFile(key string) error {
	header, exists := c.items[key]
	if !exists {
		return nil
	}
	return c.removeFileLocked(key, header)
}

// removeFileLocked drops a known disk-resident item. It must hold c.mu.
func (c *Cache) removeFileLocked(key string, header CacheItemHeader) error {
	filePath, err := Key(strings.Split(key, "/")).path(c.options.CacheDir)
	if err != nil {
		return err
	}

	if err := os.Remove(filePath); err != nil {
		return fmt.Errorf("removing file failed: %w", err)
	}
	c.currentSize -= header.Size
	delete(c.items, key)

	// The last item leaves its directory behind; a directory still
	// holding items fails this and stays
	if dir := filepath.Dir(filePath); dir != c.options.CacheDir {
		os.Remove(dir)
	}
	return nil
}

// Open returns a handle on the item and its size. Callers may hold the handle
// for as long as they like: eviction only unlinks, so an open descriptor keeps
// working, and a pending entry is served from its immutable in-memory copy.
func (c *Cache) Open(key Key) (*Item, int64, error) {
	c.mu.Lock()
	// A pending entry carries its own header, which the finalize that puts it
	// on disk hands on, so this read counts for its LRU standing there too
	if pending, exists := c.writeBack[key.String()]; exists {
		c.hits.Add(1)
		pending.modTime = time.Now()
		c.writeBack[key.String()] = pending
		c.mu.Unlock()
		return &Item{data: pending.data}, int64(len(pending.data)), nil
	}

	header, exists := c.items[key.String()]
	if !exists {
		c.mu.Unlock()
		c.misses.Add(1)
		return nil, 0, ErrItemNotFound
	}
	c.hits.Add(1)
	header.ModTime = time.Now()
	c.items[key.String()] = header
	c.mu.Unlock()

	filePath, err := key.path(c.options.CacheDir)
	if err != nil {
		return nil, 0, err
	}

	// Mirror access-time to disk so LRU order survives a restart
	if err := os.Chtimes(filePath, header.ModTime, header.ModTime); err != nil {
		return nil, 0, fmt.Errorf("failed changing access&modification times: %w", err)
	}

	file, err := os.Open(filePath)
	if err != nil {
		return nil, 0, fmt.Errorf("failed opening file for item '%s': %w", key, err)
	}

	return &Item{file: file}, header.Size, nil
}

// Stats reports what the cache holds against what it may hold. Every number is
// tracked in memory, so this costs a lock and no syscalls.
func (c *Cache) Stats() Stats {
	c.mu.RLock()
	defer c.mu.RUnlock()

	return Stats{
		Items:    len(c.items),
		Bytes:    c.currentSize,
		MaxBytes: c.options.MaxSize,

		WriteBackItems: len(c.writeBack),
		WriteBackBytes: c.writeBackBytes,

		WriteBytes: c.writeBytes.Load(),
		Writes:     c.writes.Load(),

		Hits:         c.hits.Load(),
		Misses:       c.misses.Load(),
		Evictions:    c.evictions.Load(),
		EvictedBytes: c.evictedBytes.Load(),
	}
}

// Close stops the writer, drains any remaining write-back entries to disk, and
// waits for the writer goroutine to finish, so a caller can rely on a
// deterministic disk state and no leaked goroutines. It is a no-op when
// write-back is disabled.
func (c *Cache) Close() error {
	if c.writeBackCond == nil {
		return nil
	}

	c.writeBackCond.L.Lock()
	c.closed = true
	c.writeBackCond.Broadcast()
	c.writeBackCond.L.Unlock()

	c.writeWG.Wait()

	// Anything the writer did not drain before stopping is written now
	c.mu.Lock()
	remaining := make(map[string]pendingEntry, len(c.writeBack))
	for k, entry := range c.writeBack {
		remaining[k] = entry
	}
	c.mu.Unlock()

	for k, entry := range remaining {
		c.writePending(k, entry)
	}

	return nil
}

// Groups reports what the cache holds per first key part, which is one entry
// per nzb given a key of {nzb, message-id}. A segment awaiting its write is
// held for that nzb as much as one on disk, so both count. There is no index by
// prefix, so it is one pass for every group rather than a pass per group.
//
// ponytail: O(items) per call, which is a poll of a page against a map of
// segments; an index per prefix if that ever shows up in a profile
func (c *Cache) Groups() map[string]GroupStats {
	c.mu.RLock()
	defer c.mu.RUnlock()

	groups := make(map[string]GroupStats)
	count := func(key string, header CacheItemHeader) {
		prefix, _, isGrouped := strings.Cut(key, "/")
		if !isGrouped {
			return
		}

		group := groups[prefix]
		group.Items++
		group.Bytes += header.Size
		if header.ModTime.After(group.LastRead) {
			group.LastRead = header.ModTime
		}
		groups[prefix] = group
	}

	for key, header := range c.items {
		count(key, header)
	}
	for key, pending := range c.writeBack {
		count(key, pending.header())
	}

	return groups
}

// Exists reports whether the cache holds the key, whether on disk or still
// awaiting its write.
func (c *Cache) Exists(key Key) (bool, CacheItemHeader) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	if pending, exists := c.writeBack[key.String()]; exists {
		return true, pending.header()
	}
	header, exists := c.items[key.String()]

	return exists, header
}
