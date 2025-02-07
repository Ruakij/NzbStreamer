package filehealth

import "git.ruekov.eu/ruakij/nzbStreamer/internal/presentation"

// Checker defines the interface for file health checking
type Checker interface {
	CheckFiles(files map[string]presentation.Openable) []error
}
