package resource

import (
	"errors"
	"io"
)

var (
	ErrInvalidSeek = errors.New("invalid seek position")
)

// Resource is an interface to excapsulate Open and Size actions from data-resources
//
// Specific implementations may document their own management behavior.
type ReadableResource interface {
	Open() (io.Reader, error)
	Size() (int64, error)
}

type ReadCloseableResource interface {
	Open() (io.ReadCloser, error)
	Size() (int64, error)
}

type ReadSeekCloseableResource interface {
	Open() (io.ReadSeekCloser, error)
	Size() (int64, error)
}
