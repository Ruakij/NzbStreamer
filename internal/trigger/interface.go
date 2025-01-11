package trigger

import "git.ruekov.eu/ruakij/nzbStreamer/pkg/nzbParser"

type Trigger interface {
	AddListener(addHook, removeHook func(nzbData *nzbParser.NzbData) error) (listenerId int, err error)
	RemoveListener(listenerId int) (err error)
}
