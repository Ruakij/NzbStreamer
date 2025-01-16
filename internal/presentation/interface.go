package presentation

import (
	"io"
	"time"
)

type Openable interface {
	Open() (io.ReadSeekCloser, error)
	Size() (int64, error)
}

type Presenter interface {
	AddFile(fullpath string, modTime time.Time, openable Openable) error
	RemoveFile(fullpath string) error
}
