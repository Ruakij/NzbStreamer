package folderWatcherBlackhole

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"git.ruekov.eu/ruakij/nzbStreamer/pkg/nzbParser"
	"golang.org/x/exp/slices"
)

// FolderWatcherBlackhole notifies listeners about new files in directory, after which the files are deleted
type folderWatcherBlackhole struct {
	watchFolder   string
	addHooks      []func(nzbData *nzbParser.NzbData) error
	removeHooks   []func(nzbData *nzbParser.NzbData) error
	listenerCount int
	mu            sync.Mutex
	wg            sync.WaitGroup
	stopChan      chan struct{}
}

func NewFolderWatcher(folder string) *folderWatcherBlackhole {
	fw := &folderWatcherBlackhole{
		watchFolder: folder,
	}

	fw.scanDirectory()
	go fw.startPeriodicScan(10 * time.Second)

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
		slog.Error("Error reading directory:", err)
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
		slog.Error("Failed to open file", filePath, err)
	}

	nzbData, err := nzbParser.ParseNzb(file)
	if err != nil {
		slog.Error("Failed parse nzb", filePath, err)
	}

	fw.wg.Add(1)
	defer fw.wg.Done()

	if len(fw.addHooks) == 0 {
		slog.Warn("Cannot notify for event, no listeners found", filePath)
	}

	for _, hook := range fw.addHooks {
		err := hook(nzbData)
		if err != nil {
			fmt.Println("Error executing hook:", err)
		}
	}

	err = os.Remove(filePath)
	if err != nil {
		slog.Error("Error deleting file", filePath, err)
	}
}

// AddListener adds listener hooks and returns an ID
func (fw *folderWatcherBlackhole) AddListener(addHook, removeHook func(nzbData *nzbParser.NzbData) error) (int, error) {
	fw.mu.Lock()
	defer fw.mu.Unlock()

	listenerId := len(fw.addHooks)

	fw.addHooks = append(fw.addHooks, addHook)
	fw.removeHooks = append(fw.removeHooks, removeHook)

	return listenerId, nil
}

// RemoveListener removes hooks based on listener ID
func (fw *folderWatcherBlackhole) RemoveListener(listenerId int) error {
	fw.mu.Lock()
	defer fw.mu.Unlock()

	slices.Delete(fw.addHooks, listenerId, listenerId)
	slices.Delete(fw.removeHooks, listenerId, listenerId)

	return nil
}

// StopWatching stops the folder monitoring
func (fw *folderWatcherBlackhole) StopWatching() {
	close(fw.stopChan)
	fw.wg.Wait()
}
