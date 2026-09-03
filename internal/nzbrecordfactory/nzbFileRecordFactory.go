package nzbrecordfactory

import (
	"errors"
	"fmt"
	"log/slog"
	"path"
	"slices"

	"git.ruekov.eu/ruakij/nzbStreamer/internal/nzbfileanalyzer"
	"git.ruekov.eu/ruakij/nzbStreamer/internal/presentation"
	"git.ruekov.eu/ruakij/nzbStreamer/pkg/diskcache"
	"git.ruekov.eu/ruakij/nzbStreamer/pkg/filenameops"
	"git.ruekov.eu/ruakij/nzbStreamer/pkg/nzbparser"
	"git.ruekov.eu/ruakij/nzbStreamer/pkg/resource"
	"git.ruekov.eu/ruakij/nzbStreamer/pkg/resource/adaptiveparallelmergerresource"
	"git.ruekov.eu/ruakij/nzbStreamer/pkg/resource/fullcacheresource"
	"git.ruekov.eu/ruakij/nzbStreamer/pkg/resource/nzbpostresource"
	"git.ruekov.eu/ruakij/nzbStreamer/pkg/resource/rarfileresource"
	"git.ruekov.eu/ruakij/nzbStreamer/pkg/resource/sevenzipfileresource"
)

var logger = slog.With("Module", "NzbRecordFactory")

// SegmentSizeStore is what the factory needs of the metadata store: the decoded
// lengths it already knows, and somewhere to report the ones it learns. A
// decoded length is a fact about an immutable post, so it is keyed by message-id
// and shared by every nzb naming that post. May be nil.
type SegmentSizeStore interface {
	SegmentSizes(ids []string) (map[string]int64, error)
	RecordSegmentSize(messageID string, size int64)
	ForgetSegments(ids []string) error
}

type NzbFileFactory struct {
	cache      *diskcache.Cache
	getSegment nzbpostresource.GetSegmentFunc
	sizeStore  SegmentSizeStore
	// How many segments an nzb whose hints do not identify their convention may
	// have decoded to find out; 0 or less leaves it on estimates
	probeAttempts int
}

// getSegment is the whole of what the factory needs from a news server.
func NewNzbFileFactory(cache *diskcache.Cache, getSegment nzbpostresource.GetSegmentFunc, sizeStore SegmentSizeStore, probeAttempts int) *NzbFileFactory {
	f := &NzbFileFactory{
		cache:         cache,
		sizeStore:     sizeStore,
		probeAttempts: probeAttempts,
	}

	// Decoding is what turns a segments size hint into a fact, and this is where
	// decoding ends, so the observation is taken here rather than reported back
	// up through the resource layers
	f.getSegment = func(group, id string) ([]byte, error) {
		body, err := getSegment(group, id)
		if err == nil && sizeStore != nil {
			sizeStore.RecordSegmentSize(id, int64(len(body)))
		}
		return body, err
	}

	return f
}

func (f *NzbFileFactory) BuildSegmentStackFromNzbData(nzbData *nzbparser.NzbData) (map[string]presentation.Openable, error) {
	known := f.knownSizes(nzbData)
	sizer := f.sizer(nzbData, known)

	rawFiles := f.buildRawFiles(nzbData, sizer, known)
	groupedFilenames := f.groupFiles(rawFiles)

	files := make(map[string]presentation.Openable, len(rawFiles))
	err := f.processFileGroups(groupedFilenames, rawFiles, nzbData.Meta["Password"], files)
	if err != nil {
		return files, err
	}

	return files, nil
}

// DiscardSegmentStackFromNzbData throws away everything the stack accumulated
// for an nzb: the cached segment bytes, which is the bulk of it, and the sizes
// learned from decoding them.
//
// A post another nzb also names goes with it, costing that nzb a refetch. Both
// are caches, so this is never wrong, only slow; the alternative is an
// nzb-to-post table that nothing else would use.
func (f *NzbFileFactory) DiscardSegmentStackFromNzbData(nzbData *nzbparser.NzbData) {
	ids := segmentIDs(nzbData)

	if f.cache != nil {
		for _, id := range ids {
			if err := f.cache.Remove(id); err != nil && !errors.Is(err, diskcache.ErrItemNotFound) {
				logger.Warn("Failed removing cached segment", "segment", id, "error", err)
			}
		}
	}

	if f.sizeStore != nil {
		if err := f.sizeStore.ForgetSegments(ids); err != nil {
			logger.Warn("Failed forgetting segment sizes", "nzb", nzbData.MetaName, "error", err)
		}
	}
}

func segmentIDs(nzbData *nzbparser.NzbData) []string {
	var ids []string
	for i := range nzbData.Files {
		for _, segment := range nzbData.Files[i].Segments {
			ids = append(ids, segment.ID)
		}
	}
	return ids
}

// knownSizes asks the store for every decoded length it already holds for this
// nzb, in one round-trip rather than one per segment.
func (f *NzbFileFactory) knownSizes(nzbData *nzbparser.NzbData) map[string]int64 {
	if f.sizeStore == nil {
		return nil
	}

	sizes, err := f.sizeStore.SegmentSizes(segmentIDs(nzbData))
	if err != nil {
		// Not knowing a size is the normal state, so a failed lookup costs
		// measurement later and never correctness
		logger.Warn("Failed reading known segment sizes", "nzb", nzbData.MetaName, "error", err)
		return nil
	}

	return sizes
}

// sizer decides what this nzbs bytes-hints count. Most nzbs answer that from
// their hints alone; one that does not is settled from a segment that has been
// downloaded: one the store already has a length for, or one downloaded to find out.
//
// The store answers on every build after the first read of the nzb, so probing
// costs one segment once rather than one per start. With probing off the nzb
// serves estimates until a read has measured a full segment, and settles on the
// build after that - the resources of a built stack keep the sizes they were
// made with.
func (f *NzbFileFactory) sizer(nzbData *nzbparser.NzbData, known map[string]int64) nzbfileanalyzer.SegmentSizer {
	sizer := settleConvention(nzbData, nzbfileanalyzer.NewSegmentSizer(nzbData), known)
	if sizer.Convention() != nzbfileanalyzer.ConventionUnknown || f.probeAttempts <= 0 {
		return sizer
	}

	fetchSize := func(group, id string) (int, error) {
		body, err := f.getSegment(group, id)
		return len(body), err
	}

	probed, err := sizer.SettleByProbing(nzbData, fetchSize, f.probeAttempts)
	if err != nil {
		// Estimated sizes are the state this nzb was already in, so a failed
		// probe costs measurement on a later seek and never correctness
		logger.Warn("Failed probing size convention", "nzb", nzbData.MetaName, "error", err)
		return sizer
	}

	logger.Debug("Probed size convention", "nzb", nzbData.MetaName, "convention", probed.Convention())
	return probed
}

// settleConvention identifies what an nzbs bytes-attribute counts, for one whose
// hints alone could not say, from a segment the stack has already downloaded. One
// such segment makes every full segment in the nzb exact.
func settleConvention(nzbData *nzbparser.NzbData, sizer nzbfileanalyzer.SegmentSizer, known map[string]int64) nzbfileanalyzer.SegmentSizer {
	if sizer.Convention() != nzbfileanalyzer.ConventionUnknown {
		return sizer
	}

	for i := range nzbData.Files {
		for _, segment := range nzbData.Files[i].Segments {
			size, ok := known[segment.ID]
			if !ok {
				continue
			}

			sizer = sizer.SettleWith(segment.BytesHint, int(size))
			if sizer.Convention() != nzbfileanalyzer.ConventionUnknown {
				logger.Debug("Settled size convention from a downloaded segment", "nzb", nzbData.MetaName, "convention", sizer.Convention())
				return sizer
			}
		}
	}

	return sizer
}

// buildRawFiles creates the initial map of raw file resources
func (f *NzbFileFactory) buildRawFiles(nzbData *nzbparser.NzbData, sizer nzbfileanalyzer.SegmentSizer, known map[string]int64) map[string]resource.ReadSeekCloseableResource {
	rawFiles := make(map[string]resource.ReadSeekCloseableResource, len(nzbData.Files))
	for i := range nzbData.Files {
		file := &nzbData.Files[i]
		rawFiles[file.Filename] = f.BuildFileResourceFromNzbFile(file, sizer, known)
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
	case ".7z", ".z":
		specialFiles, err = f.Build7zFileFromFileResource(groupedFiles, password)
	}

	if len(specialFiles) > 0 {
		for filepath, resource := range specialFiles {
			files[path.Join(groupFilename, filepath)] = resource
		}
	}
	return err
}

func (f *NzbFileFactory) BuildFileResourceFromNzbFile(nzbFiles *nzbparser.File, sizer nzbfileanalyzer.SegmentSizer, known map[string]int64) *adaptiveparallelmergerresource.AdaptiveParallelMergerResource {
	totalSegments := len(nzbFiles.Segments)
	cachedSegmentResources := make([]resource.ReadSeekCloseableResource, 0, totalSegments)

	// Sort so append-order is correct
	slices.SortFunc(nzbFiles.Segments, func(a, b nzbparser.Segment) int {
		return a.Index - b.Index
	})

	for i := range nzbFiles.Segments {
		nzbSegment := &nzbFiles.Segments[i]
		segmentResource := f.BuildResourceFromNzbSegment(nzbSegment, nzbFiles.Groups[0], sizer, known)
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

func (f *NzbFileFactory) BuildResourceFromNzbSegment(nzbSegment *nzbparser.Segment, groups string, sizer nzbfileanalyzer.SegmentSizer, known map[string]int64) *nzbpostresource.NzbPostResource {
	if size, ok := known[nzbSegment.ID]; ok {
		// A measured length beats anything derived from the hint
		return nzbpostresource.New(nzbSegment.ID, groups, size, true, f.getSegment)
	}

	size, sizeExact := sizer.Size(nzbSegment.BytesHint)
	return nzbpostresource.New(nzbSegment.ID, groups, int64(size), sizeExact, f.getSegment)
}

// -- Special files --

func (f *NzbFileFactory) BuildRarFileFromFileResource(underlyingResources []resource.ReadSeekCloseableResource, password string) (map[string]presentation.Openable, error) {
	resources := make(map[string]presentation.Openable, 1)

	fileheaders, err := rarfileresource.NewRarFileResource(underlyingResources, password, "", -1).GetRarFiles(1)
	if err != nil {
		return nil, fmt.Errorf("failed creating Rar resource: %w", err)
	}

	for _, fileheader := range fileheaders {
		resources[fileheader.Name] = rarfileresource.NewRarFileResource(underlyingResources, password, fileheader.Name, fileheader.UnPackedSize)
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

	for member := range files {
		resources[member] = sevenzipfileresource.NewSevenzipFileResource(mergedResource, password, member)
	}

	return resources, nil
}
