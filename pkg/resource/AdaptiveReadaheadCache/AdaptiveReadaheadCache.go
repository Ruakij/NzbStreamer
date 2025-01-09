package AdaptiveReadaheadCache

import (
	"io"
	"sync"
	"time"

	"git.ruekov.eu/ruakij/nzbStreamer/pkg/CircularBuffer"
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
	underlyingReaderEof bool
	index               int64
	readHistory         []readHistoryEntry
	cache               *CircularBuffer.CircularBuffer[byte]
	mutex               sync.RWMutex
}

type readHistoryEntry struct {
	bytesRead int64
	timestamp time.Time
}

func (r *AdaptiveReadaheadCache) Open() (io.ReadSeekCloser, error) {
	underlyingReader, err := r.underlyingResource.Open()
	if err != nil {
		return nil, err
	}

	cache := CircularBuffer.NewCircularBuffer[byte](int(r.cacheMinSize), int(r.cacheMaxSize))
	cache.SetReadBlocking(true)

	return &AdaptiveReadaheadCacheReader{
		resource:         r,
		underlyingReader: underlyingReader,
		readHistory:      make([]readHistoryEntry, 0, int(r.cacheAvgSpeedTime.Seconds())),
		cache:            cache,
	}, err
}

func (r *AdaptiveReadaheadCache) Size() (int64, error) {
	return r.underlyingResource.Size()
}

func (r *AdaptiveReadaheadCacheReader) Close() (err error) {
	if err = r.cache.Flush(); err != nil {
		return
	}
	return r.underlyingReader.Close()
}

func (r *AdaptiveReadaheadCacheReader) Seek(offset int64, whence int) (newIndex int64, err error) {
	r.mutex.Lock()
	defer r.mutex.Unlock()

	newIndex, err = r.underlyingReader.Seek(offset, whence)
	if err != nil {
		return -1, err
	}

	// Seek to same pos we are at
	if newIndex == r.index {
		return r.index, nil
	}

	// Seek cache if seeking is forwards and within bounds
	indexDelta := newIndex - r.index
	if indexDelta > 0 {
		if int64(r.cache.GetSize())-indexDelta > 0 {
			_, err = r.cache.Seek(offset, whence)
			if err != nil {
				return
			}
		}
	} else {
		// Otherwise flush cache
		r.flushCache()
		r.underlyingReaderEof = false
	}
	if !r.underlyingReaderEof {
		go r.readahead()
	}

	r.index = newIndex
	return
}

func (r *AdaptiveReadaheadCacheReader) flushCache() {
	r.readHistory = r.readHistory[:0]
	_ = r.cache.Flush()
}

func (r *AdaptiveReadaheadCacheReader) Read(p []byte) (n int, err error) {
	r.recordRead(int64(len(p)))
	if !r.underlyingReaderEof {
		go r.readahead()
	}

	// Try to fulfill the read request from the cache
	n, _ = r.cache.Read(p)
	r.index += int64(n)

	r.cache.RLock()
	if r.underlyingReaderEof && n == 0 {
		// Underlying reader is closed and we didnt read anything from cache, means we hit the end
		err = io.EOF
	}
	r.cache.RUnlock()

	return
}

func (r *AdaptiveReadaheadCacheReader) ReadOld(p []byte) (n int, err error) {
	r.mutex.Lock()
	defer r.mutex.Unlock()

	// Try to fulfill the read request from the cache
	n, _ = r.cache.Read(p)
	r.index += int64(n)

	// Read from underlying resource if there was a cache miss
	if n < len(p) {
		var m int
		m, err = r.underlyingReader.Read(p[n:])

		r.index += int64(m)
		n += m
	}

	r.recordRead(int64(n))

	// Trigger readahead asynchronously
	if !r.underlyingReaderEof {
		go r.readahead()
	}

	return
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

func (r *AdaptiveReadaheadCacheReader) readahead() error {
	r.mutex.Lock()
	defer r.mutex.Unlock()

	avgSpeed := r.avgSpeed()
	readaheadAmount := int(avgSpeed * r.resource.cacheTime.Seconds())

	if readaheadAmount > r.resource.cacheMaxSize {
		readaheadAmount = r.resource.cacheMaxSize
	}

	if readaheadAmount < r.resource.cacheMinSize {
		readaheadAmount = r.resource.cacheMinSize
	}

	if readaheadAmount == 0 {
		return nil
	}

	// Check if cache is above LowWater
	if r.resource.cacheLowWater < r.cache.GetSize() {
		return nil
	}

	/*
		// TODO: Why is this necessary in the first place? When copy couldnt write everything to cache, what happens with the leftover data?
		readaheadAmount -= int64(r.cache.GetSize())

		// Limit the reader to the readaheadAmount
		limitedReader := io.LimitReader(r.underlyingReader, readaheadAmount)
		n, err := io.Copy(r.cache, limitedReader)
	*/

	// If buffer is too small, but it can grow
	if r.cache.GetCurrFree() < int(readaheadAmount) && r.cache.GetCurrCapacity() < r.cache.GetMaxCapacity() {
		r.cache.Resize(int(readaheadAmount))
	}

	// Check if buffer has any space
	if r.cache.GetCurrFree() == 0 {
		return nil
	}

	// Directly read into buffer
	exposedBuffer := r.cache.ExposeWriteSpace()
	n, err := r.underlyingReader.Read(exposedBuffer)
	r.cache.CommitWrite(n)

	if err == io.EOF {
		r.underlyingReaderEof = true
	}

	return err
}
