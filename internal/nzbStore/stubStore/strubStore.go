package stubStore

import (
	"git.ruekov.eu/ruakij/nzbStreamer/pkg/nzbParser"
)

// StubStore is a store which does nothing
type StubStore struct {
}

func NewStubStore() *StubStore {
	return &StubStore{}
}

func (s *StubStore) List() (data []nzbParser.NzbData, err error) {
	return
}

func (s *StubStore) Set(*nzbParser.NzbData) (err error) {
	return
}
func (s *StubStore) Delete(*nzbParser.NzbData) (err error) {
	return
}
