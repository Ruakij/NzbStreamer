package filehealth

import (
	"context"

	"git.ruekov.eu/ruakij/nzbStreamer/pkg/nzbparser"
)

// ProgressFunc reports segments probed against segments meant to be probed.
type ProgressFunc func(done, total int)

// VerdictReport receives the outcome of every probe: the nzb file the segment
// belongs to, its position in the group's flattened index space (the space
// Scan's known counts in), and whether it is present. A missing segment is
// reported as well as a present one, so a caller can persist both; a probe
// that errored is reported to nobody, because it is no verdict. Calls are
// serialized. nil is a caller that does not want verdicts.
type VerdictReport func(file *nzbparser.File, index int, present bool)

// FailedGroup is one verdict: a group that lost a segment. Files are the nzb's
// own filenames the group is made of, which is what a caller drops from the
// presented tree.
type FailedGroup struct {
	Name  string
	Files []string
}

// ContentGroup is what a verdict attaches to: an archive's volumes together,
// and every other content file on its own. Offsets are where each file starts
// in the group's flattened segment index, which is the index space Scan's
// known counts in and a reported verdict position is read back from.
type ContentGroup struct {
	// The grouped filename, as filenameops.GroupPartFilenames names it
	Name string
	// The nzb's own files the group is made of, in flattened index order
	Files []*nzbparser.File
	// Offsets[i] is where Files[i] starts in the flattened index
	Offsets []int
	// Segments across all files, which is the population N
	Segments int
}

type Checker interface {
	// CheckFiles scans every content group to the add-time confidence and
	// returns the groups that lost a segment. report and progress may be nil.
	// An AddConfidence of 0 disables checking. A cancelled ctx stops it
	// probing.
	CheckFiles(ctx context.Context, nzbData *nzbparser.NzbData, report VerdictReport, progress ProgressFunc) []FailedGroup
	// Scan probes every content group to the given confidence and returns the
	// groups that lost a segment. confidence <= 0 scans nothing. known holds
	// the flattened positions that already count as present per group name, so
	// a rescan re-asks only what is unknown; every probe outcome is handed to
	// report, which may be nil.
	Scan(ctx context.Context, nzbData *nzbparser.NzbData, confidence float64, known map[string][]int, report VerdictReport, progress ProgressFunc) []FailedGroup
	// PlannedProbes is the work a check of this nzb would be, in segments it
	// would ask the server about, without asking about any of them.
	PlannedProbes(nzbData *nzbparser.NzbData) int
}
