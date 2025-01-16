package trigger

import "git.ruekov.eu/ruakij/nzbStreamer/pkg/nzbparser"

type Trigger interface {
	AddListener(addHook, removeHook func(nzbData *nzbparser.NzbData) error) (listenerID int, err error)
	RemoveListener(listenerID int) error
}
