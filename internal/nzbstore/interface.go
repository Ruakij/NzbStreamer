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
	Delete(name string) error
}
