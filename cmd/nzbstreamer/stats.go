package main

import (
	"time"

	"git.ruekov.eu/ruakij/nzbStreamer/internal/nntpclient"
	"git.ruekov.eu/ruakij/nzbStreamer/internal/nzbrecordfactory"
	"git.ruekov.eu/ruakij/nzbStreamer/internal/service/nzbservice"
	"git.ruekov.eu/ruakij/nzbStreamer/pkg/diskcache"
)

// pageStats is what the strip on the page reports, plus what each row holds in
// the cache. Per-nzb numbers are the reason this is not on /metrics: a label per
// nzb is cardinality that grows with the library.
func pageStats(cache *diskcache.Cache, pool *nntpclient.Pool, service *nzbservice.Service) func() any {
	return func() any {
		stats := cache.Stats()

		up := 0
		servers := pool.Health()
		for _, server := range servers {
			if server.Up {
				up++
			}
		}

		// One pass over the cache index answers every row, rather than a lookup
		// per segment of every nzb on every poll
		groups := cache.Groups()
		cached := make(map[string]any, len(groups))
		for _, id := range service.Names() {
			group, exists := groups[nzbrecordfactory.CachePrefix(id)]
			if !exists {
				continue
			}
			cached[id] = map[string]any{
				"segments":  group.Items,
				"bytes":     group.Bytes,
				"last_read": group.LastRead,
			}
		}

		return map[string]any{
			"cache": map[string]any{
				"items":     stats.Items,
				"bytes":     stats.Bytes,
				"max_bytes": stats.MaxBytes,
				"hits":      stats.Hits,
				"misses":    stats.Misses,
				"evictions": stats.Evictions,
			},
			"servers": map[string]any{"up": up, "total": len(servers)},
			"cached":  cached,
		}
	}
}

// fileStat is what the cache holds of one posted file.
type fileStat struct {
	Size, CachedBytes int64
	CachedSegments    int
	Exact             bool
	LastRead          time.Time
}

// statFile weighs a cached segment by what the cache stored and the rest by what
// the nzb lets them be estimated at, which is what keeps a fully cached file at
// 100% rather than at whatever the estimate was off by. Exact says the size owes
// nothing to an estimate.
func statFile(cache *diskcache.Cache, prefix string, file nzbservice.PostedFile) fileStat {
	stat := fileStat{Exact: true}
	for _, segment := range file.Segments {
		exists, header := cache.Exists(diskcache.Key{prefix, segment.ID})
		if !exists {
			stat.Size += segment.Bytes
			stat.Exact = stat.Exact && segment.Exact
			continue
		}
		stat.CachedSegments++
		stat.CachedBytes += header.Size
		stat.Size += header.Size
		if header.ModTime.After(stat.LastRead) {
			stat.LastRead = header.ModTime
		}
	}
	return stat
}

// nzbStats answers one row's info panel: what of each posted file the cache
// holds. The files are the ones the nzb posts, not the ones a presenter shows,
// since an extracted member does not map back to segments.
func nzbStats(cache *diskcache.Cache, service *nzbservice.Service) func(string) any {
	return func(id string) any {
		files := service.PostedFiles(id)
		if files == nil {
			return nil
		}
		prefix := nzbrecordfactory.CachePrefix(id)

		detail := make([]map[string]any, 0, len(files))
		var totalBytes, cachedBytes int64
		var totalSegments, cachedSegments int
		totalExact := true
		for _, file := range files {
			stat := statFile(cache, prefix, file)

			detail = append(detail, map[string]any{
				"name":            file.Name,
				"bytes":           stat.Size,
				"exact":           stat.Exact,
				"segments":        len(file.Segments),
				"cached_bytes":    stat.CachedBytes,
				"cached_segments": stat.CachedSegments,
				"last_read":       stat.LastRead,
			})

			totalBytes += stat.Size
			totalExact = totalExact && stat.Exact
			totalSegments += len(file.Segments)
			cachedBytes += stat.CachedBytes
			cachedSegments += stat.CachedSegments
		}

		return map[string]any{
			"id":              id,
			"bytes":           totalBytes,
			"exact":           totalExact,
			"segments":        totalSegments,
			"cached_bytes":    cachedBytes,
			"cached_segments": cachedSegments,
			"files":           detail,
		}
	}
}
