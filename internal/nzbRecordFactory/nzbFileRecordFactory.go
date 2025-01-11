package nzbRecordFactory

import (
	"path"
	"slices"
	"time"

	"astuart.co/nntp"
	"git.ruekov.eu/ruakij/nzbStreamer/internal/nzbFileAnalyzer"
	"git.ruekov.eu/ruakij/nzbStreamer/pkg/SimpleWebdavFilesystem"
	"git.ruekov.eu/ruakij/nzbStreamer/pkg/diskCache"
	"git.ruekov.eu/ruakij/nzbStreamer/pkg/filenameOps"
	"git.ruekov.eu/ruakij/nzbStreamer/pkg/nzbParser"
	"git.ruekov.eu/ruakij/nzbStreamer/pkg/resource"

	"git.ruekov.eu/ruakij/nzbStreamer/pkg/resource/AdaptiveParallelMergerResource"
	"git.ruekov.eu/ruakij/nzbStreamer/pkg/resource/AdaptiveReadaheadCache"
	"git.ruekov.eu/ruakij/nzbStreamer/pkg/resource/FullCacheResource"
	"git.ruekov.eu/ruakij/nzbStreamer/pkg/resource/NzbPostResource"
	"git.ruekov.eu/ruakij/nzbStreamer/pkg/resource/RarFileResource"
	"git.ruekov.eu/ruakij/nzbStreamer/pkg/resource/SevenzipFileResource"
)

type NzbFileFactory struct {
	cache      *diskCache.Cache
	nntpClient *nntp.Client

	// Over how much time average speed is calculated
	adaptiveReadaheadCacheAvgSpeedTime time.Duration
	// How far ahead to read in time
	adaptiveReadaheadCacheTime time.Duration
	// Lowest expected speed					Can help at beginning speeding up
	adaptiveReadaheadCacheMinSize int
	// Low-Buffer (When to load more data)		Helps with continous data-flow
	adaptiveReadaheadCacheLowBuffer int
	// Max expected speed + Low-Buffer			Max speed per iteration
	adaptiveReadaheadCacheMaxSize int
}

func NewNzbFileFactory(cache *diskCache.Cache, nntpClient *nntp.Client) *NzbFileFactory {
	return &NzbFileFactory{
		cache:      cache,
		nntpClient: nntpClient,
	}
}

func (f *NzbFileFactory) SetAdaptiveReadaheadCacheSettings(adaptiveReadaheadCacheAvgSpeedTime time.Duration, adaptiveReadaheadCacheTime time.Duration, adaptiveReadaheadCacheMinSize int, adaptiveReadaheadCacheLowBuffer int, adaptiveReadaheadCacheMaxSize int) {
	f.adaptiveReadaheadCacheAvgSpeedTime = adaptiveReadaheadCacheAvgSpeedTime
	f.adaptiveReadaheadCacheTime = adaptiveReadaheadCacheTime
	f.adaptiveReadaheadCacheMinSize = adaptiveReadaheadCacheMinSize
	f.adaptiveReadaheadCacheLowBuffer = adaptiveReadaheadCacheLowBuffer
	f.adaptiveReadaheadCacheMaxSize = adaptiveReadaheadCacheMaxSize
}

func (f *NzbFileFactory) BuildSegmentStackFromNzbData(nzbData *nzbParser.NzbData) map[string]SimpleWebdavFilesystem.Openable {
	// Build nzbFiles -> rawFiles
	rawFiles := make(map[string]resource.ReadSeekCloseableResource, len(nzbData.Files))
	for _, file := range nzbData.Files {
		rawFiles[file.Filename] = f.BuildFileResourceFromNzbFile(file)
	}

	// Extract filenames
	filenames := make([]string, 0, len(rawFiles))
	for filename := range rawFiles {
		filenames = append(filenames, filename)
	}

	// Group
	groupedFilenames := filenameOps.GroupPartFilenames(filenames)
	// Sort within Groups
	filenameOps.SortGroupedFilenames(groupedFilenames)

	// Assemble structure
	files := make(map[string]SimpleWebdavFilesystem.Openable, len(filenames))
	for groupFilename, filenames := range groupedFilenames {
		groupedFiles := make([]resource.ReadSeekCloseableResource, len(filenames))
		for i, filename := range filenames {
			resource := rawFiles[filename]

			files[filename] = resource
			groupedFiles[i] = resource
		}

		// Process special files
		var specialFiles map[string]SimpleWebdavFilesystem.Openable
		var err error
		extension := path.Ext(groupFilename)
		switch extension {
		case ".rar", ".r":
			specialFiles, err = f.BuildRarFileFromFileResource(groupedFiles, nzbData.Meta["Password"])
		case ".7z", ".z":
			specialFiles, err = f.Build7zFileFromFileResource(groupedFiles, nzbData.Meta["Password"])
			if err != nil && extension == ".z" { // Fallback in case its actually a zip
				//specialFiles, err = f.BuildZipFileFromFileResource(groupedFiles, nzbData.Meta["Password"])
			}
		case ".zip": // TODO: Implement zip
			//specialFiles, err = f.BuildZipFileFromFileResource(groupedFiles, nzbData.Meta["Password"])
		}
		if len(specialFiles) > 0 {
			for filepath, resource := range specialFiles {
				files[path.Join(groupFilename, filepath)] = resource
			}
		}
		if err != nil {
			// TODO: How to handle? Return or skip?
		}
	}

	// Wrap every exposed file in AdaptiveReadaheadCache
	if f.adaptiveReadaheadCacheMaxSize > 1 {
		for path, file := range files {
			files[path] = AdaptiveReadaheadCache.NewAdaptiveReadaheadCache(
				file,
				f.adaptiveReadaheadCacheAvgSpeedTime,
				f.adaptiveReadaheadCacheTime,
				f.adaptiveReadaheadCacheMinSize,
				f.adaptiveReadaheadCacheMaxSize,
				f.adaptiveReadaheadCacheLowBuffer,
			)
		}
	}

	return files
}

func (f *NzbFileFactory) BuildNamedFileResourcesFromNzb(nzbData *nzbParser.NzbData) map[string]resource.ReadSeekCloseableResource {
	fileResources := make(map[string]resource.ReadSeekCloseableResource, len(nzbData.Files))

	for _, file := range nzbData.Files {
		fileResources[file.Filename] = f.BuildFileResourceFromNzbFile(file)
	}

	return fileResources
}

func (f *NzbFileFactory) BuildFileResourceFromNzbFile(nzbFiles nzbParser.File) resource.ReadSeekCloseableResource {
	totalSegments := len(nzbFiles.Segments)
	cachedSegmentResources := make([]resource.ReadSeekCloseableResource, 0, totalSegments)

	// Sort so append-order is correct
	slices.SortFunc(nzbFiles.Segments, func(a, b nzbParser.Segment) int {
		return a.Index - b.Index
	})

	for _, nzbSegment := range nzbFiles.Segments {
		segmentResource := f.BuildResourceFromNzbSegment(&nzbSegment, nzbFiles.Groups[0])
		cachedSegmentResource := FullCacheResource.NewFullCacheResource(
			segmentResource,
			nzbSegment.Id,
			f.cache,
			&FullCacheResource.FullCacheResourceOptions{
				SizeAlwaysFromResource: false,
			},
		)
		cachedSegmentResources = append(cachedSegmentResources, cachedSegmentResource)
	}

	return AdaptiveParallelMergerResource.NewAdaptiveParallelMergerResource(cachedSegmentResources)
}

func (f *NzbFileFactory) BuildResourceFromNzbSegment(nzbSegment *nzbParser.Segment, groups string) resource.ReadCloseableResource {
	// Try to get probable size
	size := nzbFileAnalyzer.GetProbableKnownSegmentSize(nzbSegment.BytesHint)
	sizeExact := (size > 0)
	if !sizeExact {
		// Otherwise estimate segment-size (if its not a common size, its probably the yEnc-size, which is slightly bigger)
		size = nzbFileAnalyzer.GetEstimatedSegmentSize(nzbSegment.BytesHint)
	}
	return &NzbPostResource.NzbPostResource{
		Id:            nzbSegment.Id,
		Group:         groups,
		SizeHint:      int64(size),
		SizeHintExact: sizeExact,
		NntpClient:    f.nntpClient,
	}
}

// -- Special files --

func (f *NzbFileFactory) BuildRarFileFromFileResource(underlyingResources []resource.ReadSeekCloseableResource, password string) (map[string]SimpleWebdavFilesystem.Openable, error) {
	resources := make(map[string]SimpleWebdavFilesystem.Openable, 1)

	fileheaders, err := RarFileResource.NewRarFileResource(underlyingResources, password, "").GetRarFiles(1)
	if err != nil {
		return nil, err
	}

	for _, fileheader := range fileheaders {
		resources[fileheader.Name] = RarFileResource.NewRarFileResource(underlyingResources, password, fileheader.Name)
	}

	return resources, nil
}
func (f *NzbFileFactory) Build7zFileFromFileResource(underlyingResources []resource.ReadSeekCloseableResource, password string) (map[string]SimpleWebdavFilesystem.Openable, error) {
	resources := make(map[string]SimpleWebdavFilesystem.Openable, 1)

	mergedResource := AdaptiveParallelMergerResource.NewAdaptiveParallelMergerResource(underlyingResources)

	files, err := SevenzipFileResource.NewSevenzipFileResource(mergedResource, password, "").GetFiles()
	if err != nil {
		return nil, err
	}

	for filepath, fileinfo := range files {
		resources[path.Join(filepath, fileinfo.Name())] = SevenzipFileResource.NewSevenzipFileResource(mergedResource, password, "")
	}

	return resources, nil
}
