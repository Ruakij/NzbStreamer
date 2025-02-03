package adaptiveseekahead

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"git.ruekov.eu/ruakij/nzbStreamer/pkg/resource"
)

// AdaptiveSeekahead reads sequentially ahead based on current read speed
type AdaptiveSeekahead struct {
	underlyingResource resource.ReadSeekCloseableResource
	// Over how much time average speed is calculated
	cacheAvgSpeedTime time.Duration
	// How far ahead to read in time
	cacheTime time.Duration
	// Minimum amount of seekahead bytes
	cacheMinSize int
	// Maximum amount of seekahead bytes
	cacheMaxSize int
	// Only seekahead, when cache is below this size
	cacheLowWater int
}

func NewAdaptiveSeekahead(underlyingResource resource.ReadSeekCloseableResource, cacheAvgSpeedTime, cacheTime time.Duration, cacheMinSize, cacheMaxSize, cacheLowWater int) *AdaptiveSeekahead {
	return &AdaptiveSeekahead{
		underlyingResource: underlyingResource,
		cacheAvgSpeedTime:  cacheAvgSpeedTime,
		cacheTime:          cacheTime,
		cacheMinSize:       cacheMinSize,
		cacheMaxSize:       cacheMaxSize,
		cacheLowWater:      cacheLowWater,
	}
}

type AdaptiveSeekaheadReader struct {
	resource            *AdaptiveSeekahead
	underlyingReader    io.ReadSeekCloser
	underlyingReaderEOF bool
	index               int64
	readHistory         []readHistoryEntry
	seekaheadAmount     int64
	mutex               sync.RWMutex
	seekaheadRunning    atomic.Bool
}

type readHistoryEntry struct {
	bytesRead int64
	timestamp time.Time
}

func (r *AdaptiveSeekahead) Open() (io.ReadSeekCloser, error) {
	underlyingReader, err := r.underlyingResource.Open()
	if err != nil {
		return nil, fmt.Errorf("failed opening underlying resource: %w", err)
	}

	return &AdaptiveSeekaheadReader{
		resource:         r,
		underlyingReader: underlyingReader,
		readHistory:      make([]readHistoryEntry, 0, int(r.cacheAvgSpeedTime.Seconds())),
	}, nil
}

func (r *AdaptiveSeekahead) Size() (int64, error) {
	size, err := r.underlyingResource.Size()
	if err != nil {
		return 0, fmt.Errorf("failed getting size from underlying resource: %w", err)
	}
	return size, nil
}

func (r *AdaptiveSeekaheadReader) Close() error {
	r.mutex.Lock()
	defer r.mutex.Unlock()

	if err := r.underlyingReader.Close(); err != nil {
		return fmt.Errorf("failed closing underlying resource: %w", err)
	}
	r.readHistory = []readHistoryEntry{}
	return nil
}

func (r *AdaptiveSeekaheadReader) Seek(offset int64, whence int) (int64, error) {
	r.mutex.Lock()
	defer r.mutex.Unlock()
	slog.Debug("Seek\t", "AdaptiveSeekaheadReader", fmt.Sprintf("%p", r), "current index", r.index, "offset", offset, "whence", whence)

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

	// Seek cache if seeking is forwards and within bounds
	indexDelta := newIndex - r.index
	if indexDelta > 0 {
		if r.seekaheadAmount > indexDelta {
			r.seekaheadAmount -= indexDelta
		} else {
			if err := r.clearCache(); err != nil {
				return -1, fmt.Errorf("failed to clear cache: %w", err)
			}
		}
	} else {
		// Otherwise flush cache
		if err := r.clearCache(); err != nil {
			return -1, fmt.Errorf("failed to clear cache: %w", err)
		}
		r.underlyingReaderEOF = false
	}
	if !r.underlyingReaderEOF && r.seekaheadRunning.CompareAndSwap(false, true) {
		go func() {
			if err := r.seekahead(); err != nil {
				slog.Error("Seekahead error", "error", err)
			}
		}()
	}

	r.index = newIndex
	slog.Debug("Seek complete", "AdaptiveSeekaheadReader", fmt.Sprintf("%p", r), "current index", r.index)

	return r.index, nil
}

func (r *AdaptiveSeekaheadReader) clearCache() error {
	r.readHistory = r.readHistory[:0]
	_, err := r.underlyingReader.Seek(-r.seekaheadAmount, io.SeekCurrent)
	if err != nil {
		return fmt.Errorf("failed to clear cache: %w", err)
	}
	return nil
}

func (r *AdaptiveSeekaheadReader) Read(p []byte) (int, error) {
	r.mutex.Lock()
	defer r.mutex.Unlock()

	r.recordRead(int64(len(p)))

	n, err := r.underlyingReader.Read(p)
	r.index += int64(n)
	r.seekaheadAmount -= int64(n)

	if err != nil && errors.Is(err, io.EOF) {
		r.underlyingReaderEOF = true
	}

	if !r.underlyingReaderEOF && r.seekaheadRunning.CompareAndSwap(false, true) {
		go r.seekahead()
	}

	if err != nil {
		return n, fmt.Errorf("failed reading from underlying reader: %w", err)
	}
	return n, nil
}

func (r *AdaptiveSeekaheadReader) recordRead(bytesRead int64) {
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

func (r *AdaptiveSeekaheadReader) avgSpeed() float64 {
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

func (r *AdaptiveSeekaheadReader) seekahead() error {
	defer r.seekaheadRunning.Store(false)

	r.mutex.Lock()
	defer r.mutex.Unlock()

	avgSpeed := r.avgSpeed()
	seekaheadAmount := int(avgSpeed * r.resource.cacheTime.Seconds())

	if seekaheadAmount > r.resource.cacheMaxSize {
		seekaheadAmount = r.resource.cacheMaxSize
	}

	if seekaheadAmount < r.resource.cacheMinSize {
		seekaheadAmount = r.resource.cacheMinSize
	}

	if seekaheadAmount == 0 {
		return nil
	}

	// Check if cache is above LowWater
	if int64(r.resource.cacheLowWater) < r.seekaheadAmount {
		return nil
	}

	// Directly read into buffer
	_, err := r.underlyingReader.Seek(int64(seekaheadAmount), io.SeekCurrent)
	if err != nil {
		return fmt.Errorf("failed seekahead: %w", err)
	}
	_, err = r.underlyingReader.Seek(-int64(seekaheadAmount), io.SeekCurrent)
	if err != nil {
		return fmt.Errorf("failed seekahead back: %w", err)
	}

	r.seekaheadAmount = int64(seekaheadAmount)

	return nil
}
