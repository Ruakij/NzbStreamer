package folderWatcher

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"git.ruekov.eu/ruakij/nzbStreamer/pkg/nzbParser"
	"golang.org/x/exp/slices"
	"golang.org/x/sync/errgroup"
)

// FolderWatcher notifies listeners about new files in directory
type folderWatcher struct {
	watchFolder    string
	addHooks       []func(nzbData *nzbParser.NzbData) error
	removeHooks    []func(nzbData *nzbParser.NzbData) error
	listenerCount  int
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

func (fw *folderWatcher) Init() {
	go fw.scanDirectory()
	go fw.startPeriodicScan(10 * time.Second)
}

// startPeriodicScan periodically checks the directory for new files
func (fw *folderWatcher) startPeriodicScan(interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-fw.stopChan:
			return
		case <-ticker.C:
			fw.scanDirectory()
		}
	}
}

// scanDirectory scans the directory and processes each .nzb file found
func (fw *folderWatcher) scanDirectory() {
	fw.mu.Lock()
	defer fw.mu.Unlock()

	files, err := os.ReadDir(fw.watchFolder)
	if err != nil {
		slog.Error("Error reading directory", "err", err)
		return
	}

	group := errgroup.Group{}

	for _, file := range files {
		if !file.IsDir() && filepath.Ext(file.Name()) == ".nzb" {
			// Check if the file has been processed
			if _, processed := fw.processedFiles[file.Name()]; !processed {
				fw.processedFiles[file.Name()] = struct{}{}
				group.Go(func() (err error) {
					fw.processFile(file.Name())
					return
				})
			}
		}
	}

	group.Wait()
}

// processFile triggers the addHooks for the file
func (fw *folderWatcher) processFile(filename string) {
	filePath := filepath.Join(fw.watchFolder, filename)

	file, err := os.Open(filePath)
	if err != nil {
		slog.Error("Failed to open file", filePath, err)
		return
	}
	defer file.Close() // Ensure the file is closed after processing

	nzbData, err := nzbParser.ParseNzb(file)
	if err != nil {
		slog.Error("Failed to parse nzb", "filename", filename, "err", err)
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
		slog.Warn("Warnings while checking Nzb", "filename", filename, "msg", msg.String())
	}
	if len(errors) > 0 {
		var msg strings.Builder
		for i, err := range errors {
			if i != 0 {
				msg.WriteString(", ")
			}
			msg.WriteString(fmt.Sprintf("%v", err))
		}
		slog.Warn("Errors while checking Nzb", "filename", filename, "msg", msg.String())
		return
	}

	fw.wg.Add(1)
	defer fw.wg.Done()

	if len(fw.addHooks) == 0 {
		slog.Warn("Cannot notify, no listeners found", "filename", filename)
		return
	}

	for _, hook := range fw.addHooks {
		err := hook(nzbData)
		if err != nil {
			slog.Error("Error executing hook:", err)
		}
	}
}

// AddListener adds listener hooks and returns an ID
func (fw *folderWatcher) AddListener(addHook, removeHook func(nzbData *nzbParser.NzbData) error) (int, error) {
	fw.mu.Lock()
	defer fw.mu.Unlock()

	listenerId := len(fw.addHooks)

	fw.addHooks = append(fw.addHooks, addHook)
	fw.removeHooks = append(fw.removeHooks, removeHook)

	return listenerId, nil
}

// RemoveListener removes hooks based on listener ID
func (fw *folderWatcher) RemoveListener(listenerId int) error {
	fw.mu.Lock()
	defer fw.mu.Unlock()

	slices.Delete(fw.addHooks, listenerId, listenerId)
	slices.Delete(fw.removeHooks, listenerId, listenerId)

	return nil
}

// StopWatching stops the folder monitoring
func (fw *folderWatcher) StopWatching() {
	close(fw.stopChan)
	fw.wg.Wait()
}
