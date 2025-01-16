package nzbRecordFactory

import (
	"git.ruekov.eu/ruakij/nzbStreamer/internal/presentation"
	"git.ruekov.eu/ruakij/nzbStreamer/pkg/nzbParser"
)

type Factory interface {
	BuildSegmentStackFromNzbData(nzbData *nzbParser.NzbData) (map[string]presentation.Openable, error)
}
