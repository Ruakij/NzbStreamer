package nzbservice

import (
	"errors"
	"fmt"
	"log/slog"
	"path"
	"regexp"
	"strings"
	"sync"

	"git.ruekov.eu/ruakij/nzbStreamer/internal/nzbrecordfactory"
	"git.ruekov.eu/ruakij/nzbStreamer/internal/nzbstore"
	"git.ruekov.eu/ruakij/nzbStreamer/internal/presentation"
	"git.ruekov.eu/ruakij/nzbStreamer/internal/trigger"
	"git.ruekov.eu/ruakij/nzbStreamer/pkg/filenameops"
	"git.ruekov.eu/ruakij/nzbStreamer/pkg/nzbparser"
	"github.com/agnivade/levenshtein"
)

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

	// Options
	fileBlacklist                           []regexp.Regexp
	pathFlatteningDepth                     int
	filenameReplacementBelowLevensteinRatio float32
}

func NewService(store nzbstore.NzbStore, factory nzbrecordfactory.Factory, presenters []presentation.Presenter, triggers []trigger.Trigger) *Service {
	triggerListeners := make([]TriggerListener, len(triggers))
	for i, trigger := range triggers {
		triggerListeners[i] = TriggerListener{
			Trigger:    trigger,
			listenerID: -1,
		}
	}

	return &Service{
		store:      store,
		factory:    factory,
		presenters: presenters,
		triggers:   triggerListeners,

		fileBlacklist: []regexp.Regexp{},
		nzbFiledata:   make(map[string]*nzbparser.NzbData),
	}
}

func (s *Service) SetBlacklist(blacklist []regexp.Regexp) {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	s.fileBlacklist = blacklist
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

// Initialize the service; Load NzbData from store; build filedata and add to filesystem; Register to triggers
func (s *Service) Init() error {
	slog.Debug("Getting nzbData from store")
	nzbData, err := s.store.List()
	if err != nil {
		return fmt.Errorf("failed listing nzbs in store: %w", err)
	}
	slog.Info("Loaded Nzb store", "items", len(nzbData))

	for _, nzb := range nzbData {
		err := s.AddNzb(&nzb)
		if err != nil {
			slog.Error("Couldnt add nzb", "error", err)
		}
	}

	slog.Debug("Registering at triggers")
	for _, trigger := range s.triggers {
		trigger.listenerID, err = trigger.AddListener(s.AddNzb, s.RemoveNzb)
		if err != nil {
			return fmt.Errorf("failed registering at trigger %v: %w", trigger, err)
		}
	}

	slog.Debug("Init complete")

	return nil
}

var ErrNzbAlreadyExists = errors.New("nzb already exists")

// Add parsed nzb-data
func (s *Service) AddNzb(nzbData *nzbparser.NzbData) error {
	slog.Debug("Adding nzb", "MetaName", nzbData.MetaName)

	s.mutex.Lock()
	if _, exists := s.nzbFiledata[nzbData.MetaName]; exists {
		s.mutex.Unlock()
		return ErrNzbAlreadyExists
	}
	s.nzbFiledata[nzbData.MetaName] = nzbData
	s.mutex.Unlock()

	files, err := s.factory.BuildSegmentStackFromNzbData(nzbData)
	if err != nil {
		return fmt.Errorf("failed building segment-stack for %s: %w", nzbData.MetaName, err)
	}

	// Blacklist
	for path := range files {
		if s.isBlacklistedFilename(path) {
			delete(files, path)
		}
	}

	if len(files) == 0 {
		slog.Warn("After blacklist, no files left", "MetaName", nzbData.MetaName)
		return nil
	}

	// Extract paths
	paths := make([]string, 0, len(files))
	for path := range files {
		paths = append(paths, path)
	}

	for filepath, file := range files {
		filename := path.Base(filepath)
		fileExtension := path.Ext(filename)
		filepath = filepath[:len(filepath)-len(filename)]

		// If only item with extension in folder
		filesInFolder := listItemsInFolder(filepath, paths)
		filesByExtension := groupFilesByExtension(filesInFolder)
		if len(filesByExtension[fileExtension]) == 1 {
			replacement := nzbData.MetaName
			if filepath != "" {
				// When folder fuzzy-checks above nzb-name, prefer it as replacement
				foldername := path.Base(filepath)
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

		// TODO: Flatten
		filepath = s.flattenPath(filepath, paths)

		for _, presenter := range s.presenters {
			err = presenter.AddFile(
				path.Join(nzbData.MetaName, filepath, filename),
				nzbData.Files[0].ParsedDate,
				file,
			)
			if err != nil {
				slog.Error("Failed adding segment-stack as file", nzbData.MetaName, err)
			}
		}
	}

	slog.Info("Added nzb", "MetaName", nzbData.MetaName)

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

// flattenPath will remove as many folders from the file, starting from the left up to pathFlatteningDepth, and remove the resulting file
func (s *Service) flattenPath(file string, files []string) (newFile string) {
	// Extract folders of search-path
	folders := strings.SplitN(file, "/", s.pathFlatteningDepth+1)
	folders = folders[:len(folders)-1]

	maxDepth := len(folders)
	if s.pathFlatteningDepth < maxDepth {
		maxDepth = s.pathFlatteningDepth
	}

	folderPrefix := ""
	for i := 0; i < maxDepth; i++ {
		// Build folders from left to right, up to max depth
		folderPrefix = path.Join(folderPrefix, folders[i])

		// Count prefix-matching items in paths
		// If only 1 found, cut folder-prefix so far of path and return new path
		if len(listItemsInFolder(folderPrefix, files)) == 1 {
			newFile = file[len(folderPrefix):]
		}
	}

	if newFile == "" {
		return file
	}
	return path.Clean(newFile)
}

func listItemsInFolder(folder string, files []string) (foundFiles []string) {
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
	// TODO: Implement
	return nil
}
