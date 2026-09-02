package folderwatcherblackhole

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"git.ruekov.eu/ruakij/nzbStreamer/pkg/nzbparser"
)

var logger = slog.With("Module", "FolderWatcherBlackhole")

var ErrUnknownListener = errors.New("unknown listener id")

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
		logger.Error("Failed to open file", "filename", filename, "err", err)
		return
	}
	defer file.Close()

	nzbData, err := nzbparser.ParseNzb(file, filename)
	if err != nil {
		logger.Error("Failed to parse nzb", "filename", filename, "err", err)
		return
	}

	fw.wg.Add(1)
	defer fw.wg.Done()

	if len(fw.addHooks) == 0 {
		logger.Warn("Cannot notify, no listeners found", "filename", filename)
	}

	for _, hook := range fw.addHooks {
		if hook == nil {
			continue
		}
		err := hook(nzbData)
		if err != nil {
			logger.Error("Error executing hook", "filename", filename, "err", err)
		}
	}

	err = os.Remove(filePath)
	if err != nil {
		logger.Error("Error deleting file", "filename", filename, "err", err)
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

	if listenerID < 0 || listenerID >= len(fw.addHooks) {
		return fmt.Errorf("%w: %d", ErrUnknownListener, listenerID)
	}

	// Clear the slot rather than removing it, so the ids handed out earlier keep
	// pointing at the same listener
	fw.addHooks[listenerID] = nil
	fw.removeHooks[listenerID] = nil

	return nil
}

// StopWatching stops the folder monitoring
func (fw *folderWatcherBlackhole) StopWatching() {
	close(fw.stopChan)
	fw.wg.Wait()
}
