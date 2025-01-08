package diskCache

import "time"

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

func EvictFIFO(entries map[string]CacheItemHeader) string {
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
