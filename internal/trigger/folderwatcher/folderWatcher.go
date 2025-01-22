package folderwatcher

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"git.ruekov.eu/ruakij/nzbStreamer/pkg/nzbparser"
	"github.com/fsnotify/fsnotify"
	"golang.org/x/sync/errgroup"
)

var logger = slog.With("Module", "FolderWatcher")

// FolderWatcher notifies listeners about new files in directory
type Listener struct {
	add, remove func(nzbData *nzbparser.NzbData) error
}

type folderWatcher struct {
	watchFolder string
	listeners   []Listener
	mu          sync.Mutex
	wg          sync.WaitGroup
	stopChan    chan struct{}
	nzbDataMap  map[string]*nzbparser.NzbData // Store NzbData for processed files
}

// NewFolderWatcher creates a new instance of folderWatcher
func NewFolderWatcher(folder string) *folderWatcher {
	return &folderWatcher{
		watchFolder: folder,
		nzbDataMap:  make(map[string]*nzbparser.NzbData),
		stopChan:    make(chan struct{}),
	}
}

const PollingScanTime = 15 * time.Second

func (fw *folderWatcher) Init() {
	go fw.scanDirectory()
	err := fw.startFsNotifyScan()
	if err != nil {
		logger.Error("Error when setting up FsNotifyScan, continuing with polling", "error", err)
		fw.startPeriodicScan(PollingScanTime)
	}
}

// startFsNotifyScan uses fsnotify to detect changes on disk
func (fw *folderWatcher) startFsNotifyScan() error {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return fmt.Errorf("failed creating fsnotify watcher: %w", err)
	}

	err = watcher.Add(fw.watchFolder)
	if err != nil {
		watcher.Close()
		return fmt.Errorf("failed adding folder %s to watch: %w", fw.watchFolder, err)
	}

	go func() {
		defer watcher.Close()
		for range watcher.Events {
			fw.scanDirectory()
		}
	}()

	return nil
}

// startPeriodicScan periodically checks the directory for new files
func (fw *folderWatcher) startPeriodicScan(interval time.Duration) {
	ticker := time.NewTicker(interval)

	go func() {
		defer ticker.Stop()

		for {
			select {
			case <-fw.stopChan:
				return
			case <-ticker.C:
				fw.scanDirectory()
			}
		}
	}()
}

// scanDirectory scans the directory and processes each .nzb file found
func (fw *folderWatcher) scanDirectory() {
	fw.mu.Lock()
	defer fw.mu.Unlock()

	currentFiles := make(map[string]struct{})
	files, err := os.ReadDir(fw.watchFolder)
	if err != nil {
		logger.Error("Error reading directory", "err", err)
		return
	}

	group := errgroup.Group{}

	for _, file := range files {
		if !file.IsDir() && strings.ToLower(filepath.Ext(file.Name())) == ".nzb" {
			currentFiles[file.Name()] = struct{}{}
			if _, processed := fw.nzbDataMap[file.Name()]; !processed {
				fileName := file.Name() // Create a copy for the closure
				group.Go(func() error {
					fw.handleAddedFile(filepath.Join(fw.watchFolder, fileName))
					return nil
				})
			}
		}
	}

	// Check for removed files
	for fileName := range fw.nzbDataMap {
		if _, exists := currentFiles[fileName]; !exists {
			fw.handleRemovedFile(filepath.Join(fw.watchFolder, fileName))
		}
	}

	//nolint:errcheck // because there will never be an error
	_ = group.Wait()
}

// handleRemovedFile triggers the removeHooks for the file
func (fw *folderWatcher) handleRemovedFile(filePath string) {
	fw.mu.Lock()
	defer fw.mu.Unlock()

	fileName := filepath.Base(filePath)
	if nzbData, exists := fw.nzbDataMap[fileName]; exists {
		fw.wg.Add(1)
		defer fw.wg.Done()

		for _, listener := range fw.listeners {
			if err := listener.remove(nzbData); err != nil {
				logger.Error("Error executing remove hook", "err", err)
			}
		}

		delete(fw.nzbDataMap, fileName)
	}
}

func (fw *folderWatcher) logPlausabilityWarnings(warnings []nzbparser.EncapsulatedError, filePath string) {
	if len(warnings) == 0 {
		return
	}
	var msg strings.Builder
	for i, warn := range warnings {
		if i != 0 {
			msg.WriteString(", ")
		}
		msg.WriteString(fmt.Sprintf("%v", warn))
	}
	logger.Warn("Warnings while checking Nzb", "filePath", filePath, "msg", msg.String())
}

func (fw *folderWatcher) logPlausabilityErrors(errors []nzbparser.EncapsulatedError, filePath string) bool {
	if len(errors) == 0 {
		return false
	}
	var msg strings.Builder
	for i, err := range errors {
		if i != 0 {
			msg.WriteString(", ")
		}
		msg.WriteString(fmt.Sprintf("%v", err))
	}
	logger.Warn("Errors while checking Nzb", "filePath", filePath, "msg", msg.String())
	return true
}

// handleAddedFile triggers the addHooks for the file
func (fw *folderWatcher) handleAddedFile(filePath string) {
	file, err := os.Open(filePath)
	if err != nil {
		logger.Error("Failed to open file", filePath, err)
		return
	}
	defer file.Close()

	nzbData, err := nzbparser.ParseNzb(file)
	if err != nil {
		logger.Error("Failed to parse nzb", "filePath", filePath, "err", err)
		return
	}

	warnings, errors := nzbData.CheckPlausability()
	fw.logPlausabilityWarnings(warnings, filePath)
	if fw.logPlausabilityErrors(errors, filePath) {
		return
	}

	fileName := filepath.Base(filePath)
	fw.mu.Lock()
	fw.nzbDataMap[fileName] = nzbData
	fw.mu.Unlock()

	fw.wg.Add(1)
	defer fw.wg.Done()

	if len(fw.listeners) == 0 {
		logger.Warn("Cannot notify, no listeners found", "filePath", filePath)
		return
	}

	for _, listener := range fw.listeners {
		if err := listener.add(nzbData); err != nil {
			logger.Error("Error executing hook:", "err", err)
		}
	}
}

// AddListener adds listener hooks and returns an ID
func (fw *folderWatcher) AddListener(addHook, removeHook func(nzbData *nzbparser.NzbData) error) (int, error) {
	fw.mu.Lock()
	defer fw.mu.Unlock()

	listenerID := len(fw.listeners)
	fw.listeners = append(fw.listeners, Listener{add: addHook, remove: removeHook})

	return listenerID, nil
}

// RemoveListener removes hooks based on listener ID
func (fw *folderWatcher) RemoveListener(listenerID int) error {
	fw.mu.Lock()
	defer fw.mu.Unlock()

	fw.listeners = slices.Delete(fw.listeners, listenerID, listenerID+1)
	return nil
}

// StopWatching stops the folder monitoring
func (fw *folderWatcher) StopWatching() {
	close(fw.stopChan)
	fw.wg.Wait()
}
