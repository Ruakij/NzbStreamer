package filehealth

import (
	"context"

	"git.ruekov.eu/ruakij/nzbStreamer/pkg/nzbparser"
)

// ProgressFunc reports segments probed against segments meant to be probed. The
// total grows while the check runs, since a file that cannot be decided is
// probed again with a wider sample.
type ProgressFunc func(done, total int)

// Checker defines the interface for file health checking
type Checker interface {
	// CheckFiles returns one error per file that is not fully retrievable.
	// progress may be nil. A cancelled ctx stops it probing; what it reports of
	// the files it got to is then nobodys answer, since whoever asked has gone.
	CheckFiles(ctx context.Context, nzbData *nzbparser.NzbData, progress ProgressFunc) []error
	// PlannedProbes is the work a check of this nzb would be, in segments it
	// would ask the server about, without asking about any of them
	PlannedProbes(nzbData *nzbparser.NzbData) int
}
