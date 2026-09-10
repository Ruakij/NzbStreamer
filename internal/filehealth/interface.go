package filehealth

import (
	"context"

	"git.ruekov.eu/ruakij/nzbStreamer/pkg/nzbparser"
)

// ProgressFunc reports segments probed against segments meant to be probed.
type ProgressFunc func(done, total int)

// FailedGroup is one verdict: a group that lost a segment. Files are the nzb's
// own filenames the group is made of, which is what a caller drops from the
// presented tree.
type FailedGroup struct {
	Name  string
	Files []string
}

type Checker interface {
	// CheckFiles scans every content group to the add-time confidence and
	// returns the groups that lost a segment. progress may be nil. An
	// AddConfidence of 0 disables checking. A cancelled ctx stops it probing.
	CheckFiles(ctx context.Context, nzbData *nzbparser.NzbData, progress ProgressFunc) []FailedGroup
	// PlannedProbes is the work a check of this nzb would be, in segments it
	// would ask the server about, without asking about any of them.
	PlannedProbes(nzbData *nzbparser.NzbData) int
}
