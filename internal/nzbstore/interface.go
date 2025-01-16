package nzbstore

import "git.ruekov.eu/ruakij/nzbStreamer/pkg/nzbparser"

type NzbStore interface {
	List() ([]nzbparser.NzbData, error)
	Set(*nzbparser.NzbData) error
	Delete(*nzbparser.NzbData) error
}
