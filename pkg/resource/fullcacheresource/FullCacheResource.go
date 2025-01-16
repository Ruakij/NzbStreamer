package fullcacheresource

import (
	"errors"
	"fmt"
	"io"
	"sync"

	"git.ruekov.eu/ruakij/nzbStreamer/pkg/diskcache"
	"git.ruekov.eu/ruakij/nzbStreamer/pkg/resource"
)

var (
	mutexMapMutex sync.Mutex             = sync.Mutex{}
	mutexMap      map[string]*sync.Mutex = make(map[string]*sync.Mutex)
)

// FullCacheResource caches underlying Record by fully reading its content into cache
type FullCacheResource struct {
	UnderlyingResource       resource.ReadCloseableResource
	CacheKey                 string
	Cache                    *diskcache.Cache
	cachedSize               int64
	cachedSizeAccurate       bool
	cachedSizeAccurateCached bool
	options                  *FullCacheResourceOptions
}

type FullCacheResourceOptions struct {
	// Force lookup Size() from underlying resource, ignoring any Caches
	SizeAlwaysFromResource bool
}

func NewFullCacheResource(underlyingResource resource.ReadCloseableResource, cacheKey string, cache *diskcache.Cache, options *FullCacheResourceOptions) *FullCacheResource {
	// Create cache-keyed mutex if not exists
	mutexMapMutex.Lock()
	_, exists := mutexMap[cacheKey]
	if !exists {
		mutexMap[cacheKey] = &sync.Mutex{}
	}
	mutexMapMutex.Unlock()

	return &FullCacheResource{
		UnderlyingResource: underlyingResource,
		options:            options,
		CacheKey:           cacheKey,
		Cache:              cache,
		cachedSize:         -1,
	}
}

type FullCacheResourceReader struct {
	resource         *FullCacheResource
	underlyingReader io.ReadCloser
	index            int64
}

func (r *FullCacheResource) Open() (io.ReadSeekCloser, error) {
	underlyingReader, err := r.UnderlyingResource.Open()
	if err != nil {
		return nil, fmt.Errorf("failed opening underlying resource: %w", err)
	}

	return &FullCacheResourceReader{
		resource:         r,
		underlyingReader: underlyingReader,
	}, nil
}

func (r *FullCacheResource) Size() (int64, error) {
	mutexMapMutex.Lock()
	mu := mutexMap[r.CacheKey]
	mutexMapMutex.Unlock()

	mu.Lock()
	defer mu.Unlock()

	// Check if size is cached
	if r.cachedSize >= 0 {
		return r.cachedSize, nil
	}

	if !r.options.SizeAlwaysFromResource {
		exists, header := r.Cache.Exists(r.CacheKey)
		if exists {
			r.cachedSize = header.Size
			r.cachedSizeAccurateCached = true
			r.cachedSizeAccurate = true
			return r.cachedSize, nil
		}
	}

	// Fetching size from the underlying resource
	size, err := r.UnderlyingResource.Size()
	if err != nil {
		return 0, fmt.Errorf("failed getting size from underlying resource: %w", err)
	}

	r.cachedSize = size
	return size, nil
}

// IsSizeAccurate checks if the underlying reader supports accurate size reporting.
func (r *FullCacheResource) IsSizeAccurate() bool {
	mutexMapMutex.Lock()
	mu := mutexMap[r.CacheKey]
	mutexMapMutex.Unlock()

	mu.Lock()
	defer mu.Unlock()

	sizeAccurateResource, ok := r.UnderlyingResource.(resource.SizeAccurateResource)
	if !ok {
		// If it doesnt support it, default to true
		return true
	}

	if r.cachedSizeAccurateCached {
		return r.cachedSizeAccurate
	}

	if !r.options.SizeAlwaysFromResource {
		exists, header := r.Cache.Exists(r.CacheKey)
		if exists {
			r.cachedSize = header.Size
			r.cachedSizeAccurateCached = true
			r.cachedSizeAccurate = true
			return r.cachedSizeAccurate
		}
	}

	// Get from underlying
	r.cachedSizeAccurate = sizeAccurateResource.IsSizeAccurate()
	r.cachedSizeAccurateCached = true
	return r.cachedSizeAccurate
}

func (r *FullCacheResourceReader) Close() error {
	if r.underlyingReader != nil {
		err := r.underlyingReader.Close()
		r.underlyingReader = nil
		if err != nil {
			return fmt.Errorf("failed closing underlying reader: %w", err)
		}
	}
	return nil
}

func (r *FullCacheResourceReader) Seek(offset int64, whence int) (int64, error) {
	var newIndex int64

	switch whence {
	case io.SeekStart:
		newIndex = offset
	case io.SeekCurrent:
		newIndex = r.index + offset
	case io.SeekEnd:
		// If size not accurate, needs to trigger read to get accurate size
		if !r.resource.IsSizeAccurate() {
			_, err := io.CopyN(io.Discard, r, 1)
			if err != nil {
				return 0, fmt.Errorf("failed reading from underlying reader: %w", err)
			}
		}
		resourceSize, err := r.resource.Size()
		if err != nil {
			return 0, err
		}
		newIndex = resourceSize - offset
	default:
		return 0, resource.ErrInvalidSeek
	}

	// Seek to same pos we are at
	if newIndex == r.index {
		return r.index, nil
	}
	// Out of range
	if newIndex < 0 {
		return 0, resource.ErrInvalidSeek
	}

	r.index = newIndex
	return r.index, nil
}

func (r *FullCacheResourceReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if r.resource.cachedSize > 0 && r.index >= r.resource.cachedSize {
		return 0, io.EOF
	}

	mutexMapMutex.Lock()
	mu := mutexMap[r.resource.CacheKey]
	mutexMapMutex.Unlock()

	mu.Lock()
	defer mu.Unlock()

	reader, header, err := r.resource.Cache.GetWithReader(r.resource.CacheKey)
	if errors.Is(err, diskcache.ErrItemNotFound) {
		n, err := r.resource.Cache.SetWithReader(r.resource.CacheKey, r.underlyingReader)
		if err != nil {
			return int(n), err
		}
		// Free resources, we wont need it anymore
		if err := r.underlyingReader.Close(); err != nil {
			return 0, fmt.Errorf("failed closing underlying reader: %w", err)
		}
		r.underlyingReader = nil

		reader, header, err = r.resource.Cache.GetWithReader(r.resource.CacheKey)
		if err != nil {
			return 0, fmt.Errorf("failed getting item from cache immediately after writing: %w", err)
		}
	} else if err != nil {
		return 0, fmt.Errorf("failed getting item from cache: %w", err)
	}

	defer reader.Close()

	_, err = reader.Seek(r.index, io.SeekStart)
	if err != nil {
		return 0, fmt.Errorf("failed seeking cache-reader to %d: %w", r.index, err)
	}

	n, err := reader.Read(p)
	r.index += int64(n)

	// Update cachedSize on read
	r.resource.cachedSize = header.Size
	r.resource.cachedSizeAccurate = true
	r.resource.cachedSizeAccurateCached = true

	return n, err
}
