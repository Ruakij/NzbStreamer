package filehealth

import "git.ruekov.eu/ruakij/nzbStreamer/pkg/nzbparser"

// ProgressFunc reports segments probed against segments meant to be probed. The
// total grows while the check runs, since a file that cannot be decided is
// probed again with a wider sample.
type ProgressFunc func(done, total int)

// Checker defines the interface for file health checking
type Checker interface {
	// CheckFiles returns one error per file that is not fully retrievable.
	// progress may be nil.
	CheckFiles(nzbData *nzbparser.NzbData, progress ProgressFunc) []error
	// PlannedProbes is the work a check of this nzb would be, in segments it
	// would ask the server about, without asking about any of them
	PlannedProbes(nzbData *nzbparser.NzbData) int
}
