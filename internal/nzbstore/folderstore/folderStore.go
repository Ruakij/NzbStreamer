package folderstore

import (
	"os"
	"path"
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
		return nil, err
	}

	group := errgroup.Group{}
	mu := sync.Mutex{}
	list := make([]nzbparser.NzbData, 0, len(entries))

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}

		group.Go(func() (err error) {
			file, err := os.Open(path.Join(s.location, entry.Name()))
			if err != nil {
				return
			}

			data, err := nzbparser.ParseNzb(file)
			if err != nil {
				return
			}

			mu.Lock()
			list = append(list, *data)
			mu.Unlock()

			return
		})

	}
	err = group.Wait()

	return list, err
}

func (s *FolderStore) Set(*nzbparser.NzbData) error
func (s *FolderStore) Delete(*nzbparser.NzbData) error
