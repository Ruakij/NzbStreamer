// Package presentation is the seam between the service and its filesystem
// views: an openable handle for one file, and the presenter that mounts it.
package presentation

import (
	"io"
	"time"
)

type Openable interface {
	Open() (io.ReadSeekCloser, error)
	SizeHint() (int64, error)
}

type Presenter interface {
	AddFile(fullpath string, modTime time.Time, openable Openable) error
	RemoveFile(fullpath string) error
}
