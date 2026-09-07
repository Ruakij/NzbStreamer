package diskcache

import "time"

// EvictLRU picks the item read longest ago. Open bumps an items ModTime, so
// that is what "least recently used" is recorded as.
func EvictLRU(entries map[string]CacheItemHeader) string {
	var oldestTime time.Time
	var oldestKey string
	for key, header := range entries {
		if oldestTime.IsZero() || header.ModTime.Before(oldestTime) {
			oldestTime = header.ModTime
			oldestKey = key
		}
	}

	return oldestKey
}
