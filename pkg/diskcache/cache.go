package diskcache

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"
)

var ErrInvalidCacheOptions = errors.New("invalid cache settings")

func NewCache(options *CacheOptions) (*Cache, error) {
	if options.MaxSize < 0 || options.ItemMaxSize < 0 || options.CacheDir == "" {
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
	}

	if err := cache.loadExistingItems(); err != nil {
		return nil, err
	}

	// Run sizeEvict, when current size is too large for maxSize
	if cache.options.MaxSize > 0 && cache.currentSize > cache.options.MaxSize {
		err := cache.maxSizeEvict(0)
		if err != nil {
			return nil, fmt.Errorf("failed initial evicting: %w", err)
		}
	}

	return cache, nil
}

func (c *Cache) loadExistingItems() error {
	files, err := os.ReadDir(c.options.CacheDir)
	if err != nil {
		return fmt.Errorf("failed reading dir: %w", err)
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	for _, file := range files {
		filePath := filepath.Join(c.options.CacheDir, file.Name())
		info, err := os.Stat(filePath)
		if err != nil {
			continue
		}
		if info.IsDir() {
			continue
		}

		c.items[file.Name()] = CacheItemHeader{
			ModTime: info.ModTime(),
			Size:    info.Size(),
		}
		c.currentSize += info.Size()
	}

	return nil
}

var (
	ErrCouldNotMakeEnoughSpace = errors.New("could not make required space")
	ErrItemNotFound            = errors.New("item not found")
)

func (c *Cache) maxSizeEvict(requiredSpace int64) error {
	for c.options.MaxSize-c.currentSize < requiredSpace {
		key := c.options.EvictPolicyHook(c.items)
		if key == "" {
			return ErrCouldNotMakeEnoughSpace
		}

		if _, exists := c.items[key]; !exists {
			return ErrItemNotFound
		}

		if err := c.removeFile(key); err != nil {
			return err
		}
	}
	return nil
}

const ReadBufferSize = 1024 * 1024 // 1MB buffer for reading, adjust size as needed

func (c *Cache) SetWithReader(key string, reader io.Reader) (int64, error) {
	// Define the path for the temporary file
	tempFilePath := filepath.Join(c.options.TmpCacheDir, key)

	file, err := os.Create(tempFilePath)
	if err != nil {
		return 0, fmt.Errorf("failed creating file '%s': %w", tempFilePath, err)
	}
	defer func() {
		file.Close()
		// Clean up the temporary file in case of an error
		if err != nil {
			os.Remove(tempFilePath)
		}
	}()

	var totalWritten int64
	buf := make([]byte, ReadBufferSize)

	var totalN int64 = 0
	for {
		// Read a chunk
		n, readErr := reader.Read(buf)
		totalN += int64(n)
		if n > 0 {
			if c.options.MaxSize > 0 {
				if defaultCacheOptions.MaxSizeEvictBlocking {
					// Ensure there is enough space, evict if necessary
					c.mu.Lock()
					err = c.maxSizeEvict(totalN)
					c.mu.Unlock()
					if err != nil {
						return totalWritten, err
					}
				} else {
					go func(totalN int64) {
						// Ensure there is enough space, evict if necessary
						c.mu.Lock()
						err = c.maxSizeEvict(totalN)
						c.mu.Unlock()
						if err != nil {
							slog.Error("Couldnt evict for item", "wanted space", totalN, "error", err)
						}
					}(totalN)
				}
			}

			// Write the chunk
			nw, writeErr := file.Write(buf[:n])
			if writeErr != nil {
				return totalWritten, fmt.Errorf("failed writing chunk: %w", writeErr)
			}
			totalWritten += int64(nw)
		}

		// End of reader, or error
		if readErr != nil {
			if readErr == io.EOF {
				break
			}
			return totalWritten, fmt.Errorf("failed reading chunk: %w", readErr)
		}
	}

	if err := file.Sync(); err != nil {
		return totalWritten, fmt.Errorf("failed syncing file: %w", err)
	}

	finalFilePath := filepath.Join(c.options.CacheDir, key)
	err = os.Rename(tempFilePath, finalFilePath)
	if err != nil {
		return totalWritten, fmt.Errorf("faile drenaming file: %w", err)
	}

	// Successfully updated, update header
	c.mu.Lock()
	header, exists := c.items[key]
	if !exists {
		header = CacheItemHeader{ModTime: time.Now()}
	}
	header.Size = totalWritten
	c.items[key] = header
	c.currentSize += totalWritten
	c.mu.Unlock()

	return totalWritten, nil
}

func (c *Cache) Set(key string, data []byte) (int64, error) {
	return c.SetWithReader(key, bytes.NewReader(data))
}

func (c *Cache) Remove(key string) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if _, exists := c.items[key]; !exists {
		return ErrItemNotFound
	}

	return c.removeFile(key)
}

func (c *Cache) removeFile(key string) error {
	filePath := filepath.Join(c.options.CacheDir, key)
	if _, exists := c.items[key]; exists {
		if err := os.Remove(filePath); err != nil {
			return fmt.Errorf("removing file failed: %w", err)
		}
		c.currentSize -= c.items[key].Size
		delete(c.items, key)
	}
	return nil
}

// Open returns the item's file and size. Callers may hold the file for as long as
// they like: eviction only unlinks, so an open descriptor keeps working.
func (c *Cache) Open(key string) (*os.File, int64, error) {
	c.mu.Lock()
	header, exists := c.items[key]
	if !exists {
		c.mu.Unlock()
		return nil, 0, ErrItemNotFound
	}
	header.ModTime = time.Now()
	c.items[key] = header
	c.mu.Unlock()

	filePath := filepath.Join(c.options.CacheDir, key)

	// Mirror access-time to disk so LRU order survives a restart
	if err := os.Chtimes(filePath, header.ModTime, header.ModTime); err != nil {
		return nil, 0, fmt.Errorf("failed changing access&modification times: %w", err)
	}

	file, err := os.Open(filePath)
	if err != nil {
		return nil, 0, fmt.Errorf("failed opening file for item '%s': %w", key, err)
	}

	return file, header.Size, nil
}

// Stats reports what the cache holds against what it may hold. Both numbers are
// tracked in memory, so this costs a lock and no syscalls.
func (c *Cache) Stats() (items int, bytes, maxBytes int64) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	return len(c.items), c.currentSize, c.options.MaxSize
}

func (c *Cache) Exists(key string) (bool, CacheItemHeader) {
	c.mu.RLock()
	header, exists := c.items[key]
	c.mu.RUnlock()

	return exists, header
}
