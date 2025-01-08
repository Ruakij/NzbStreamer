package diskCache

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"
)

func NewCache(options *CacheOptions) (*Cache, error) {
	if options.MaxSize < 0 || options.ItemMaxSize < 0 || options.CacheDir == "" {
		return nil, errors.New("invalid cache settings")
	}

	if err := ensureDirExists(options.CacheDir); err != nil {
		return nil, err
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
	if cache.currentSize > cache.options.MaxSize {
		cache.maxSizeEvict(0)
	}

	return cache, nil
}

func (c *Cache) loadExistingItems() error {
	files, err := os.ReadDir(c.options.CacheDir)
	if err != nil {
		return err
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	for _, file := range files {
		filePath := filepath.Join(c.options.CacheDir, file.Name())
		info, err := os.Stat(filePath)
		if err != nil {
			continue
		}

		c.items[file.Name()] = CacheItemHeader{
			lock:    &sync.RWMutex{},
			ModTime: info.ModTime(),
			Size:    info.Size(),
		}
		c.currentSize += info.Size()
	}

	return nil
}

func (c *Cache) maxSizeEvict(requiredSpace int64) error {
	for c.options.MaxSize-c.currentSize < requiredSpace {
		key := c.options.EvictPolicyHook(c.items)
		if key == "" {
			return errors.New("coult not make required space")
		}

		err := c.removeFile(key)
		if err != nil {
			return err
		}
	}
	return nil
}

func (c *Cache) SetWithReader(key string, reader io.Reader) (int64, error) {
	// Define the path for the temporary file
	tempFilePath := filepath.Join(c.options.TmpCacheDir, key)

	file, err := os.Create(tempFilePath)
	if err != nil {
		return 0, err
	}
	defer func() {
		file.Close()
		// Clean up the temporary file in case of an error
		if err != nil {
			os.Remove(tempFilePath)
		}
	}()

	limit := c.options.MaxSize - c.currentSize
	if c.options.ItemMaxSize > 0 && limit > c.options.ItemMaxSize {
		limit = c.options.ItemMaxSize
	}

	var limitReader io.Reader
	if c.options.MaxSize > 0 || c.options.ItemMaxSize > 0 {
		limitReader = io.LimitReader(reader, limit)
	} else {
		limitReader = reader
	}

	n, err := io.Copy(file, limitReader)
	if err != nil {
		return 0, err
	}

	if err := file.Sync(); err != nil {
		return 0, err
	}

	// Atomic rename
	finalFilePath := filepath.Join(c.options.CacheDir, key)
	err = os.Rename(tempFilePath, finalFilePath)
	if err != nil {
		return 0, err
	}

	// Successfully updated, update header
	c.mu.Lock()
	header, exists := c.items[key]
	if !exists {
		header = CacheItemHeader{
			lock:    &sync.RWMutex{},
			ModTime: time.Now(),
		}
	}
	header.Size = n
	c.items[key] = header
	c.mu.Unlock()

	c.mu.Lock()
	c.currentSize += n
	c.mu.Unlock()

	return n, nil
}

func (c *Cache) Set(key string, data []byte) (int64, error) {
	return c.SetWithReader(key, bytes.NewReader(data))
}

func (c *Cache) Remove(key string) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	header, exists := c.items[key]
	if !exists {
		return errors.New("item not found")
	}

	header.lock.Lock()
	defer header.lock.Unlock()

	return c.removeFile(key)
}

func (c *Cache) removeFile(key string) error {
	filePath := filepath.Join(c.options.CacheDir, key)
	if _, exists := c.items[key]; exists {
		if err := os.Remove(filePath); err != nil {
			return err
		}
		c.currentSize -= c.items[key].Size
		delete(c.items, key)
	}
	return nil
}

func (c *Cache) GetWithReader(key string) (io.ReadSeekCloser, *CacheItemHeader, error) {
	c.mu.Lock()

	header, exists := c.items[key]
	if !exists {
		c.mu.Unlock()
		return nil, nil, errors.New("item not found")
	}
	filePath := filepath.Join(c.options.CacheDir, key)

	header.lock.RLock()

	// Update access-time
	header.ModTime = time.Now()
	c.items[key] = header

	c.mu.Unlock()

	// Update access-time on disk
	err := os.Chtimes(filePath, header.ModTime, header.ModTime)
	if err != nil {
		header.lock.RUnlock()
		return nil, nil, err
	}

	file, err := os.Open(filePath)
	if err != nil {
		return nil, nil, err
	}

	return &CacheItemReader{
		lock:             header.lock,
		underlyingReader: file,
	}, &header, nil
}

func (c *Cache) Get(key string) ([]byte, *CacheItemHeader, error) {
	reader, header, err := c.GetWithReader(key)
	if err != nil {
		return nil, nil, err
	}
	defer reader.Close()

	data, err := io.ReadAll(reader)
	return data, header, err
}

func (c *Cache) Exists(key string) (bool, CacheItemHeader) {
	c.mu.RLock()
	header, exists := c.items[key]
	c.mu.RUnlock()

	return exists, header
}
