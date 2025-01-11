package nzbStore

import "git.ruekov.eu/ruakij/nzbStreamer/pkg/nzbParser"

type NzbStore interface {
	List() ([]nzbParser.NzbData, error)
	Set(*nzbParser.NzbData) error
	Delete(*nzbParser.NzbData) error
}
