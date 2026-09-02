package filehealth

import "git.ruekov.eu/ruakij/nzbStreamer/pkg/nzbparser"

// Checker defines the interface for file health checking
type Checker interface {
	// CheckFiles returns one error per file that is not fully retrievable
	CheckFiles(nzbData *nzbparser.NzbData) []error
}
