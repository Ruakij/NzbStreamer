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
type folderWatcher struct {
	watchFolder    string
	addHooks       []func(nzbData *nzbparser.NzbData) error
	removeHooks    []func(nzbData *nzbparser.NzbData) error
	mu             sync.Mutex
	wg             sync.WaitGroup
	stopChan       chan struct{}
	processedFiles map[string]struct{} // Store processed file names
}

// NewFolderWatcher creates a new instance of folderWatcher
func NewFolderWatcher(folder string) *folderWatcher {
	return &folderWatcher{
		watchFolder:    folder,
		processedFiles: make(map[string]struct{}), // Initialize the map
		stopChan:       make(chan struct{}),
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

	files, err := os.ReadDir(fw.watchFolder)
	if err != nil {
		logger.Error("Error reading directory", "err", err)
		return
	}

	group := errgroup.Group{}

	for _, file := range files {
		if !file.IsDir() && strings.ToLower(filepath.Ext(file.Name())) == ".nzb" {
			// Check if the file has been processed
			if _, processed := fw.processedFiles[file.Name()]; !processed {
				fw.processedFiles[file.Name()] = struct{}{}
				group.Go(func() error {
					fw.processFile(file.Name())
					return nil
				})
			}
		}
	}

	//nolint:errcheck // because there will never be an error
	_ = group.Wait()
}

// processFile triggers the addHooks for the file
func (fw *folderWatcher) processFile(filename string) {
	filePath := filepath.Join(fw.watchFolder, filename)
	file, err := os.Open(filePath)
	if err != nil {
		logger.Error("Failed to open file", "filename", filename, "err", err)
		return
	}
	defer file.Close() // Ensure the file is closed after processing

	nzbData, err := nzbparser.ParseNzb(file)
	if err != nil {
		logger.Error("Failed to parse nzb", "filename", filename, "err", err)
		return
	}

	warnings, errors := nzbData.CheckPlausability()
	if len(warnings) > 0 {
		var msg strings.Builder
		for i, warn := range warnings {
			if i != 0 {
				msg.WriteString(", ")
			}
			msg.WriteString(fmt.Sprintf("%v", warn))
		}
		logger.Warn("Warnings while checking Nzb", "filename", filename, "msg", msg.String())
	}
	if len(errors) > 0 {
		var msg strings.Builder
		for i, err := range errors {
			if i != 0 {
				msg.WriteString(", ")
			}
			msg.WriteString(fmt.Sprintf("%v", err))
		}
		logger.Warn("Errors while checking Nzb", "filename", filename, "msg", msg.String())
		return
	}

	fw.wg.Add(1)
	defer fw.wg.Done()

	if len(fw.addHooks) == 0 {
		logger.Warn("Cannot notify, no listeners found", "filename", filename)
		return
	}

	for _, hook := range fw.addHooks {
		err := hook(nzbData)
		if err != nil {
			logger.Error("Error executing hook:", "filename", filename, "err", err)
		}
	}
}

// AddListener adds listener hooks and returns an ID
func (fw *folderWatcher) AddListener(addHook, removeHook func(nzbData *nzbparser.NzbData) error) (int, error) {
	fw.mu.Lock()
	defer fw.mu.Unlock()

	listenerID := len(fw.addHooks)

	fw.addHooks = append(fw.addHooks, addHook)
	fw.removeHooks = append(fw.removeHooks, removeHook)

	return listenerID, nil
}

// RemoveListener removes hooks based on listener ID
func (fw *folderWatcher) RemoveListener(listenerID int) error {
	fw.mu.Lock()
	defer fw.mu.Unlock()

	_ = slices.Delete(fw.addHooks, listenerID, listenerID)
	_ = slices.Delete(fw.removeHooks, listenerID, listenerID)

	return nil
}

// StopWatching stops the folder monitoring
func (fw *folderWatcher) StopWatching() {
	close(fw.stopChan)
	fw.wg.Wait()
}
