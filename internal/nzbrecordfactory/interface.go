package nzbrecordfactory

import (
	"git.ruekov.eu/ruakij/nzbStreamer/internal/presentation"
	"git.ruekov.eu/ruakij/nzbStreamer/pkg/nzbparser"
)

type Factory interface {
	BuildSegmentStackFromNzbData(nzbData *nzbparser.NzbData) (map[string]presentation.Openable, error)
	// DiscardSegmentStackFromNzbData throws away what the stack accumulated for
	// an nzb nobody will read again.
	DiscardSegmentStackFromNzbData(nzbData *nzbparser.NzbData)
}
