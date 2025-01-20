package folderwatcherblackhole

import (
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"git.ruekov.eu/ruakij/nzbStreamer/pkg/nzbparser"
	"golang.org/x/exp/slices"
)

var logger = slog.With("Module", "FolderWatcherBlackhole")

// FolderWatcherBlackhole notifies listeners about new files in directory, after which the files are deleted
type folderWatcherBlackhole struct {
	watchFolder string
	addHooks    []func(nzbData *nzbparser.NzbData) error
	removeHooks []func(nzbData *nzbparser.NzbData) error
	mu          sync.Mutex
	wg          sync.WaitGroup
	stopChan    chan struct{}
}

const PollingScanTime = 15 * time.Second

func NewFolderWatcher(folder string) *folderWatcherBlackhole {
	fw := &folderWatcherBlackhole{
		watchFolder: folder,
	}

	fw.scanDirectory()
	go fw.startPeriodicScan(PollingScanTime)

	return fw
}

// startPeriodicScan periodically checks the directory for new files
func (fw *folderWatcherBlackhole) startPeriodicScan(interval time.Duration) {
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
func (fw *folderWatcherBlackhole) scanDirectory() {
	fw.mu.Lock()
	defer fw.mu.Unlock()

	files, err := os.ReadDir(fw.watchFolder)
	if err != nil {
		logger.Error("Error reading directory", "error", err)
		return
	}

	for _, file := range files {
		if !file.IsDir() && filepath.Ext(file.Name()) == ".nzb" {
			fw.processFile(file.Name())
		}
	}
}

// processFile triggers the addHooks for the file then deletes it
func (fw *folderWatcherBlackhole) processFile(filename string) {
	filePath := filepath.Join(fw.watchFolder, filename)

	file, err := os.Open(filePath)
	if err != nil {
		logger.Error("Failed to open file", filePath, err)
	}

	nzbData, err := nzbparser.ParseNzb(file)
	if err != nil {
		logger.Error("Failed parse nzb", filePath, err)
	}

	fw.wg.Add(1)
	defer fw.wg.Done()

	if len(fw.addHooks) == 0 {
		logger.Warn("Cannot notify for event, no listeners found", "filepath", filePath)
	}

	for _, hook := range fw.addHooks {
		err := hook(nzbData)
		if err != nil {
			logger.Error("Error executing hook:", "error", err)
		}
	}

	err = os.Remove(filePath)
	if err != nil {
		logger.Error("Error deleting file", "filepath", filePath, "error", err)
	}
}

// AddListener adds listener hooks and returns an ID
func (fw *folderWatcherBlackhole) AddListener(addHook, removeHook func(nzbData *nzbparser.NzbData) error) (int, error) {
	fw.mu.Lock()
	defer fw.mu.Unlock()

	listenerID := len(fw.addHooks)

	fw.addHooks = append(fw.addHooks, addHook)
	fw.removeHooks = append(fw.removeHooks, removeHook)

	return listenerID, nil
}

// RemoveListener removes hooks based on listener ID
func (fw *folderWatcherBlackhole) RemoveListener(listenerID int) error {
	fw.mu.Lock()
	defer fw.mu.Unlock()

	slices.Delete(fw.addHooks, listenerID, listenerID)
	slices.Delete(fw.removeHooks, listenerID, listenerID)

	return nil
}

// StopWatching stops the folder monitoring
func (fw *folderWatcherBlackhole) StopWatching() {
	close(fw.stopChan)
	fw.wg.Wait()
}
