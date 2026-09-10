package nzbrecordfactory

import (
	"git.ruekov.eu/ruakij/nzbStreamer/pkg/nzbparser"
)

type Factory interface {
	// BuildSegmentStackFromNzbData returns the files an nzb presents and the
	// nzb's own file each was built from. A returned ErrArchiveLeftPacked
	// comes with a usable tree; any other error does not.
	BuildSegmentStackFromNzbData(nzbData *nzbparser.NzbData, progress ProgressFunc) (BuildResult, error)
	// DiscardSegmentStackFromNzbData throws away what the stack accumulated for
	// an nzb nobody will read again.
	DiscardSegmentStackFromNzbData(nzbData *nzbparser.NzbData)
}
