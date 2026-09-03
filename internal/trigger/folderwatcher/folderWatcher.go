package folderwatcher

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"git.ruekov.eu/ruakij/nzbStreamer/pkg/nzbparser"
	"github.com/fsnotify/fsnotify"
	"golang.org/x/sync/errgroup"
)

var logger = slog.With("Module", "FolderWatcher")

var ErrUnknownListener = errors.New("unknown listener id")

// FolderWatcher notifies listeners about new files in directory
type folderWatcher struct {
	watchFolder string
	addHooks    []func(nzbData *nzbparser.NzbData) error
	removeHooks []func(nzbData *nzbparser.NzbData) error
	mu          sync.Mutex
	wg          sync.WaitGroup
	stopChan    chan struct{}
	// Content hashes already handed to the hooks, so a file re-appearing under
	// another name is one release and a name reused by another release is two.
	// It lives only as long as the process, so a restart re-offers what is still
	// in the folder and the listeners reject what they already hold.
	processed map[string]string
	// What each file looked like during the previous scan; a file that has not
	// changed since then is done being written
	sighted map[string]fileStat
}

type fileStat struct {
	size  int64
	mtime time.Time
}

// NewFolderWatcher creates a new instance of folderWatcher
func NewFolderWatcher(folder string) *folderWatcher {
	return &folderWatcher{
		watchFolder: folder,
		processed:   make(map[string]string),
		sighted:     make(map[string]fileStat),
		stopChan:    make(chan struct{}),
	}
}

const PollingScanTime = 15 * time.Second

func (fw *folderWatcher) Init() {
	// Polling runs regardless, because a file only counts as written once a later
	// scan sees it unchanged and the last write may be the last event
	fw.startPeriodicScan(PollingScanTime)

	err := fw.startFsNotifyScan()
	if err != nil {
		logger.Error("Error when setting up FsNotifyScan, continuing with polling", "error", err)
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

		fw.scanDirectory()

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
	sighted := make(map[string]fileStat, len(files))

	for _, file := range files {
		if file.IsDir() || strings.ToLower(filepath.Ext(file.Name())) != ".nzb" {
			continue
		}

		info, err := file.Info()
		if err != nil {
			logger.Error("Failed to stat file", "filename", file.Name(), "err", err)
			continue
		}

		stat := fileStat{size: info.Size(), mtime: info.ModTime()}
		sighted[file.Name()] = stat
		if fw.sighted[file.Name()] != stat {
			continue
		}

		content, err := os.ReadFile(filepath.Join(fw.watchFolder, file.Name()))
		if err != nil {
			logger.Error("Failed to read file", "filename", file.Name(), "err", err)
			continue
		}

		// Keyed by content, so a broken or half-written file gets another chance
		// once it is rewritten, while an unchanged one is not retried forever
		sum := sha256.Sum256(content)
		hash := string(sum[:])
		if seenAs, done := fw.processed[hash]; done {
			if seenAs != file.Name() {
				logger.Debug("Skipping file already processed under another name", "filename", file.Name(), "seenAs", seenAs)
			}
			continue
		}
		fw.processed[hash] = file.Name()

		group.Go(func() error {
			fw.processFile(file.Name(), content)
			return nil
		})
	}

	fw.sighted = sighted

	//nolint:errcheck // because there will never be an error
	_ = group.Wait()
}

// processFile triggers the addHooks for the file
func (fw *folderWatcher) processFile(filename string, content []byte) {
	nzbData, err := nzbparser.ParseNzb(bytes.NewReader(content), filename)
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
		if hook == nil {
			continue
		}
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
func (fw *folderWatcher) StopWatching() {
	close(fw.stopChan)
	fw.wg.Wait()
}
