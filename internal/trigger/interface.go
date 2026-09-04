// Package trigger defines the hook by which an nzb source reports files
// arriving and leaving.
package trigger

import "git.ruekov.eu/ruakij/nzbStreamer/pkg/nzbparser"

type Trigger interface {
	AddListener(addHook, removeHook func(nzbData *nzbparser.NzbData) error) (listenerID int, err error)
	RemoveListener(listenerID int) error
}
