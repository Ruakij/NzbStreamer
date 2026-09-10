// Package nzbstore keeps the nzbs the service knows about and the outcome of
// adding each one.
package nzbstore

import (
	"errors"
	"time"

	"git.ruekov.eu/ruakij/nzbStreamer/pkg/nzbparser"
)

// Record is one nzb as the store keeps it: the nzb itself, and how its add went.
// The two are one record because deleting a download and forgetting it are the
// same act - files nothing can report on, or a report on files that are gone,
// are both states nobody can act on.
//
// The stage is a plain string here, since what its values mean belongs to the
// service that sets them.
type Record struct {
	Data *nzbparser.NzbData
	// Category is what the client api that added it called it; empty for the
	// watch folder, which has no notion of one
	Category   string
	Stage      string
	Err        string
	AddedAt    time.Time
	FinishedAt time.Time
	// TreeKey identifies the settings the stored files were built with; the
	// files are only worth reading back while it still matches
	TreeKey string
	// Archived marks a record a client took out of its history. It changes
	// nothing about the nzb: the files stay presented and the record stays
	// here, only the default history listing leaves it out
	Archived bool
}

// File is one path an nzb presents, as it was presented when the add finished.
// Exact separates a measured size from a hint, which is the same distinction the
// live stack makes: a size may be a guess, the bytes never are.
type File struct {
	Path  string
	Size  int64
	Exact bool
	// Source is the nzb's own filename the presented path was built from, the
	// volume-set member or raw content file a verdict on the source file
	// reaches this path through
	Source string
}

// SourceFile is one of the nzb's own files, the input to a check.
type SourceFile struct {
	Filename string
	PostedAt time.Time
}

// SegmentVerdict is what is known about one segment of one source file. Sparse:
// absent entries mean unknown. Size -1 means not measured.
type SegmentVerdict struct {
	Filename  string
	Index     int
	MessageID string
	Size      int64
	Present   bool
	CheckedAt time.Time
	FetchedAt time.Time
	Fetches   int
}

// ProbeResult is one probe/fetch outcome to persist. Server "" writes no
// segment_missing row, which is the shape of an answer that cannot name the
// server that gave it.
type ProbeResult struct {
	Filename  string
	Index     int
	MessageID string
	Present   bool
	Server    string
}

// ErrNotFound reports a name no record is kept under.
var ErrNotFound = errors.New("nzb not found")

type NzbStore interface {
	List() ([]Record, error)
	// Raw reads back the nzb as it was submitted, for any record the store
	// holds - a failed or archived one included
	Raw(name string) ([]byte, error)
	// Add records an accepted nzb, before anything is built from it, and
	// supersedes an earlier record of the same name
	Add(data *nzbparser.NzbData, stage, category string) error
	// SetStage records how the add ended
	SetStage(name, stage, errMessage string) error
	// SetArchived takes a record out of the default history listing, or puts it
	// back
	SetArchived(name string, archived bool) error
	// SetFiles replaces what an nzb presents, under the key the tree was built
	// with
	SetFiles(name, treeKey string, files []File) error
	// Files reads back what SetFiles recorded
	Files(name string) ([]File, error)
	// RemoveFiles deletes the given presented paths, the inverse of registering
	// a subset of the tree
	RemoveFiles(name string, paths []string) error
	// EnsureSourceFiles records the nzb's own files a health verdict attaches
	// to. Idempotent: an upsert that leaves an existing row's retry_after and
	// posted_at alone
	EnsureSourceFiles(nzbName string, files []SourceFile) error
	// SetRetryAfter records that a file's posts are younger than the minimum
	// age a miss counts as final from, and when it is worth asking again
	SetRetryAfter(nzbName, filename string, after time.Time) error
	// SegmentVerdicts answers, per filename, what is known about the segments
	// of that source file. Only rows that exist come back; a segment nothing
	// has touched has no verdict
	SegmentVerdicts(nzbName string, filenames []string) (map[string]map[int]SegmentVerdict, error)
	// RecordProbes persists probe and fetch outcomes against the source files
	// of one nzb
	RecordProbes(nzbName string, probes []ProbeResult) error
	Delete(name string) error
}
