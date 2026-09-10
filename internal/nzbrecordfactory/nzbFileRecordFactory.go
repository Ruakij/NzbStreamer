// Package nzbrecordfactory turns an nzb into the stack of resources that
// serves each of the files it contains.
package nzbrecordfactory

import (
	"errors"
	"fmt"
	"log/slog"
	"path"
	"slices"
	"strings"

	"git.ruekov.eu/ruakij/nzbStreamer/internal/nntpclient"
	"git.ruekov.eu/ruakij/nzbStreamer/internal/nzbfileanalyzer"
	"git.ruekov.eu/ruakij/nzbStreamer/internal/nzbstore"
	"git.ruekov.eu/ruakij/nzbStreamer/internal/presentation"
	"git.ruekov.eu/ruakij/nzbStreamer/pkg/diskcache"
	"git.ruekov.eu/ruakij/nzbStreamer/pkg/filenameops"
	"git.ruekov.eu/ruakij/nzbStreamer/pkg/nzbparser"
	"git.ruekov.eu/ruakij/nzbStreamer/pkg/resource"
	"git.ruekov.eu/ruakij/nzbStreamer/pkg/resource/adaptiveparallelmergerresource"
	"git.ruekov.eu/ruakij/nzbStreamer/pkg/resource/fullcacheresource"
	"git.ruekov.eu/ruakij/nzbStreamer/pkg/resource/nzbpostresource"
	"git.ruekov.eu/ruakij/nzbStreamer/pkg/resource/rarfileresource"
	"git.ruekov.eu/ruakij/nzbStreamer/pkg/resource/readaheadresource"
	"git.ruekov.eu/ruakij/nzbStreamer/pkg/resource/sevenzipfileresource"
)

// SegmentStore is what the factory needs of the metadata store: the decoded
// lengths it already knows, and somewhere to report what the read path learns -
// a fetched size, a read, and the outcome of a fetch, which is evidence about
// the nzb's health. Everything is keyed by the nzb's own file and the segment's
// position within it, the hierarchy the verdicts hang off. The position is the
// segment's 0-based index into the file's sorted Segments, the space the scan
// reports in; the nzb's number attribute is a different one, and a report in it
// lands beside the scan's rows instead of on them. May be nil.
type SegmentStore interface {
	SegmentSizes(nzbName string, ids []string) (map[string]int64, error)
	RecordSegmentSize(nzbName, filename, messageID string, index int, size int64)
	RecordSegmentRead(nzbName, filename string, index int)
	RecordProbes(nzbName string, probes []nzbstore.ProbeResult) error
	EnsureSourceFiles(nzbName string, files []nzbstore.SourceFile) error
}

// BuildResult is what a build presents and what each presented path was built
// from.
type BuildResult struct {
	Presented map[string]presentation.Openable
	// SourceOf maps a presented path to the nzb's own filename it was built
	// from, the volume-set member or raw content file. The service attributes
	// a presented file back to its source file through it, so a failed group
	// can be un-presented; a member of an unpacked archive attributes to the
	// first volume of its set, whose grouping is the whole archive again
	SourceOf map[string]string
}

type NzbFileFactory struct {
	cache      *diskcache.Cache
	getSegment nzbpostresource.GetSegmentFunc
	sizeStore  SegmentStore
	// How many segments an nzb whose hints do not identify their convention may
	// have decoded to find out; 0 or less leaves it on estimates
	probeAttempts int
	// How many archives deep unpacking goes. An upload of an archive of an
	// archive is a real thing, an unbounded chain of them is a way to spend the
	// whole add reading headers
	maxArchiveDepth int
	readaheadMin    int
	readaheadMax    int
	readaheadChunk  int
	rampSpeed       float64
}

func (f *NzbFileFactory) SetReadahead(minSize, maxSize, chunk int, rampSpeed float64) {
	f.readaheadMin = minSize
	f.readaheadMax = maxSize
	f.readaheadChunk = chunk
	f.rampSpeed = rampSpeed
}

func NewNzbFileFactory(cache *diskcache.Cache, getSegment nzbpostresource.GetSegmentFunc, sizeStore SegmentStore, probeAttempts, maxArchiveDepth int) *NzbFileFactory {
	return &NzbFileFactory{
		cache:           cache,
		getSegment:      getSegment,
		sizeStore:       sizeStore,
		probeAttempts:   probeAttempts,
		maxArchiveDepth: maxArchiveDepth,
	}
}

// Marks an archive that could not be opened
var ErrArchiveLeftPacked = errors.New("archive left packed")

// BuildSegmentStackFromNzbData returns the files an nzb presents and what each
// was built from. A returned ErrArchiveLeftPacked comes with a usable tree; any
// other error does not.
//
// progress reports the volumes the header walks have opened, which is what the
// build spends its time on; nil is a caller that does not want to know.
func (f *NzbFileFactory) BuildSegmentStackFromNzbData(nzbData *nzbparser.NzbData, progress ProgressFunc) (BuildResult, error) {
	f.ensureSourceFiles(nzbData)

	known := f.knownSizes(nzbData)
	sizer := f.sizer(nzbData, known)

	rawFiles := f.buildRawFiles(nzbData, sizer, known, CachePrefix(nzbData.MetaName))

	result := BuildResult{
		Presented: make(map[string]presentation.Openable, len(rawFiles)),
		SourceOf:  make(map[string]string, len(rawFiles)),
	}
	packed := f.expand(rawFiles, sourceOfEach(rawFiles), "", 0, nzbData.Meta["Password"], &result, &buildProgress{report: progress})
	for name, file := range result.Presented {
		if _, windowed := file.(*readaheadresource.Resource); windowed {
			continue // A raw file presented as it is carries its window already
		}
		if underlying, ok := file.(resource.ReadSeekCloseableResource); ok {
			result.Presented[name] = f.withReadahead(underlying)
		}
	}

	return result, packed
}

// ensureSourceFiles makes sure the store holds the rows a health verdict hangs
// off before anything the build does reports against them. The upsert is
// idempotent, so this costs one write per name on every build and only fills
// the gap where the caller has not recorded the files yet.
func (f *NzbFileFactory) ensureSourceFiles(nzbData *nzbparser.NzbData) {
	if f.sizeStore == nil {
		return
	}

	files := make([]nzbstore.SourceFile, len(nzbData.Files))
	for i := range nzbData.Files {
		files[i] = nzbstore.SourceFile{
			Filename: nzbData.Files[i].Filename,
			PostedAt: nzbData.Files[i].ParsedDate,
		}
	}
	if err := f.sizeStore.EnsureSourceFiles(nzbData.MetaName, files); err != nil {
		slog.Warn("Failed recording the nzb's own files", "nzb", nzbData.MetaName, "error", err)
	}
}

// sourceOfEach maps every key to itself, the source each nzb file starts as;
// unpacked members inherit the first volume of their set below.
func sourceOfEach[V any](entries map[string]V) map[string]string {
	sources := make(map[string]string, len(entries))
	for filename := range entries {
		sources[filename] = filename
	}
	return sources
}

// withReadahead puts a window in front of a resource, or hands it back where
// readahead is switched off.
func (f *NzbFileFactory) withReadahead(underlying resource.ReadSeekCloseableResource) resource.ReadSeekCloseableResource {
	if f.readaheadMax <= 0 || f.readaheadChunk <= 0 {
		return underlying
	}

	return readaheadresource.New(underlying, f.readaheadMin, f.readaheadMax, f.readaheadChunk, f.rampSpeed)
}

// DiscardSegmentStackFromNzbData throws away everything the stack accumulated
// for an nzb: the cached segment bytes, which is the bulk of it.
//
// What the store knows about the nzb's segments goes with the nzb record
// itself, by the cascade. Both are caches, so this is never wrong, only slow.
func (f *NzbFileFactory) DiscardSegmentStackFromNzbData(nzbData *nzbparser.NzbData) {
	if f.cache != nil {
		if err := f.cache.RemoveAll(diskcache.Key{CachePrefix(nzbData.MetaName)}); err != nil {
			slog.Warn("Failed removing cached segments", "nzb", nzbData.MetaName, "error", err)
		}
	}
}

// CachePrefix is the first key part every segment of an nzb is cached under.
func CachePrefix(metaName string) string {
	return strings.ReplaceAll(metaName, "/", "_")
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

	sizes, err := f.sizeStore.SegmentSizes(nzbData.MetaName, segmentIDs(nzbData))
	if err != nil {
		// Not knowing a size is the normal state, so a failed lookup costs
		// measurement later and never correctness
		slog.Warn("Failed reading known segment sizes", "nzb", nzbData.MetaName, "error", err)
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

	// The probe fetches one segment whole to learn what the hints count, which
	// is a read of that segment as much as any other, so its outcome reports
	// the same way a read of it would
	locations := locateSegments(nzbData)
	fetchSize := func(group, id string) (int, error) {
		body, err := f.observedFor(nzbData.MetaName, locations, group, id)
		return len(body), err
	}

	probed, err := sizer.SettleByProbing(nzbData, fetchSize, f.probeAttempts)
	if err != nil {
		// Estimated sizes are the state this nzb was already in, so a failed
		// probe costs measurement on a later seek and never correctness
		slog.Warn("Failed probing size convention", "nzb", nzbData.MetaName, "error", err)
		return sizer
	}

	slog.Debug("Probed size convention", "nzb", nzbData.MetaName, "convention", probed.Convention())
	return probed
}

// segmentLocation is where a message-id sits in the nzb, for the reports that
// only carry the id. index is the segment's 0-based position in its file's
// sorted Segments, the space every report to the store is keyed in.
type segmentLocation struct {
	filename string
	index    int
}

// locateSegments maps every message-id of the nzb to the file and position it
// holds there.
func locateSegments(nzbData *nzbparser.NzbData) map[string]segmentLocation {
	locations := make(map[string]segmentLocation)
	for i := range nzbData.Files {
		file := &nzbData.Files[i]
		// The build sorts the same slice by the same key, so the position
		// taken here is the one the resources are built with
		slices.SortFunc(file.Segments, func(a, b nzbparser.Segment) int {
			return a.Index - b.Index
		})
		for j, segment := range file.Segments {
			locations[segment.ID] = segmentLocation{filename: file.Filename, index: j}
		}
	}
	return locations
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
				slog.Debug("Settled size convention from a known segment size", "nzb", nzbData.MetaName, "convention", sizer.Convention())
				return sizer
			}
		}
	}

	return sizer
}

// buildRawFiles creates the initial map of raw file resources
func (f *NzbFileFactory) buildRawFiles(nzbData *nzbparser.NzbData, sizer nzbfileanalyzer.SegmentSizer, known map[string]int64, cachePrefix string) map[string]resource.ReadSeekCloseableResource {
	rawFiles := make(map[string]resource.ReadSeekCloseableResource, len(nzbData.Files))
	for i := range nzbData.Files {
		file := &nzbData.Files[i]
		// The window sits above the merger, so an archive decodes out of one too
		// rather than a segment at a time: the volumes are addressable whatever
		// the member inside them turns out to be
		rawFiles[file.Filename] = f.withReadahead(f.BuildFileResourceFromNzbFile(nzbData.MetaName, file, sizer, known, cachePrefix))
	}
	return rawFiles
}

// expand presents every entry under prefix and unpacks the archives among them,
// running itself over what each archive contained. An archive becomes the folder
// its volumes group into and its members live below it, so the volumes
// themselves are presented only where they were not unpacked - a set of one is
// named after the group holding it, and a file cannot also be a folder.
//
// sources says, per entry, which nzb file it was built from; what is presented
// lands in result with that attribution. A member of an unpacked set may be
// built from every volume in it, and is attributed to the first one, whose
// grouping is the whole set again - a verdict on the archive reaches the
// member through it.
//
// depth counts the archives already opened on the way here. One nested deeper
// than the limit is left presented as the volumes it is: a client sees an
// archive it has to unpack itself, which is less than it wanted and more than
// failing the add would have given it.
func (f *NzbFileFactory) expand(entries map[string]resource.ReadSeekCloseableResource, sources map[string]string, prefix string, depth int, password string, result *BuildResult, report *buildProgress) error {
	filenames := make([]string, 0, len(entries))
	for filename := range entries {
		filenames = append(filenames, filename)
	}
	grouped := filenameops.GroupPartFilenames(filenames)
	filenameops.SortGroupedFilenames(grouped)

	var packed []error
	for groupFilename, groupFilenames := range grouped {
		volumes := make([]resource.ReadSeekCloseableResource, len(groupFilenames))
		for i, filename := range groupFilenames {
			volumes[i] = entries[filename]
		}

		archivePath := path.Join(prefix, groupFilename)
		members, err := f.unpack(groupFilename, archivePath, volumes, depth, password, report)
		switch {
		case err != nil:
			slog.Warn("Archive could not be opened, leaving it packed",
				"archive", archivePath, "error", err)
			packed = append(packed, err)
		case len(members) > 0:
			memberSource := sources[groupFilenames[0]]
			memberSources := make(map[string]string, len(members))
			for member := range members {
				memberSources[member] = memberSource
			}
			if err := f.expand(members, memberSources, archivePath, depth+1, password, result, report); err != nil {
				packed = append(packed, err)
			}
			continue
		}

		for i, filename := range groupFilenames {
			presented := path.Join(prefix, filename)
			result.Presented[presented] = volumes[i]
			result.SourceOf[presented] = sources[filename]
		}
	}
	return errors.Join(packed...)
}

// unpack lists what an archive holds, or nothing where the group is not an
// archive, is nested deeper than the limit, or turned out to be empty.
func (f *NzbFileFactory) unpack(groupFilename, archivePath string, volumes []resource.ReadSeekCloseableResource, depth int, password string, report *buildProgress) (map[string]resource.ReadSeekCloseableResource, error) {
	open := f.archiveOpener(path.Ext(groupFilename))
	if open == nil {
		return nil, nil
	}

	if depth >= f.maxArchiveDepth {
		slog.Warn("Archive nested deeper than the limit, leaving it packed",
			"archive", archivePath, "limit", f.maxArchiveDepth)
		return nil, nil
	}

	members, err := open(volumes, password, report)
	if err != nil {
		return nil, fmt.Errorf("%w: %s: %w", ErrArchiveLeftPacked, archivePath, err)
	}
	return members, nil
}

// archiveOpener is what unpacks a group of volumes, or nil where the group is
// not an archive.
func (f *NzbFileFactory) archiveOpener(extension string) func([]resource.ReadSeekCloseableResource, string, *buildProgress) (map[string]resource.ReadSeekCloseableResource, error) {
	switch {
	case extension == ".rar":
		return f.BuildRarFileFromFileResource
	case isArchive(extension):
		return f.Build7zFileFromFileResource
	}
	return nil
}

// isArchive says whether a group of files under this extension is unpacked
// rather than presented as it is.
func isArchive(extension string) bool {
	switch extension {
	case ".rar", ".7z", ".z":
		return true
	}
	return false
}

func (f *NzbFileFactory) BuildFileResourceFromNzbFile(nzbName string, nzbFile *nzbparser.File, sizer nzbfileanalyzer.SegmentSizer, known map[string]int64, cachePrefix string) *adaptiveparallelmergerresource.AdaptiveParallelMergerResource {
	totalSegments := len(nzbFile.Segments)
	cachedSegmentResources := make([]resource.ReadSeekCloseableResource, 0, totalSegments)

	// Sort so append-order is correct, and so the loop position below is the
	// segment's place in the file, which is what the reports are keyed by
	slices.SortFunc(nzbFile.Segments, func(a, b nzbparser.Segment) int {
		return a.Index - b.Index
	})

	sizes := sizer.FileSizes(nzbFile)
	for i := range nzbFile.Segments {
		nzbSegment := &nzbFile.Segments[i]
		segmentResource := f.BuildResourceFromNzbSegment(nzbName, nzbFile, nzbSegment, i, sizes[i], known)
		cachedSegmentResource := fullcacheresource.NewFullCacheResource(
			segmentResource,
			diskcache.Key{cachePrefix, nzbSegment.ID},
			f.cache,
			&fullcacheresource.FullCacheResourceOptions{
				SizeAlwaysFromResource: false,
				OnRead:                 f.readRecorder(nzbName, nzbFile.Filename, i),
			},
		)
		cachedSegmentResources = append(cachedSegmentResources, cachedSegmentResource)
	}

	return adaptiveparallelmergerresource.NewAdaptiveParallelMergerResource(cachedSegmentResources)
}

// readRecorder reports a read of a segment, from the cache or from the server.
// index is the segment's position in its file, the key the scan writes the same
// facts under. A cold read is reported after the fetch that learned the segments
// size, so the store has the segment by the time the read arrives.
// Which segments were read within a timespan is the working set the cache has to
// hold, which the fetches alone cannot say: a cache large enough to serve every
// read reports no fetches at all.
func (f *NzbFileFactory) readRecorder(nzbName, filename string, index int) func() {
	if f.sizeStore == nil {
		return nil
	}

	return func() { f.sizeStore.RecordSegmentRead(nzbName, filename, index) }
}

// observed wraps a segment fetch so its outcome reaches the store. index is
// the segment's position in its file, the key the scan writes the same facts
// under. A fetch that comes back not-found is a verdict, not a log line: it is
// a GET against the exact segment, which is stronger evidence than the STAT a
// scan would have spent. A fetch that delivered bytes is the same evidence the
// other way, and both are reported off the read path. Any other error says
// nothing about the segment - a transport failure has not answered the question.
func (f *NzbFileFactory) observed(nzbName, filename string, index int, get nzbpostresource.GetSegmentFunc) nzbpostresource.GetSegmentFunc {
	if f.sizeStore == nil {
		return get
	}

	return func(group, id string) ([]byte, error) {
		body, err := get(group, id)
		switch {
		case err == nil:
			// Decoding is what turns a segments size hint into a fact, and
			// this is where it ends, so the length is taken here rather than
			// reported back up through the resource layers
			f.sizeStore.RecordSegmentSize(nzbName, filename, id, index, int64(len(body)))
			f.reportProbe(nzbName, nzbstore.ProbeResult{
				Filename: filename, Index: index, MessageID: id, Present: true,
			})
		case errors.Is(err, nntpclient.ErrArticleNotFound):
			// The pool has tried every server it has by the time this error
			// surfaces, and it names none of them, so the miss is stored
			// without a server. Per-server rows are left to a caller that sees
			// each 430 one server at a time; attributing it here would guess.
			f.reportProbe(nzbName, nzbstore.ProbeResult{
				Filename: filename, Index: index, MessageID: id, Present: false,
			})
		}
		return body, err
	}
}

// observedFor is the observed fetch of a segment picked by message-id, which is
// how the convention probe asks for the segment it settles on.
func (f *NzbFileFactory) observedFor(nzbName string, locations map[string]segmentLocation, group, id string) ([]byte, error) {
	if location, ok := locations[id]; ok {
		return f.observed(nzbName, location.filename, location.index, f.getSegment)(group, id)
	}
	return f.getSegment(group, id)
}

// reportProbe persists one fetch outcome off the read path: the reader has its
// answer already, and a write that waits on the database must not hold it.
func (f *NzbFileFactory) reportProbe(nzbName string, probe nzbstore.ProbeResult) {
	go func() {
		if err := f.sizeStore.RecordProbes(nzbName, []nzbstore.ProbeResult{probe}); err != nil {
			slog.Warn("Failed recording probe outcome", "nzb", nzbName, "segment", probe.MessageID, "error", err)
		}
	}()
}

// BuildResourceFromNzbSegment builds the resource that fetches one segment.
// index is the segment's 0-based position in nzbFile's sorted Segments, the
// space every report the resource fires is keyed in.
func (f *NzbFileFactory) BuildResourceFromNzbSegment(nzbName string, nzbFile *nzbparser.File, nzbSegment *nzbparser.Segment, index int, sized nzbfileanalyzer.SegmentSize, known map[string]int64) *nzbpostresource.NzbPostResource {
	get := f.observed(nzbName, nzbFile.Filename, index, f.getSegment)

	if size, ok := known[nzbSegment.ID]; ok {
		// A measured length beats anything derived from the hint
		return nzbpostresource.New(nzbSegment.ID, nzbFile.Groups[0], size, true, get)
	}

	return nzbpostresource.New(nzbSegment.ID, nzbFile.Groups[0], int64(sized.Size), sized.Exact, get)
}

// -- Special files --

// allMembers lists an archive whole. An archive holding a set of its own is why
// the first member is not enough: what it holds decides whether anything below
// it gets unpacked.
const allMembers = -1

func (f *NzbFileFactory) BuildRarFileFromFileResource(underlyingResources []resource.ReadSeekCloseableResource, password string, report *buildProgress) (map[string]resource.ReadSeekCloseableResource, error) {
	resources := make(map[string]resource.ReadSeekCloseableResource, 1)

	fileheaders, err := rarfileresource.NewRarFileResource(headerVolumes(underlyingResources, report), password, "", -1).GetRarFiles(allMembers)
	if err != nil {
		return nil, fmt.Errorf("failed creating Rar resource: %w", err)
	}

	for _, fileheader := range fileheaders {
		resources[fileheader.Name] = rarfileresource.NewRarFileResource(underlyingResources, password, fileheader.Name, fileheader.UnPackedSize)
	}

	return resources, nil
}

func (f *NzbFileFactory) Build7zFileFromFileResource(underlyingResources []resource.ReadSeekCloseableResource, password string, report *buildProgress) (map[string]resource.ReadSeekCloseableResource, error) {
	resources := make(map[string]resource.ReadSeekCloseableResource, 1)

	mergedResource := adaptiveparallelmergerresource.NewAdaptiveParallelMergerResource(underlyingResources)
	headerResource := adaptiveparallelmergerresource.NewAdaptiveParallelMergerResource(headerVolumes(underlyingResources, report))

	files, err := sevenzipfileresource.NewSevenzipFileResource(headerResource, password, "").GetFiles()
	if err != nil {
		return nil, fmt.Errorf("failed creating 7z resource: %w", err)
	}

	for member := range files {
		resources[member] = sevenzipfileresource.NewSevenzipFileResource(mergedResource, password, member)
	}

	return resources, nil
}
