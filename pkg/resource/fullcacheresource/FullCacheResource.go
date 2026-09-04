// Package fullcacheresource fetches its underlying resource fully up front,
// into a disk cache when one is configured, so later reads never touch the
// server.
package fullcacheresource

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"sync/atomic"

	"git.ruekov.eu/ruakij/nzbStreamer/pkg/diskcache"
	"git.ruekov.eu/ruakij/nzbStreamer/pkg/resource"
)

// maxPrealloc bounds what a size hint may reserve, since it comes from the nzb
// and a segment is a fraction of it. Anything larger grows as it is read.
const maxPrealloc = 4 << 20

var ErrFetchAbandoned = errors.New("the fetch of this segment did not finish")

// fetching is one download of a cache-key. Every reader that wants the key
// while it runs waits on it and is served the same bytes, so a segment two
// chunks of the readahead window straddle is fetched once and read back never.
type fetching struct {
	done chan struct{}
	data []byte
	err  error
	refs int
}

var (
	fetchingMutex sync.Mutex
	fetchingByKey = make(map[string]*fetching)
)

// fetchOnce runs fetch unless another caller is already running it for key, and
// returns what that call produced. The entry is dropped once the last waiter
// has it, so a later reader finds the item in the cache instead.
func fetchOnce(key string, fetch func() ([]byte, error)) ([]byte, error) {
	fetchingMutex.Lock()
	f, joined := fetchingByKey[key]
	if !joined {
		f = &fetching{done: make(chan struct{})}
		fetchingByKey[key] = f
	}
	f.refs++
	fetchingMutex.Unlock()

	if !joined {
		// A fetch that panics still releases the waiters, with an error rather
		// than the empty content they would take a nil for
		f.err = ErrFetchAbandoned
		func() {
			defer close(f.done)
			data, err := fetch()
			f.data, f.err = data, err
		}()
	}
	<-f.done

	fetchingMutex.Lock()
	f.refs--
	if f.refs == 0 {
		delete(fetchingByKey, key)
	}
	fetchingMutex.Unlock()

	return f.data, f.err
}

// FullCacheResource caches underlying Record by fully reading its content into cache
type FullCacheResource struct {
	UnderlyingResource resource.ReadCloseableResource
	CacheKey           diskcache.Key
	Cache              *diskcache.Cache
	// Size is settled by whichever reader gets there first, and read by all of them
	cachedSize      atomic.Int64
	cachedSizeExact atomic.Bool
	options         *FullCacheResourceOptions
}

type FullCacheResourceOptions struct {
	// Force lookup Size() from underlying resource, ignoring any Caches
	SizeAlwaysFromResource bool
}

func NewFullCacheResource(underlyingResource resource.ReadCloseableResource, cacheKey diskcache.Key, cache *diskcache.Cache, options *FullCacheResourceOptions) *FullCacheResource {
	r := &FullCacheResource{
		UnderlyingResource: underlyingResource,
		options:            options,
		CacheKey:           cacheKey,
		Cache:              cache,
	}
	r.cachedSize.Store(-1)

	return r
}

type FullCacheResourceReader struct {
	resource         *FullCacheResource
	underlyingReader io.ReadCloser
	// Cache-file kept open for the readers lifetime; reads are positional, so no seeking
	fileMutex sync.Mutex
	cacheFile *os.File
	// data is what this reader fetched, where it did; a loaded reader has one of
	// the two and an empty segment neither
	data   []byte
	loaded bool
	index  int64
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
	if r.cachedSizeExact.Load() {
		return r.cachedSize.Load(), true, nil
	}

	if !r.options.SizeAlwaysFromResource {
		if exists, header := r.Cache.Exists(r.CacheKey); exists {
			r.setExactSize(header.Size)
			return header.Size, true, nil
		}
	}

	// Not cached yet, so only the underlying resource can answer
	if sized, ok := r.UnderlyingResource.(resource.Sized); ok {
		if size, err := sized.Size(); err == nil {
			r.setExactSize(size)
			return size, true, nil
		} else if !errors.Is(err, resource.ErrSizeNotExact) {
			return 0, false, fmt.Errorf("failed getting size from underlying resource: %w", err)
		}
	}

	if size := r.cachedSize.Load(); size >= 0 {
		return size, false, nil
	}

	size, err = r.UnderlyingResource.SizeHint()
	if err != nil {
		return 0, false, fmt.Errorf("failed getting size-hint from underlying resource: %w", err)
	}

	r.cachedSize.Store(size)
	return size, false, nil
}

// setExactSize records a size known to be exact, which nothing revises later.
func (r *FullCacheResource) setExactSize(size int64) {
	r.cachedSize.Store(size)
	r.cachedSizeExact.Store(true)
}

func (r *FullCacheResourceReader) Close() error {
	r.fileMutex.Lock()
	cacheFile := r.cacheFile
	r.cacheFile, r.data, r.loaded = nil, nil, false
	r.fileMutex.Unlock()

	if cacheFile != nil {
		if err := cacheFile.Close(); err != nil {
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
	if size := r.resource.cachedSize.Load(); size > 0 && r.index >= size {
		return 0, io.EOF
	}

	n, err := r.ReadAt(p, r.index)
	r.index += int64(n)

	return n, err
}

// ReadAt reads at an absolute offset in the segment, answered by the fetched
// content or by the cache-file. It carries no position, so concurrent calls run
// in parallel.
func (r *FullCacheResourceReader) ReadAt(p []byte, off int64) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}

	r.fileMutex.Lock()
	if !r.loaded {
		if err := r.load(); err != nil {
			r.fileMutex.Unlock()
			return 0, err
		}
		r.loaded = true
	}
	data, cacheFile := r.data, r.cacheFile
	r.fileMutex.Unlock()

	if cacheFile != nil {
		//nolint:wrapcheck // io.EOF has to reach the caller unwrapped
		return cacheFile.ReadAt(p, off)
	}

	if off >= int64(len(data)) {
		return 0, io.EOF
	}
	n := copy(p, data[off:])
	if n < len(p) {
		return n, io.EOF
	}

	return n, nil
}

// load makes the segment readable, from the cache-file where it is cached and
// from the content of a fetch where it is not. Requires fileMutex.
func (r *FullCacheResourceReader) load() error {
	file, size, err := r.resource.Cache.Open(r.resource.CacheKey)
	if err == nil {
		r.cacheFile = file
		r.resource.setExactSize(size)

		return nil
	}
	if !errors.Is(err, diskcache.ErrItemNotFound) {
		return fmt.Errorf("failed getting item from cache: %w", err)
	}

	// Item was evicted between reads, or was never fetched
	data, err := fetchOnce(r.resource.CacheKey.String(), r.fetch)
	if err != nil {
		return err
	}

	r.data = data
	r.resource.setExactSize(int64(len(data)))

	return nil
}

// fetch reads the underlying resource whole and stores it. Its content serves
// the readers waiting on it, so a miss does not write the segment and read it
// straight back.
func (r *FullCacheResourceReader) fetch() ([]byte, error) {
	if r.underlyingReader == nil {
		reader, err := r.resource.UnderlyingResource.Open()
		if err != nil {
			return nil, fmt.Errorf("failed reopening underlying resource: %w", err)
		}
		r.underlyingReader = reader
	}

	// Sized from the hint, so the content is not copied through a growing buffer
	hint, err := r.resource.UnderlyingResource.SizeHint()
	if err != nil {
		return nil, fmt.Errorf("failed getting size-hint from underlying resource: %w", err)
	}
	buffer := bytes.NewBuffer(make([]byte, 0, min(max(hint, 0), maxPrealloc)))
	if _, err := buffer.ReadFrom(r.underlyingReader); err != nil {
		return nil, fmt.Errorf("failed reading underlying resource: %w", err)
	}

	if err := r.underlyingReader.Close(); err != nil {
		return nil, fmt.Errorf("failed closing underlying reader: %w", err)
	}
	r.underlyingReader = nil

	data := buffer.Bytes()
	if _, err := r.resource.Cache.Set(r.resource.CacheKey, data); err != nil {
		return nil, err
	}

	return data, nil
}
