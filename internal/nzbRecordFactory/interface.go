package nzbRecordFactory

import (
	"git.ruekov.eu/ruakij/nzbStreamer/pkg/SimpleWebdavFilesystem"
	"git.ruekov.eu/ruakij/nzbStreamer/pkg/nzbParser"
)

type Factory interface {
	BuildSegmentStackFromNzbData(nzbData *nzbParser.NzbData) (map[string]SimpleWebdavFilesystem.Openable, error)
}
