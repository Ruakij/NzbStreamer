package adaptivereadaheadcache

import (
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"time"

	"git.ruekov.eu/ruakij/nzbStreamer/pkg/circularbuffer"
	"git.ruekov.eu/ruakij/nzbStreamer/pkg/resource"
)

// AdaptiveReadaheadCache reads sequentially ahead based on current read speed
type AdaptiveReadaheadCache struct {
	underlyingResource resource.ReadSeekCloseableResource
	// Over how much time average speed is calculated
	cacheAvgSpeedTime time.Duration
	// How far ahead to read in time
	cacheTime time.Duration
	// Minimum amount of readahead bytes
	cacheMinSize int
	// Maximum amount of readahead bytes
	cacheMaxSize int
	// Only readahead, when cache is below this size
	cacheLowWater int
}

func NewAdaptiveReadaheadCache(underlyingResource resource.ReadSeekCloseableResource, cacheAvgSpeedTime, cacheTime time.Duration, cacheMinSize, cacheMaxSize, cacheLowWater int) *AdaptiveReadaheadCache {
	return &AdaptiveReadaheadCache{
		underlyingResource: underlyingResource,
		cacheAvgSpeedTime:  cacheAvgSpeedTime,
		cacheTime:          cacheTime,
		cacheMinSize:       cacheMinSize,
		cacheMaxSize:       cacheMaxSize,
		cacheLowWater:      cacheLowWater,
	}
}

type AdaptiveReadaheadCacheReader struct {
	resource            *AdaptiveReadaheadCache
	underlyingReader    io.ReadSeekCloser
	underlyingReaderEOF bool
	index               int64
	readHistory         []readHistoryEntry
	cache               *circularbuffer.CircularBuffer[byte]
	mutex               sync.RWMutex
	readaheadRunning    atomic.Bool
}

type readHistoryEntry struct {
	bytesRead int64
	timestamp time.Time
}

func (r *AdaptiveReadaheadCache) Open() (io.ReadSeekCloser, error) {
	underlyingReader, err := r.underlyingResource.Open()
	if err != nil {
		return nil, fmt.Errorf("failed opening underlying resource: %w", err)
	}

	cache := circularbuffer.NewCircularBuffer[byte](r.cacheMinSize, r.cacheMaxSize)
	cache.SetReadBlocking(true)

	return &AdaptiveReadaheadCacheReader{
		resource:         r,
		underlyingReader: underlyingReader,
		readHistory:      make([]readHistoryEntry, 0, int(r.cacheAvgSpeedTime.Seconds())),
		cache:            cache,
	}, nil
}

func (r *AdaptiveReadaheadCache) Size() (int64, error) {
	size, err := r.underlyingResource.Size()
	if err != nil {
		return 0, fmt.Errorf("failed getting size from underlying resource: %w", err)
	}
	return size, nil
}

func (r *AdaptiveReadaheadCacheReader) Close() error {
	r.mutex.Lock()
	defer r.mutex.Unlock()

	if err := r.underlyingReader.Close(); err != nil {
		return fmt.Errorf("failed closing underlying resource: %w", err)
	}
	if err := r.cache.Flush(); err != nil {
		return fmt.Errorf("failed flushing cache: %w", err)
	}
	r.readHistory = []readHistoryEntry{}
	return nil
}

func (r *AdaptiveReadaheadCacheReader) trySeekCache(indexDelta int64) bool {
	if indexDelta <= 0 {
		return false
	}

	if int64(r.cache.GetSize()) > indexDelta {
		_, err := r.cache.Seek(indexDelta, io.SeekCurrent)
		if err == nil {
			return true
		}
		if !errors.Is(err, circularbuffer.ErrSeekOutOfBounds) {
			// Log error if needed, but continue with cache clear
			_ = err
		}
	}
	return false
}

func (r *AdaptiveReadaheadCacheReader) Seek(offset int64, whence int) (int64, error) {
	r.mutex.Lock()
	defer r.mutex.Unlock()

	var newIndex int64
	switch whence {
	case io.SeekStart:
		newIndex = offset
	case io.SeekCurrent:
		newIndex = r.index + offset
	case io.SeekEnd:
		newIndex = -1
	}

	// Seek to same pos we are at
	if newIndex == r.index {
		return r.index, nil
	}

	newIndex, err := r.underlyingReader.Seek(offset, whence)
	if err != nil {
		return -1, fmt.Errorf("failed seeking underlying reader: %w", err)
	}

	indexDelta := newIndex - r.index
	cacheValid := r.trySeekCache(indexDelta)

	if !cacheValid {
		if err := r.clearCache(); err != nil {
			return 0, fmt.Errorf("failed to clear cache during seek: %w", err)
		}
		r.underlyingReaderEOF = false
	}

	if !r.underlyingReaderEOF && r.readaheadRunning.CompareAndSwap(false, true) {
		go r.readahead()
	}

	r.index = newIndex
	return r.index, nil
}

func (r *AdaptiveReadaheadCacheReader) clearCache() error {
	r.readHistory = r.readHistory[:0]
	if err := r.cache.Clear(); err != nil {
		return fmt.Errorf("failed to clear cache: %w", err)
	}
	return nil
}

func (r *AdaptiveReadaheadCacheReader) Read(p []byte) (int, error) {
	r.recordRead(int64(len(p)))
	if !r.underlyingReaderEOF && r.readaheadRunning.CompareAndSwap(false, true) {
		go r.readahead()
	}

	// Try to fulfill the read request from the cache
	n, err := r.cache.Read(p)
	r.index += int64(n)
	if err != nil && !errors.Is(err, io.EOF) {
		if errors.Is(err, io.EOF) {
			err = nil
		} else {
			return n, fmt.Errorf("failed reading from cache: %w", err)
		}
	}

	if r.underlyingReaderEOF && n == 0 {
		// Underlying reader is closed and we didnt read anything from cache, means we hit the end
		err = io.EOF
	}

	return n, err
}

func (r *AdaptiveReadaheadCacheReader) recordRead(bytesRead int64) {
	r.mutex.Lock()
	defer r.mutex.Unlock()

	now := time.Now()
	r.readHistory = append(r.readHistory, readHistoryEntry{bytesRead, now})

	// Remove old entries from history
	cutoff := now.Add(-r.resource.cacheAvgSpeedTime)

	var firstValidIndex int
	for i, entry := range r.readHistory {
		if entry.timestamp.After(cutoff) {
			firstValidIndex = i
			break
		}
	}

	// Reslice to remove all items before firstValidIndex
	r.readHistory = r.readHistory[firstValidIndex:]
}

func (r *AdaptiveReadaheadCacheReader) avgSpeed() float64 {
	var totalBytes int64
	var earliest, latest time.Time

	if len(r.readHistory) > 0 {
		earliest = r.readHistory[0].timestamp
		latest = r.readHistory[len(r.readHistory)-1].timestamp

		for _, entry := range r.readHistory {
			totalBytes += entry.bytesRead
		}

		duration := latest.Sub(earliest)
		durationSecs := duration.Seconds()
		if duration > 0 {
			speed := float64(totalBytes) / durationSecs
			return speed
		}
	}

	return 0
}

func (r *AdaptiveReadaheadCacheReader) ensureBufferSize(readaheadAmount int) error {
	if r.cache.GetCurrFree() < readaheadAmount && r.cache.GetCurrCapacity() < r.cache.GetMaxCapacity() {
		resizeTarget := r.cache.GetSize() + readaheadAmount
		if resizeTarget > r.cache.GetMaxCapacity() {
			resizeTarget = r.cache.GetMaxCapacity()
		}
		if err := r.cache.Resize(resizeTarget); err != nil {
			return fmt.Errorf("failed resizing cache: %w", err)
		}
	}
	return nil
}

func (r *AdaptiveReadaheadCacheReader) readahead() error {
	defer r.readaheadRunning.Store(false)

	r.mutex.Lock()
	defer r.mutex.Unlock()

	avgSpeed := r.avgSpeed()
	readaheadAmount := int(avgSpeed * r.resource.cacheTime.Seconds())

	if readaheadAmount > r.resource.cacheMaxSize {
		readaheadAmount = r.resource.cacheMaxSize
	} else if readaheadAmount < r.resource.cacheMinSize {
		readaheadAmount = r.resource.cacheMinSize
	}

	if readaheadAmount == 0 {
		return nil
	}

	// Check if cache is above LowWater
	if r.resource.cacheLowWater < r.cache.GetSize() {
		return nil
	}

	if err := r.ensureBufferSize(readaheadAmount); err != nil {
		return err
	}

	// Check if buffer has any space
	if r.cache.GetCurrFree() == 0 {
		return nil
	}

	// Directly read into buffer
	exposedBuffer := r.cache.ExposeWriteSpace()
	n, err := r.underlyingReader.Read(exposedBuffer)

	// Immediately commit the result (even if n==0) so that the cache unblocks waiting reads:
	if commitErr := r.cache.CommitWrite(n); commitErr != nil {
		return fmt.Errorf("failed committing write to cache: %w", commitErr)
	}

	if err != nil {
		if errors.Is(err, io.EOF) {
			r.underlyingReaderEOF = true
			return io.EOF
		} else {
			return fmt.Errorf("failed reading from underlying reader: %w", err)
		}
	}

	return nil
}
