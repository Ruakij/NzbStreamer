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
}

type NzbStore interface {
	List() ([]Record, error)
	// Add records an accepted nzb, before anything is built from it, and
	// supersedes an earlier record of the same name
	Add(data *nzbparser.NzbData, stage, category string) error
	// SetStage records how the add ended
	SetStage(name, stage, errMessage string) error
	Delete(name string) error
}
