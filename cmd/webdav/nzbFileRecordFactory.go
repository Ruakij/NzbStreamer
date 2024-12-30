package main

import (
	"slices"

	"astuart.co/nntp"
	"git.ruekov.eu/ruakij/nzbStreamer/pkg/nzbParser"
	"git.ruekov.eu/ruakij/nzbStreamer/pkg/resource"

	"git.ruekov.eu/ruakij/nzbStreamer/pkg/resource/AdaptiveParallelMergerResource"
	"git.ruekov.eu/ruakij/nzbStreamer/pkg/resource/FullCacheResource"
	"git.ruekov.eu/ruakij/nzbStreamer/pkg/resource/NzbPostResource"
	"github.com/eko/gocache/lib/v4/cache"
)

func BuildNamedFileResourcesFromNzb(nzbData *nzbParser.NzbData, cache cache.CacheInterface[[]byte], nntpClient *nntp.Client) map[string]resource.ReadSeekCloseableResource {
	fileResources := make(map[string]resource.ReadSeekCloseableResource, len(nzbData.Files))

	for _, file := range nzbData.Files {
		fileResources[file.Filename] = BuildFileResourceFromNzbFile(file, cache, nntpClient)
	}

	return fileResources
}

func BuildFileResourceFromNzbFile(nzbFiles nzbParser.File, cache cache.CacheInterface[[]byte], nntpClient *nntp.Client) resource.ReadSeekCloseableResource {
	totalSegments := len(nzbFiles.Segments)
	cachedSegmentResources := make([]resource.ReadSeekCloseableResource, 0, totalSegments)

	// Sort so append-order is correct
	slices.SortFunc(nzbFiles.Segments, func(a, b nzbParser.Segment) int {
		return a.Index - b.Index
	})

	for _, nzbSegment := range nzbFiles.Segments {
		segmentResource := BuildResourceFromNzbSegment(&nzbSegment, nzbFiles.Groups[0], nntpClient)
		cachedSegmentResource := FullCacheResource.NewFullCacheResource(
			segmentResource,
			nzbSegment.Id,
			cache,
			&FullCacheResource.FullCacheResourceOptions{
				SizeAlwaysFromResource: true,
			},
		)
		cachedSegmentResources = append(cachedSegmentResources, cachedSegmentResource)
	}

	return AdaptiveParallelMergerResource.NewAdaptiveParallelMergerResource(cachedSegmentResources, 0)
}

func BuildResourceFromNzbSegment(nzbSegment *nzbParser.Segment, groups string, nntpClient *nntp.Client) resource.ReadableResource {
	return &NzbPostResource.NzbPostResource{
		Id:         nzbSegment.Id,
		Group:      groups,
		SizeHint:   int64(nzbSegment.BytesHint),
		NntpClient: nntpClient,
	}
}
