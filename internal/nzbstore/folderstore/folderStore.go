package folderstore

import (
	"encoding/xml"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"git.ruekov.eu/ruakij/nzbStreamer/pkg/nzbparser"
	"golang.org/x/sync/errgroup"
)

// FolderStore Handles
type FolderStore struct {
	mu       sync.RWMutex
	location string
}

func NewFolderStore(location string) *FolderStore {
	return &FolderStore{
		location: location,
	}
}

func (s *FolderStore) List() ([]nzbparser.NzbData, error) {
	entries, err := os.ReadDir(s.location)
	if err != nil {
		return nil, fmt.Errorf("failed to read directory %s: %w", s.location, err)
	}

	group := errgroup.Group{}
	mu := sync.Mutex{}
	list := make([]nzbparser.NzbData, 0, len(entries))

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}

		entryName := entry.Name()
		group.Go(func() error {
			file, err := os.Open(filepath.Join(s.location, entryName))
			if err != nil {
				return fmt.Errorf("failed to open file %s: %w", entryName, err)
			}
			defer file.Close()

			var data nzbparser.NzbData
			decoder := xml.NewDecoder(file)
			if err := decoder.Decode(&data); err != nil {
				return fmt.Errorf("failed to decode XML from %s: %w", entryName, err)
			}

			mu.Lock()
			list = append(list, data)
			mu.Unlock()

			return nil
		})
	}

	if err := group.Wait(); err != nil {
		return nil, fmt.Errorf("error processing files: %w", err)
	}

	return list, nil
}

func sanitizeFilename(name string) string {
	// Replace invalid characters with underscore
	invalid := []string{"/", "\\", ":", "*", "?", "\"", "<", ">", "|"}
	result := name
	for _, char := range invalid {
		result = strings.ReplaceAll(result, char, "_")
	}
	return result + ".nzb"
}

func (s *FolderStore) Set(data *nzbparser.NzbData) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	filename := sanitizeFilename(data.MetaName)
	filepath := filepath.Join(s.location, filename)

	file, err := os.Create(filepath)
	if err != nil {
		return fmt.Errorf("failed to create file %s: %w", filename, err)
	}
	defer file.Close()

	encoder := xml.NewEncoder(file)
	encoder.Indent("", "  ")
	if err := encoder.Encode(data); err != nil {
		return fmt.Errorf("failed to encode NZB data to %s: %w", filename, err)
	}

	return nil
}

func (s *FolderStore) Delete(data *nzbparser.NzbData) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	filename := sanitizeFilename(data.MetaName)
	filepath := filepath.Join(s.location, filename)

	if err := os.Remove(filepath); err != nil {
		if os.IsNotExist(err) {
			return nil // File already doesn't exist, not an error
		}
		return fmt.Errorf("failed to delete file %s: %w", filename, err)
	}

	return nil
}
