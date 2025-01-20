package nzbstore

import "git.ruekov.eu/ruakij/nzbStreamer/pkg/nzbparser"

type NzbStore interface {
	List() ([]nzbparser.NzbData, error)
	Set(data *nzbparser.NzbData) error
	Delete(data *nzbparser.NzbData) error
}
