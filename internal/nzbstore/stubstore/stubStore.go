package stubstore

import (
	"git.ruekov.eu/ruakij/nzbStreamer/pkg/nzbparser"
)

// StubStore is a store which does nothing
type StubStore struct{}

func NewStubStore() *StubStore {
	return &StubStore{}
}

func (s *StubStore) List() ([]nzbparser.NzbData, error) {
	return nil, nil
}

func (s *StubStore) Set(*nzbparser.NzbData) error {
	return nil
}

func (s *StubStore) Delete(*nzbparser.NzbData) error {
	return nil
}
