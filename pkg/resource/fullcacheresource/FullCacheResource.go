package fullcacheresource

import (
	"errors"
	"fmt"
	"io"
	"os"
	"sync"

	"git.ruekov.eu/ruakij/nzbStreamer/pkg/diskcache"
	"git.ruekov.eu/ruakij/nzbStreamer/pkg/resource"
)

var (
	mutexMapMutex sync.Mutex             = sync.Mutex{}
	mutexMap      map[string]*sync.Mutex = make(map[string]*sync.Mutex)
)

// keyMutex serializes fetch-and-store for a cache-key, so concurrent readers of the
// same segment download it once.
func keyMutex(key string) *sync.Mutex {
	mutexMapMutex.Lock()
	defer mutexMapMutex.Unlock()

	mu, exists := mutexMap[key]
	if !exists {
		mu = &sync.Mutex{}
		mutexMap[key] = mu
	}
	return mu
}

// FullCacheResource caches underlying Record by fully reading its content into cache
type FullCacheResource struct {
	UnderlyingResource resource.ReadCloseableResource
	CacheKey           string
	Cache              *diskcache.Cache
	cachedSize         int64
	cachedSizeExact    bool
	options            *FullCacheResourceOptions
}

type FullCacheResourceOptions struct {
	// Force lookup Size() from underlying resource, ignoring any Caches
	SizeAlwaysFromResource bool
}

func NewFullCacheResource(underlyingResource resource.ReadCloseableResource, cacheKey string, cache *diskcache.Cache, options *FullCacheResourceOptions) *FullCacheResource {
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
	// Cache-file kept open for the readers lifetime; reads are positional, so no seeking
	cacheFile *os.File
	index     int64
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

func (r *FullCacheResource) SizeHint() (int64, error) {
	size, _, err := r.size()
	return size, err
}

// Prefetch stores the content in the cache without handing out a reader. A
// demand read arriving later finds the file and does no fetch of its own.
func (r *FullCacheResource) Prefetch() error {
	mu := keyMutex(r.CacheKey)
	mu.Lock()
	defer mu.Unlock()

	if exists, _ := r.Cache.Exists(r.CacheKey); exists {
		return nil
	}

	underlyingReader, err := r.UnderlyingResource.Open()
	if err != nil {
		return fmt.Errorf("failed opening underlying resource: %w", err)
	}
	size, err := r.Cache.SetWithReader(r.CacheKey, underlyingReader)
	if err != nil {
		return fmt.Errorf("failed storing item in cache: %w", errors.Join(err, underlyingReader.Close()))
	}
	if err := underlyingReader.Close(); err != nil {
		return fmt.Errorf("failed closing underlying reader: %w", err)
	}

	r.cachedSize = size
	r.cachedSizeExact = true

	return nil
}

// Size is exact once the content sits in the cache, since the cache header then
// records what was actually stored.
func (r *FullCacheResource) Size() (int64, error) {
	size, exact, err := r.size()
	if err != nil {
		return 0, err
	}
	if !exact {
		return 0, resource.ErrSizeNotExact
	}

	return size, nil
}

func (r *FullCacheResource) size() (size int64, exact bool, err error) {
	mu := keyMutex(r.CacheKey)
	mu.Lock()
	defer mu.Unlock()

	if r.cachedSizeExact {
		return r.cachedSize, true, nil
	}

	if !r.options.SizeAlwaysFromResource {
		if exists, header := r.Cache.Exists(r.CacheKey); exists {
			r.cachedSize = header.Size
			r.cachedSizeExact = true
			return r.cachedSize, true, nil
		}
	}

	// Not cached yet, so only the underlying resource can answer
	if sized, ok := r.UnderlyingResource.(resource.Sized); ok {
		if size, err := sized.Size(); err == nil {
			r.cachedSize = size
			r.cachedSizeExact = true
			return size, true, nil
		} else if !errors.Is(err, resource.ErrSizeNotExact) {
			return 0, false, fmt.Errorf("failed getting size from underlying resource: %w", err)
		}
	}

	if r.cachedSize >= 0 {
		return r.cachedSize, false, nil
	}

	size, err = r.UnderlyingResource.SizeHint()
	if err != nil {
		return 0, false, fmt.Errorf("failed getting size-hint from underlying resource: %w", err)
	}

	r.cachedSize = size
	return size, false, nil
}

func (r *FullCacheResourceReader) Close() error {
	if r.cacheFile != nil {
		err := r.cacheFile.Close()
		r.cacheFile = nil
		if err != nil {
			return fmt.Errorf("failed closing cache-file: %w", err)
		}
	}
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
		resourceSize, err := r.resource.Size()
		if errors.Is(err, resource.ErrSizeNotExact) {
			// Only reading the content settles the size
			if _, err := io.CopyN(io.Discard, r, 1); err != nil {
				return 0, fmt.Errorf("failed reading from underlying reader: %w", err)
			}
			resourceSize, err = r.resource.Size()
		}
		if err != nil {
			return 0, err
		}
		newIndex = resourceSize + offset
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

	if r.cacheFile == nil {
		if err := r.openCacheFile(); err != nil {
			return 0, err
		}
	}

	n, err := r.cacheFile.ReadAt(p, r.index)
	r.index += int64(n)

	return n, err
}

// openCacheFile ensures the segment is cached and keeps its file open for subsequent reads.
func (r *FullCacheResourceReader) openCacheFile() error {
	mu := keyMutex(r.resource.CacheKey)
	mu.Lock()
	defer mu.Unlock()

	file, size, err := r.resource.Cache.Open(r.resource.CacheKey)
	if errors.Is(err, diskcache.ErrItemNotFound) {
		// Item was evicted between reads, or was never fetched
		if r.underlyingReader == nil {
			r.underlyingReader, err = r.resource.UnderlyingResource.Open()
			if err != nil {
				return fmt.Errorf("failed reopening underlying resource: %w", err)
			}
		}

		if _, err := r.resource.Cache.SetWithReader(r.resource.CacheKey, r.underlyingReader); err != nil {
			return err
		}
		// Free resources, we wont need it anymore
		if err := r.underlyingReader.Close(); err != nil {
			return fmt.Errorf("failed closing underlying reader: %w", err)
		}
		r.underlyingReader = nil

		file, size, err = r.resource.Cache.Open(r.resource.CacheKey)
		if err != nil {
			return fmt.Errorf("failed getting item from cache immediately after writing: %w", err)
		}
	} else if err != nil {
		return fmt.Errorf("failed getting item from cache: %w", err)
	}

	r.cacheFile = file
	r.resource.cachedSize = size
	r.resource.cachedSizeExact = true

	return nil
}
