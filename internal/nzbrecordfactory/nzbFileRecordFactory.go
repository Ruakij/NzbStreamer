package nzbrecordfactory

import (
	"fmt"
	"path"
	"slices"
	"time"

	"astuart.co/nntp"
	"git.ruekov.eu/ruakij/nzbStreamer/internal/nzbfileanalyzer"
	"git.ruekov.eu/ruakij/nzbStreamer/internal/presentation"
	"git.ruekov.eu/ruakij/nzbStreamer/pkg/diskcache"
	"git.ruekov.eu/ruakij/nzbStreamer/pkg/filenameops"
	"git.ruekov.eu/ruakij/nzbStreamer/pkg/nzbparser"
	"git.ruekov.eu/ruakij/nzbStreamer/pkg/resource"
	"git.ruekov.eu/ruakij/nzbStreamer/pkg/resource/adaptiveparallelmergerresource"
	"git.ruekov.eu/ruakij/nzbStreamer/pkg/resource/adaptivereadaheadcache"
	"git.ruekov.eu/ruakij/nzbStreamer/pkg/resource/fullcacheresource"
	"git.ruekov.eu/ruakij/nzbStreamer/pkg/resource/nzbpostresource"
	"git.ruekov.eu/ruakij/nzbStreamer/pkg/resource/rarfileresource"
	"git.ruekov.eu/ruakij/nzbStreamer/pkg/resource/sevenzipfileresource"
)

type NzbFileFactory struct {
	cache      *diskcache.Cache
	nntpClient *nntp.Client

	// Over how much time average speed is calculated
	adaptiveReadaheadCacheAvgSpeedTime time.Duration
	// How far ahead to read in time
	adaptiveReadaheadCacheTime time.Duration
	// Lowest expected speed					Can help at beginning speeding up
	adaptiveReadaheadCacheMinSize int
	// Low-Buffer (When to load more data)		Helps with continuous data-flow
	adaptiveReadaheadCacheLowBuffer int
	// Max expected speed + Low-Buffer			Max speed per iteration
	adaptiveReadaheadCacheMaxSize int
}

func NewNzbFileFactory(cache *diskcache.Cache, nntpClient *nntp.Client) *NzbFileFactory {
	return &NzbFileFactory{
		cache:      cache,
		nntpClient: nntpClient,
	}
}

func (f *NzbFileFactory) SetAdaptiveReadaheadCacheSettings(adaptiveReadaheadCacheAvgSpeedTime, adaptiveReadaheadCacheTime time.Duration, adaptiveReadaheadCacheMinSize, adaptiveReadaheadCacheLowBuffer, adaptiveReadaheadCacheMaxSize int) {
	f.adaptiveReadaheadCacheAvgSpeedTime = adaptiveReadaheadCacheAvgSpeedTime
	f.adaptiveReadaheadCacheTime = adaptiveReadaheadCacheTime
	f.adaptiveReadaheadCacheMinSize = adaptiveReadaheadCacheMinSize
	f.adaptiveReadaheadCacheLowBuffer = adaptiveReadaheadCacheLowBuffer
	f.adaptiveReadaheadCacheMaxSize = adaptiveReadaheadCacheMaxSize
}

func (f *NzbFileFactory) BuildSegmentStackFromNzbData(nzbData *nzbparser.NzbData) (map[string]presentation.Openable, error) {
	rawFiles := f.buildRawFiles(nzbData)
	groupedFilenames := f.groupFiles(rawFiles)

	files := make(map[string]presentation.Openable, len(rawFiles))
	err := f.processFileGroups(groupedFilenames, rawFiles, nzbData.Meta["Password"], files)
	if err != nil {
		return files, err
	}

	return f.wrapWithCache(files), nil
}

// buildRawFiles creates the initial map of raw file resources
func (f *NzbFileFactory) buildRawFiles(nzbData *nzbparser.NzbData) map[string]resource.ReadSeekCloseableResource {
	rawFiles := make(map[string]resource.ReadSeekCloseableResource, len(nzbData.Files))
	for i := range nzbData.Files {
		file := &nzbData.Files[i]
		rawFiles[file.Filename] = f.BuildFileResourceFromNzbFile(file)
	}
	return rawFiles
}

// groupFiles extracts and groups filenames from raw files
func (f *NzbFileFactory) groupFiles(rawFiles map[string]resource.ReadSeekCloseableResource) map[string][]string {
	filenames := make([]string, 0, len(rawFiles))
	for filename := range rawFiles {
		filenames = append(filenames, filename)
	}

	groupedFilenames := filenameops.GroupPartFilenames(filenames)
	filenameops.SortGroupedFilenames(groupedFilenames)
	return groupedFilenames
}

// processFileGroups handles processing of file groups and their special cases
func (f *NzbFileFactory) processFileGroups(groupedFilenames map[string][]string, rawFiles map[string]resource.ReadSeekCloseableResource, password string, files map[string]presentation.Openable) error {
	for groupFilename, filenames := range groupedFilenames {
		groupedFiles := f.prepareGroupedFiles(filenames, rawFiles, files)
		if err := f.processSpecialFiles(groupFilename, groupedFiles, password, files); err != nil {
			return fmt.Errorf("build special-file %s failed: %w", groupFilename, err)
		}
	}
	return nil
}

// prepareGroupedFiles prepares grouped files from filenames
func (f *NzbFileFactory) prepareGroupedFiles(filenames []string, rawFiles map[string]resource.ReadSeekCloseableResource, files map[string]presentation.Openable) []resource.ReadSeekCloseableResource {
	groupedFiles := make([]resource.ReadSeekCloseableResource, len(filenames))
	for i, filename := range filenames {
		resource := rawFiles[filename]
		files[filename] = resource
		groupedFiles[i] = resource
	}
	return groupedFiles
}

// processSpecialFiles handles special file types like RAR and 7z
func (f *NzbFileFactory) processSpecialFiles(groupFilename string, groupedFiles []resource.ReadSeekCloseableResource, password string, files map[string]presentation.Openable) error {
	extension := path.Ext(groupFilename)
	var specialFiles map[string]presentation.Openable
	var err error

	switch extension {
	case ".rar", ".r":
		specialFiles, err = f.BuildRarFileFromFileResource(groupedFiles, password)
	case ".7z", ".z", ".zip":
		specialFiles, err = f.Build7zFileFromFileResource(groupedFiles, password)
		if err != nil && extension == ".z" {
			// Handle potential zip fallback
		}
	}

	if len(specialFiles) > 0 {
		for filepath, resource := range specialFiles {
			files[path.Join(groupFilename, filepath)] = resource
		}
	}
	return err
}

// wrapWithCache wraps files with adaptive readahead cache if enabled
func (f *NzbFileFactory) wrapWithCache(files map[string]presentation.Openable) map[string]presentation.Openable {
	if f.adaptiveReadaheadCacheMaxSize <= 1 {
		return files
	}

	wrappedFiles := make(map[string]presentation.Openable, len(files))
	for path, file := range files {
		wrappedFiles[path] = adaptivereadaheadcache.NewAdaptiveReadaheadCache(
			file,
			f.adaptiveReadaheadCacheAvgSpeedTime,
			f.adaptiveReadaheadCacheTime,
			f.adaptiveReadaheadCacheMinSize,
			f.adaptiveReadaheadCacheMaxSize,
			f.adaptiveReadaheadCacheLowBuffer,
		)
	}
	return wrappedFiles
}

func (f *NzbFileFactory) BuildNamedFileResourcesFromNzb(nzbData *nzbparser.NzbData) map[string]resource.ReadSeekCloseableResource {
	fileResources := make(map[string]resource.ReadSeekCloseableResource, len(nzbData.Files))

	for i := range nzbData.Files {
		file := &nzbData.Files[i]
		fileResources[file.Filename] = f.BuildFileResourceFromNzbFile(file)
	}

	return fileResources
}

func (f *NzbFileFactory) BuildFileResourceFromNzbFile(nzbFiles *nzbparser.File) *adaptiveparallelmergerresource.AdaptiveParallelMergerResource {
	totalSegments := len(nzbFiles.Segments)
	cachedSegmentResources := make([]resource.ReadSeekCloseableResource, 0, totalSegments)

	// Sort so append-order is correct
	slices.SortFunc(nzbFiles.Segments, func(a, b nzbparser.Segment) int {
		return a.Index - b.Index
	})

	for i := range nzbFiles.Segments {
		nzbSegment := &nzbFiles.Segments[i]
		segmentResource := f.BuildResourceFromNzbSegment(nzbSegment, nzbFiles.Groups[0])
		cachedSegmentResource := fullcacheresource.NewFullCacheResource(
			segmentResource,
			nzbSegment.ID,
			f.cache,
			&fullcacheresource.FullCacheResourceOptions{
				SizeAlwaysFromResource: false,
			},
		)
		cachedSegmentResources = append(cachedSegmentResources, cachedSegmentResource)
	}

	return adaptiveparallelmergerresource.NewAdaptiveParallelMergerResource(cachedSegmentResources)
}

func (f *NzbFileFactory) BuildResourceFromNzbSegment(nzbSegment *nzbparser.Segment, groups string) *nzbpostresource.NzbPostResource {
	// Try to get probable size
	size := nzbfileanalyzer.GetProbableKnownSegmentSize(nzbSegment.BytesHint)
	sizeExact := (size > 0)
	if !sizeExact {
		// Otherwise estimate segment-size (if its not a common size, its probably the yEnc-size, which is slightly bigger)
		size = nzbfileanalyzer.GetEstimatedSegmentSize(nzbSegment.BytesHint)
	}
	return &nzbpostresource.NzbPostResource{
		ID:            nzbSegment.ID,
		Group:         groups,
		SizeHint:      int64(size),
		SizeHintExact: sizeExact,
		NntpClient:    f.nntpClient,
	}
}

// -- Special files --

func (f *NzbFileFactory) BuildRarFileFromFileResource(underlyingResources []resource.ReadSeekCloseableResource, password string) (map[string]presentation.Openable, error) {
	resources := make(map[string]presentation.Openable, 1)

	fileheaders, err := rarfileresource.NewRarFileResource(underlyingResources, password, "").GetRarFiles(1)
	if err != nil {
		return nil, fmt.Errorf("failed creating Rar resource: %w", err)
	}

	for _, fileheader := range fileheaders {
		resources[fileheader.Name] = rarfileresource.NewRarFileResource(underlyingResources, password, fileheader.Name)
	}

	return resources, nil
}

func (f *NzbFileFactory) Build7zFileFromFileResource(underlyingResources []resource.ReadSeekCloseableResource, password string) (map[string]presentation.Openable, error) {
	resources := make(map[string]presentation.Openable, 1)

	mergedResource := adaptiveparallelmergerresource.NewAdaptiveParallelMergerResource(underlyingResources)

	files, err := sevenzipfileresource.NewSevenzipFileResource(mergedResource, password, "").GetFiles()
	if err != nil {
		return nil, fmt.Errorf("failed creating 7z resource: %w", err)
	}

	for filepath, fileinfo := range files {
		resources[path.Join(filepath, fileinfo.Name())] = sevenzipfileresource.NewSevenzipFileResource(mergedResource, password, "")
	}

	return resources, nil
}
