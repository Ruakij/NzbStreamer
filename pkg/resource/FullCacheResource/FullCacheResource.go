package FullCacheResource

import (
	"context"
	"fmt"
	"io"
	"sync"

	"git.ruekov.eu/ruakij/nzbStreamer/pkg/resource"
	"github.com/eko/gocache/lib/v4/cache"
)

var mutexMapMutex sync.Mutex = sync.Mutex{}
var mutexMap map[string]*sync.Mutex = make(map[string]*sync.Mutex)

// FullCacheResource caches underlying Record by fully reading its content into cache
type FullCacheResource struct {
	UnderlyingResource resource.ReadableResource
	CacheKey           string
	Cache              cache.CacheInterface[[]byte]
	options            *FullCacheResourceOptions
}

type FullCacheResourceOptions struct {
	// Check for size mismatch when reading between read Size and reported Size by underlying resource
	CheckSizeMismatch bool
	// Force lookup Size() from underlying resource, ignoring any Caches
	SizeAlwaysFromResource bool
}

func NewFullCacheResource(underlyingResource resource.ReadableResource, cacheKey string, Cache cache.CacheInterface[[]byte], options *FullCacheResourceOptions) *FullCacheResource {
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
	}
}

type FullCacheResourceReader struct {
	resource         *FullCacheResource
	underlyingReader io.Reader
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

	if !r.options.SizeAlwaysFromResource {
		data, err := r.Cache.Get(context.Background(), r.CacheKey)
		if err == nil {
			return int64(len(data)), nil
		}
	}

	return r.UnderlyingResource.Size()
}

func (r *FullCacheResourceReader) Close() (err error) {
	r.ctx_cancel()
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
	n := len(p)
	if n <= 0 {
		return 0, nil
	}

	mutexMapMutex.Lock()
	mu, _ := mutexMap[r.resource.CacheKey]
	mutexMapMutex.Unlock()

	mu.Lock()
	defer mu.Unlock()

	data, err := r.resource.Cache.Get(r.ctx, r.resource.CacheKey)
	if err != nil {
		data, err = io.ReadAll(r.underlyingReader)
		if err != nil {
			return 0, err
		}

		sizeRead := int64(len(data))
		// Size plausability check
		if r.resource.options.CheckSizeMismatch {
			size, err := r.resource.Size()
			if err != nil {
				return int(size), err
			}

			if sizeRead != size {
				return int(size), fmt.Errorf("sizeData=%d and Size()=%d mismatch", sizeRead, size)
			}
		}

		r.resource.Cache.Set(r.ctx, r.resource.CacheKey, data)
	}

	n = copy(p, data[r.index:])

	// If we copied less, it means there isnt anything else there
	if n < len(p) {
		err = io.EOF
	}

	r.index += int64(n)

	return n, err
}
