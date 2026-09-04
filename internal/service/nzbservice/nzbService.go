package nzbservice

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path"
	"regexp"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"git.ruekov.eu/ruakij/nzbStreamer/internal/filehealth"
	"git.ruekov.eu/ruakij/nzbStreamer/internal/nzbrecordfactory"
	"git.ruekov.eu/ruakij/nzbStreamer/internal/nzbstore"
	"git.ruekov.eu/ruakij/nzbStreamer/internal/presentation"
	"git.ruekov.eu/ruakij/nzbStreamer/internal/trigger"
	"git.ruekov.eu/ruakij/nzbStreamer/pkg/filenameops"
	"git.ruekov.eu/ruakij/nzbStreamer/pkg/nzbparser"
	"git.ruekov.eu/ruakij/nzbStreamer/pkg/resource"
	"github.com/agnivade/levenshtein"
	"golang.org/x/sync/errgroup"
)

var logger = slog.With("Module", "NzbService")

type TriggerListener struct {
	trigger.Trigger
	listenerID int
}

type Service struct {
	mutex       sync.RWMutex
	store       nzbstore.NzbStore
	factory     nzbrecordfactory.Factory
	presenters  []presentation.Presenter
	triggers    []TriggerListener
	nzbFiledata map[string]*nzbparser.NzbData
	nzbFiles    map[string][]string // Maps NZB MetaName to its file paths

	// What every client api reports on, kept apart from the tree because an add
	// is observable long before it has one. Scanned linearly and never trimmed:
	// one entry per nzb the process knows of.
	queueMutex sync.Mutex
	queue      []*QueueItem

	// Options
	fileBlacklist                           []regexp.Regexp
	nzbFileBlacklist                        []regexp.Regexp
	pathFlatteningDepth                     int
	filenameReplacementBelowLevensteinRatio float32
	healthChecker                           filehealth.Checker
	exactSizeClasses                        []filenameops.FileClass
	// treeKey identifies the settings above that decide what a tree looks like,
	// so a stored one is only restored while they are unchanged. Empty stores
	// and restores nothing.
	treeKey string
	// addLimit bounds the trees being built at once, whether by an add or by a
	// restore; the ones it holds back sit in the queue as what they are. nil is
	// no limit.
	addLimit chan struct{}

	// What the startup restore is still doing. Until it is done the library is
	// incomplete, which is what a client acting on what it reads has to wait for
	restoring atomic.Int64
	ready     atomic.Bool
}

// Ready reports whether the startup restore has finished.
func (s *Service) Ready() bool {
	return s.ready.Load()
}

// Restoring reports how many nzbs are still having their tree rebuilt.
func (s *Service) Restoring() int {
	return int(s.restoring.Load())
}

func NewService(store nzbstore.NzbStore, factory nzbrecordfactory.Factory, presenters []presentation.Presenter, triggers []trigger.Trigger, healthChecker filehealth.Checker) *Service {
	triggerListeners := make([]TriggerListener, len(triggers))
	for i, trigger := range triggers {
		triggerListeners[i] = TriggerListener{
			Trigger:    trigger,
			listenerID: -1,
		}
	}

	return &Service{
		store:            store,
		factory:          factory,
		presenters:       presenters,
		triggers:         triggerListeners,
		fileBlacklist:    []regexp.Regexp{},
		nzbFileBlacklist: []regexp.Regexp{},
		nzbFiledata:      make(map[string]*nzbparser.NzbData),
		nzbFiles:         make(map[string][]string),
		healthChecker:    healthChecker,
	}
}

func (s *Service) SetBlacklist(blacklist []regexp.Regexp) {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	s.fileBlacklist = blacklist
}

func (s *Service) SetNzbFileBlacklist(blacklist []regexp.Regexp) {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	s.nzbFileBlacklist = blacklist
}

func (s *Service) SetPathFlatteningDepth(depth int) {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	s.pathFlatteningDepth = depth
}

func (s *Service) SetFilenameReplacementBelowLevensteinRatio(ratio float32) {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	s.filenameReplacementBelowLevensteinRatio = ratio
}

// SetConcurrency bounds how many trees are built at once. Building one is
// mostly waiting on the news server, over connections every read shares, so
// more is not faster past a point. 0 or less is no limit.
func (s *Service) SetConcurrency(builds int) {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	s.addLimit = nil
	if builds > 0 {
		s.addLimit = make(chan struct{}, builds)
	}
}

// acquireAdd waits for a free slot and returns what gives it back.
func (s *Service) acquireAdd() func() {
	s.mutex.RLock()
	limit := s.addLimit
	s.mutex.RUnlock()

	if limit == nil {
		return func() {} // unbounded, nothing to give back
	}

	limit <- struct{}{}
	return func() { <-limit }
}

// SetTreeKey names the settings a stored tree was built under. A restored tree
// is only presented while the key still matches; one built under anything else
// is rebuilt and rewrites its rows. Empty disables the stored trees entirely.
func (s *Service) SetTreeKey(key string) {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	s.treeKey = key
}

// SetExactSizeClasses picks which files are measured as part of an add rather
// than on their first read.
func (s *Service) SetExactSizeClasses(classes []filenameops.FileClass) {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	s.exactSizeClasses = classes
}

// Initialize the service; Load NzbData from store; build filedata and add to filesystem; Register to triggers
//
// It runs in the background of a start, so the presenters are up while the trees
// are rebuilt; Ready reports when it is done.
func (s *Service) Init() error {
	defer s.ready.Store(true)

	logger.Debug("Getting nzbData from store")
	records, err := s.store.List()
	if err != nil {
		return fmt.Errorf("failed listing nzbs in store: %w", err)
	}
	logger.Info("Loaded Nzb store", "items", len(records))
	started := time.Now()
	restored, rebuilt := 0, 0

	// Rebuilding an nzb walks its archive headers, which is seconds of waiting on
	// the news server, and the trees are independent of one another. How many run
	// at once is addNzb's own limit
	group := errgroup.Group{}

	for _, record := range records {
		switch Stage(record.Stage) {
		// Rebuilding is not adding: the posts were checked when they were added,
		// so the health check does not happen again. How it ends is recorded, in
		// the queue and in the store, since one that cannot be rebuilt is not a
		// completed download any more
		case StageCompleted:
			s.restore(record)

			// What it presents is a pure function of the nzb and the settings,
			// so it is read back rather than walked out of the archives again
			if s.restoreTree(record) {
				restored++
				continue
			}

			rebuilt++
			s.restoring.Add(1)
			s.rebuilding(record.Data.MetaName, true)
			group.Go(func() error {
				// The stage is cleared before the count drops, so nothing sees a
				// restore that is over with an item still in it
				defer func() {
					s.rebuilding(record.Data.MetaName, false)
					logger.Info("Rebuilt nzb", "MetaName", record.Data.MetaName,
						"remaining", s.restoring.Add(-1))
				}()

				if err := s.addNzb(record.Data, false); err != nil {
					logger.Error("Couldnt rebuild nzb", "MetaName", record.Data.MetaName, "error", err)
					s.failedRebuild(record.Data.MetaName, err)
				}
				return nil
			})

		// It ended without a tree, and a client that has not read the answer yet
		// still gets it
		case StageFailed, StageCancelled:
			s.restore(record)

		// The process died mid-add. Nothing of it was kept beyond the nzb, so
		// the add starts again rather than picking up
		default:
			logger.Info("Resuming interrupted add", "MetaName", record.Data.MetaName, "stage", record.Stage)
			if _, err := s.Add(record.Data, record.Category); err != nil {
				logger.Error("Couldnt resume add", "MetaName", record.Data.MetaName, "error", err)
			}
		}
	}

	// A rebuild is archive walks over the network, seconds each, and the files
	// of those nzbs are not presented until it is done. A start that says nothing
	// between listing the store and finishing looks finished while they are
	// missing
	if rebuilding := s.restoring.Load(); rebuilding > 0 {
		logger.Info("Rebuilding nzb trees, their files appear as each one finishes", "nzbs", rebuilding)
	}

	_ = group.Wait()

	logger.Info("Restored nzbs", "restored", restored, "rebuilt", rebuilt,
		"took", time.Since(started).Truncate(time.Millisecond))

	logger.Debug("Registering at triggers")
	for _, trigger := range s.triggers {
		trigger.listenerID, err = trigger.AddListener(s.AddNzb, s.RemoveNzb)
		if err != nil {
			return fmt.Errorf("failed registering at trigger %v: %w", trigger, err)
		}
	}

	logger.Debug("Init complete")

	return nil
}

var (
	ErrNzbAlreadyExists  = errors.New("nzb already exists")
	ErrNzbNotFound       = errors.New("nzb not found")
	ErrHealthCheckFailed = errors.New("health check failed")
)

// Add parsed nzb-data, and wait for it. Add() is the same thing without the
// wait.
func (s *Service) AddNzb(nzbData *nzbparser.NzbData) error {
	if err := s.enqueue(nzbData, ""); err != nil {
		return err
	}

	err := s.addNzb(nzbData, true)
	s.finish(nzbData.MetaName, err)

	return err
}

// addNzb builds the tree for an nzb. isNew separates an add from restoring what
// the store already holds.
func (s *Service) addNzb(nzbData *nzbparser.NzbData, isNew bool) (err error) {
	release := s.acquireAdd()
	defer release()

	logger.Debug("Adding nzb", "MetaName", nzbData.MetaName)

	s.mutex.Lock()
	if _, exists := s.nzbFiledata[nzbData.MetaName]; exists {
		s.mutex.Unlock()
		return ErrNzbAlreadyExists
	}
	s.nzbFiledata[nzbData.MetaName] = nzbData
	s.mutex.Unlock()

	// The entry reserves the name for this add, so an add that fails has to give
	// it back along with whatever it already presented; keeping either would make
	// the nzb un-re-addable
	defer func() {
		if err != nil {
			s.mutex.Lock()
			s.unregister(nzbData.MetaName)
			s.mutex.Unlock()
		}
	}()

	s.filterNzbFiles(nzbData)
	if len(nzbData.Files) == 0 {
		logger.Warn("After blacklist, no nzb-files left", "MetaName", nzbData.MetaName)
		return nil
	}

	// Verify the posts still exist before building anything on top of them
	if isNew {
		if err := s.stage(nzbData.MetaName, StageChecking); err != nil {
			return err
		}

		if healthErrors := s.healthChecker.CheckFiles(nzbData); len(healthErrors) > 0 {
			for _, err := range healthErrors {
				logger.Warn("Unhealthy file detected",
					"nzb", nzbData.MetaName,
					"error", err)
			}
			return fmt.Errorf("%w: %d files beyond repair", ErrHealthCheckFailed, len(healthErrors))
		}
	}

	// The archive walk behind this is seconds and a cancel is waiting on it, so
	// it is the boundary worth checking
	if err := s.stage(nzbData.MetaName, StageBuilding); err != nil {
		return err
	}

	tree, err := s.buildTree(nzbData)
	if err != nil {
		return err
	}
	if len(tree) == 0 {
		logger.Warn("After blacklist, no files left", "MetaName", nzbData.MetaName)
		return nil
	}

	s.register(nzbData, tree)
	s.measure(nzbData.MetaName, tree)
	s.storeFiles(nzbData.MetaName, tree)

	// The record already holds it: enqueue wrote it there when the add was
	// accepted, and finish records how this ends

	logger.Info("Added nzb", "MetaName", nzbData.MetaName)

	return nil
}

// filterNzbFiles drops what the early blacklist matches. A file dropped here is
// not built, not presented and not health-checked; the par2 files the check
// needs for its verdict are hidden by FILESYSTEM_BLACKLIST instead, which drops
// them after they have been counted.
func (s *Service) filterNzbFiles(nzbData *nzbparser.NzbData) {
	for i := len(nzbData.Files) - 1; i >= 0; i-- {
		if s.isBlacklistedNzbFile(nzbData.Files[i].Filename) {
			nzbData.Files = append(nzbData.Files[:i], nzbData.Files[i+1:]...)
		}
	}
}

// buildTree turns an nzb into the files it presents, keyed by the path each is
// presented under. It is the whole naming decision - the blacklists,
// deobfuscation and flattening - and the stored tree is a cache of its answer,
// which is why a restore that goes on to build runs exactly this.
func (s *Service) buildTree(nzbData *nzbparser.NzbData) (map[string]presentation.Openable, error) {
	s.filterNzbFiles(nzbData)

	files, err := s.factory.BuildSegmentStackFromNzbData(nzbData)
	if err != nil {
		return nil, fmt.Errorf("failed building segment-stack for %s: %w", nzbData.MetaName, err)
	}

	for name := range files {
		if s.isBlacklistedFilename(name) {
			delete(files, name)
		}
	}

	paths := make([]string, 0, len(files))
	for name := range files {
		paths = append(paths, name)
	}

	tree := make(map[string]presentation.Openable, len(files))
	for filepath, file := range files {
		filepath = s.deobfuscateFilename(filepath, paths, nzbData)
		filepath = s.flattenPath(filepath, paths)
		tree[path.Join(nzbData.MetaName, filepath)] = file
	}

	return tree, nil
}

// register presents a tree and records what it presents.
func (s *Service) register(nzbData *nzbparser.NzbData, tree map[string]presentation.Openable) {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	modTime := time.Time{}
	if len(nzbData.Files) > 0 {
		modTime = nzbData.Files[0].ParsedDate
	}

	s.nzbFiles[nzbData.MetaName] = make([]string, 0, len(tree))
	for fullPath, file := range tree {
		s.nzbFiles[nzbData.MetaName] = append(s.nzbFiles[nzbData.MetaName], fullPath)

		for _, presenter := range s.presenters {
			if err := presenter.AddFile(fullPath, modTime, file); err != nil {
				logger.Error("Failed adding segment-stack as file", "nzb", nzbData.MetaName, "error", err)
			}
		}
	}
}

// measure settles the size of the classes NZB_EAGER_EXACT_SIZE_CLASSES names,
// before the add reports finished, since a client that imports on that stats the
// file first. A rebuild does it too, and pays nothing for it: the store hands
// back the lengths the first measurement learned.
func (s *Service) measure(metaName string, tree map[string]presentation.Openable) {
	for fullPath, file := range tree {
		if !slices.Contains(s.exactSizeClasses, filenameops.Classify(fullPath)) {
			continue
		}
		if err := measureFile(file); err != nil {
			// A size that could not be measured is a worse size, not a missing file
			logger.Warn("Failed measuring file, leaving the estimate in place",
				"nzb", metaName, "file", fullPath, "error", err)
		}
	}
}

// measureFile seeks to the end so the parts that did not know their own length
// do from here on, which with a size convention identified is the tail segment.
// A decoder stream is left alone: its length is exact from the archive header
// and seeking it would decode the whole member.
func measureFile(file presentation.Openable) error {
	// A file that already answers exactly has nothing to measure, and opening it
	// to find that out would walk an archives headers
	if sized, ok := file.(resource.Sized); ok {
		if _, err := sized.Size(); err == nil {
			return nil
		}
	}

	reader, err := file.Open()
	if err != nil {
		return fmt.Errorf("failed opening file: %w", err)
	}
	defer reader.Close()

	if _, addressable := reader.(io.ReaderAt); !addressable {
		return nil
	}

	if _, err := reader.Seek(0, io.SeekEnd); err != nil {
		return fmt.Errorf("failed seeking to the end: %w", err)
	}
	return nil
}

func (s *Service) isBlacklistedFilename(filename string) bool {
	for i := range s.fileBlacklist {
		if s.fileBlacklist[i].MatchString(filename) {
			return true
		}
	}
	return false
}

func (s *Service) isBlacklistedNzbFile(filename string) bool {
	for i := range s.nzbFileBlacklist {
		if s.nzbFileBlacklist[i].MatchString(filename) {
			return true
		}
	}
	return false
}

// deobfuscateFilename names a file after the nzb where its own name says
// nothing. Only content is renamed: an .nfo or an .sfv carries no obfuscated
// name worth repairing, and naming it after the release makes it look like one.
func (s *Service) deobfuscateFilename(filepath string, paths []string, nzbData *nzbparser.NzbData) string {
	if filenameops.Classify(filepath) != filenameops.ClassContent {
		return filepath
	}

	filename := path.Base(filepath)
	basePath := strings.TrimLeft(filepath[:len(filepath)-len(filename)], "/")
	fileExtension := path.Ext(filename)

	// If only item with extension in folder
	filesInFolder := listItemsInFolder(basePath, paths)
	filesByExtension := groupFilesByExtension(filesInFolder)
	if len(filesByExtension[fileExtension]) == 1 {
		replacement := nzbData.MetaName
		if basePath != "" {
			// When folder fuzzy-checks above nzb-name, prefer it as replacement
			foldername := path.Base(basePath)
			folderBase := filenameops.GetBaseFilename(foldername)
			if 1-float32(levenshtein.ComputeDistance(folderBase, replacement))/float32(len(replacement)) >= s.filenameReplacementBelowLevensteinRatio {
				replacement = folderBase
			}
		}
		// Apply Fuzzy-check
		fileBase := filename[:len(filename)-len(fileExtension)]
		if 1-float32(levenshtein.ComputeDistance(fileBase, replacement))/float32(len(replacement)) < s.filenameReplacementBelowLevensteinRatio {
			filename = replacement + fileExtension
		}
	}

	return path.Join(basePath, filename)
}

// flattenPath will remove as many folders from the file, starting from the left up to pathFlatteningDepth, and return the resulting file
func (s *Service) flattenPath(file string, files []string) (newFile string) {
	// Extract folders of search-path
	folders := strings.SplitN(file, "/", s.pathFlatteningDepth+1)
	folders = folders[:len(folders)-1]

	maxDepth := len(folders)
	if s.pathFlatteningDepth < maxDepth {
		maxDepth = s.pathFlatteningDepth
	}

	folderPrefix := ""
	for i := range maxDepth {
		// Build folders from left to right, up to max depth
		folderPrefix = path.Join(folderPrefix, folders[i])

		// Count prefix-matching items in paths
		// If only 1 found, cut folder-prefix so far of path and return new path
		if len(listItemsInFolder(folderPrefix, files)) == 1 {
			newFile = file[len(folderPrefix)+1:]
		}
	}

	if newFile == "" {
		return file
	}
	return path.Clean(newFile)
}

func listItemsInFolder(folder string, files []string) (foundFiles []string) {
	if folder == "." {
		folder = ""
	}
	folder = strings.TrimLeft(folder, "/")

	for _, file := range files {
		// Match folder
		if after, found := strings.CutPrefix(file, folder); found {
			after = strings.TrimLeft(after, "/")
			// Skip if in subfolder
			if strings.Contains(after, "/") {
				continue
			}

			foundFiles = append(foundFiles, after)
		}
	}
	return foundFiles
}

func groupFilesByExtension(files []string) (filesByExtension map[string][]string) {
	filesByExtension = make(map[string][]string, 1)
	for _, file := range files {
		extension := path.Ext(file)
		filesByExtension[extension] = append(filesByExtension[extension], file)
	}
	return filesByExtension
}

func (s *Service) RemoveNzb(nzbData *nzbparser.NzbData) error {
	return s.Delete(nzbData.MetaName)
}

// Delete removes an nzb and everything recorded about it: the files it
// presents, the segment data it accumulated, and the record of the add. The two
// go together - files nothing can report on, and a report on files that are
// gone, are both states nobody can act on - which is why an add still running
// is refused here and belongs to Cancel.
//
// An add that failed left no files, so deleting it is deleting its record.
func (s *Service) Delete(id string) error {
	s.queueMutex.Lock()
	item := s.find(id)
	if item != nil && !item.Done() {
		s.queueMutex.Unlock()
		return fmt.Errorf("%w: %s", ErrNzbStillRunning, id)
	}
	s.remove(id)
	s.queueMutex.Unlock()

	s.mutex.Lock()
	registered, built := s.nzbFiledata[id]
	s.unregister(id)
	s.mutex.Unlock()

	if item == nil && !built {
		return fmt.Errorf("%w: %s", ErrNzbNotFound, id)
	}

	logger.Debug("Removing nzb", "MetaName", id)

	// Thousands of unlinks and a database write, so it happens once the nzb is
	// out of the presenters rather than under the lock every read waits on. The
	// registered data is what was actually built; the caller may hold a re-parse
	// of the same nzb with fewer files if a blacklist changed since.
	if built {
		s.factory.DiscardSegmentStackFromNzbData(registered)
	}

	if err := s.store.Delete(id); err != nil {
		return fmt.Errorf("failed removing nzb %s from store: %w", id, err)
	}

	logger.Info("Removed nzb", "MetaName", id)
	return nil
}

// unregister takes an nzb's files back out of the presenters and drops its
// tracking entries. Caller holds the mutex.
func (s *Service) unregister(metaName string) {
	for _, filepath := range s.nzbFiles[metaName] {
		for _, presenter := range s.presenters {
			if err := presenter.RemoveFile(filepath); err != nil {
				logger.Error("Failed removing file from presenter",
					"nzb", metaName,
					"file", filepath,
					"error", err)
			}
		}
	}

	delete(s.nzbFiledata, metaName)
	delete(s.nzbFiles, metaName)
}

// Files returns the final paths exposed for each NZB.
func (s *Service) Files() map[string][]string {
	s.mutex.RLock()
	defer s.mutex.RUnlock()

	files := make(map[string][]string, len(s.nzbFiles))
	for id, paths := range s.nzbFiles {
		paths = slices.Clone(paths)
		slices.Sort(paths)
		files[id] = slices.Compact(paths)
	}
	return files
}
