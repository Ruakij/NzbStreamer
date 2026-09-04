// Package resource defines the read, size and close interfaces every file
// object in the streamer implements.
package resource

import (
	"errors"
	"io"
)

var (
	ErrInvalidSeek = errors.New("invalid seek position")
	// ErrSizeNotExact is returned by Size when the exact length is not known
	// yet. It is not a failure: SizeHint still answers, and reading the
	// resource is what turns the estimate into a fact.
	ErrSizeNotExact = errors.New("exact size not known")
)

// SizeHinter reports a length that may be an estimate. It always answers and
// never costs a read, which makes it the right thing to plan with and the wrong
// thing to compute a final offset from.
//
// Every resource can hint, so the resource interfaces embed this one.
type SizeHinter interface {
	SizeHint() (int64, error)
}

// Sized reports an exact length. A resource that cannot always know one returns
// ErrSizeNotExact rather than a guess, so a caller holding a Sized still has to
// handle not being told - what the interface guarantees is that any number it
// does return is a fact.
//
// Type-assert for this before opening anything: an exact size answered here
// costs no reader, no descriptor and no download, where Seek(0, io.SeekEnd) on
// an opened reader costs all three.
type Sized interface {
	Size() (int64, error)
}

// Resource is an interface to excapsulate Open and Size actions from data-resources
//
// Specific implementations may document their own management behavior.
type ReadableResource interface {
	Open() (io.Reader, error)
	SizeHinter
}

type ReadCloseableResource interface {
	Open() (io.ReadCloser, error)
	SizeHinter
}

type ReadSeekCloseableResource interface {
	Open() (io.ReadSeekCloser, error)
	SizeHinter
}
