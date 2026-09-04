// Package nzbstore keeps the nzbs the service knows about and the outcome of
// adding each one.
package nzbstore

import (
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
}

// File is one path an nzb presents, as it was presented when the add finished.
// Exact separates a measured size from a hint, which is the same distinction the
// live stack makes: a size may be a guess, the bytes never are.
type File struct {
	Path  string
	Size  int64
	Exact bool
}

type NzbStore interface {
	List() ([]Record, error)
	// Add records an accepted nzb, before anything is built from it, and
	// supersedes an earlier record of the same name
	Add(data *nzbparser.NzbData, stage, category string) error
	// SetStage records how the add ended
	SetStage(name, stage, errMessage string) error
	// SetFiles replaces what an nzb presents, under the key the tree was built
	// with
	SetFiles(name, treeKey string, files []File) error
	// Files reads back what SetFiles recorded
	Files(name string) ([]File, error)
	Delete(name string) error
}
