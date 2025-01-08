package FullCacheResource

import (
	"context"
	"fmt"
	"io"
	"sync"

	"git.ruekov.eu/ruakij/nzbStreamer/pkg/diskCache"
	"git.ruekov.eu/ruakij/nzbStreamer/pkg/resource"
)

var mutexMapMutex sync.Mutex = sync.Mutex{}
var mutexMap map[string]*sync.Mutex = make(map[string]*sync.Mutex)

// FullCacheResource caches underlying Record by fully reading its content into cache
type FullCacheResource struct {
	UnderlyingResource       resource.ReadCloseableResource
	CacheKey           string
	Cache              *diskCache.Cache
	cachedSize         int64
	options            *FullCacheResourceOptions
}

type FullCacheResourceOptions struct {
	// Check for size mismatch when reading between read Size and reported Size by underlying resource
	CheckSizeMismatch bool
	// Force lookup Size() from underlying resource, ignoring any Caches
	SizeAlwaysFromResource bool
}

func NewFullCacheResource(underlyingResource resource.ReadCloseableResource, cacheKey string, Cache *diskCache.Cache, options *FullCacheResourceOptions) *FullCacheResource {
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
		Cache:              Cache,
		cachedSize:         -1,
	}
}

type FullCacheResourceReader struct {
	resource         *FullCacheResource
	underlyingReader io.ReadCloser
	index            int64
	ctx              context.Context
	ctx_cancel       context.CancelFunc
}

func (r *FullCacheResource) Open() (io.ReadSeekCloser, error) {
	underlyingReader, err := r.UnderlyingResource.Open()
	if err != nil {
		return nil, err
	}

	ctx, ctx_cancel := context.WithCancel(context.Background())
	return &FullCacheResourceReader{
		resource:         r,
		underlyingReader: underlyingReader,
		ctx:              ctx,
		ctx_cancel:       ctx_cancel,
	}, err
}

func (r *FullCacheResource) Size() (int64, error) {
	mutexMapMutex.Lock()
	mu, _ := mutexMap[r.CacheKey]
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
			return header.Size, nil
		}
	}

	// Fetching size from the underlying resource
	size, err := r.UnderlyingResource.Size()
	if err != nil {
		return 0, err
	}

	r.cachedSize = size
	return size, nil
}

func (r *FullCacheResourceReader) Close() (err error) {
	r.ctx_cancel()
	if r.underlyingReader != nil {
		err = r.underlyingReader.Close()
	}
	return
}

func (r *FullCacheResourceReader) Seek(offset int64, whence int) (int64, error) {
	var newIndex int64

	resourceSize, err := r.resource.Size()
	if err != nil {
		return 0, err
	}

	switch whence {
	case io.SeekStart:
		newIndex = offset
	case io.SeekCurrent:
		newIndex = r.index + offset
	case io.SeekEnd:
		newIndex = resourceSize + offset
	default:
		return 0, resource.ErrInvalidSeek
	}

	// Seek to same pos we are at
	if newIndex == r.index {
		return r.index, nil
	}
	// Out of range
	if newIndex < 0 || newIndex > resourceSize {
		return 0, resource.ErrInvalidSeek
	}

	r.index = newIndex
	return r.index, nil
}

func (r *FullCacheResourceReader) Read(p []byte) (int, error) {
	size, err := r.resource.Size()
	if err != nil {
		return 0, err
	}
	if r.index >= size {
		return 0, io.EOF
	}

	mutexMapMutex.Lock()
	mu, _ := mutexMap[r.resource.CacheKey]
	mutexMapMutex.Unlock()

	mu.Lock()
	defer mu.Unlock()

	reader, header, err := r.resource.Cache.GetWithReader(r.resource.CacheKey)
	if err != nil {
		n, err := r.resource.Cache.SetWithReader(r.resource.CacheKey, r.underlyingReader)
		if err != nil {
			return int(n), err
		}
		// Free resources, we wont need it anymore
		if err := r.underlyingReader.Close(); err != nil {
			return 0, err
		}
		r.underlyingReader = nil

		// Size plausability check
		if r.resource.options.CheckSizeMismatch {
			size, err := r.resource.Size()
			if err != nil {
				return int(size), err
			}

			if n != size {
				return int(size), fmt.Errorf("sizeData=%d and Size()=%d mismatch", n, size)
			}
		}

		reader, header, _ = r.resource.Cache.GetWithReader(r.resource.CacheKey)
	}

	defer reader.Close()

	_, err = reader.Seek(r.index, io.SeekStart)
	if err != nil {
		return 0, err
	}

	n, err := reader.Read(p)
	r.index += int64(n)

	// Update cachedSize on read
	r.resource.cachedSize = header.Size

	if r.index >= header.Size {
		err = io.EOF
	}

	return n, err
}
