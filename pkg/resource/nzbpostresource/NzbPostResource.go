package nzbpostresource

import (
	"bytes"
	"fmt"
	"io"
	"sync/atomic"

	"git.ruekov.eu/ruakij/nzbStreamer/pkg/resource"
)

// GetSegmentFunc returns the decoded content of one segment. Fetching, decoding
// and retrying all live behind it, so this package needs no news server to test.
type GetSegmentFunc func(group, id string) ([]byte, error)

// NzbPostResource allows reading the post-content from a Newsserver
type NzbPostResource struct {
	ID         string
	Group      string
	GetSegment GetSegmentFunc

	// length is the nzb hint until the segment has been decoded once, which is
	// what lengthExact reports. Every reader of the segment settles them, so
	// they are read and written concurrently.
	length      atomic.Int64
	lengthExact atomic.Bool
}

type NzbPostResourceReader struct {
	resource   *NzbPostResource
	dataReader io.Reader
	index      int
}

func New(id, group string, lengthHint int64, lengthExact bool, getSegment GetSegmentFunc) *NzbPostResource {
	r := &NzbPostResource{
		ID:         id,
		Group:      group,
		GetSegment: getSegment,
	}
	r.length.Store(lengthHint)
	r.lengthExact.Store(lengthExact)

	return r
}

func (r *NzbPostResource) Open() (io.ReadCloser, error) {
	return &NzbPostResourceReader{
		resource: r,
		index:    0,
	}, nil
}

func (r *NzbPostResource) SizeHint() (int64, error) {
	return r.length.Load(), nil
}

func (r *NzbPostResource) Size() (int64, error) {
	if !r.lengthExact.Load() {
		return 0, resource.ErrSizeNotExact
	}

	return r.length.Load(), nil
}

func (r *NzbPostResourceReader) Close() error {
	r.dataReader = nil
	return nil
}

func (r *NzbPostResourceReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}

	if r.dataReader == nil {
		err := r.loadPostFromServer()
		if err != nil {
			return 0, err
		}
	}

	n, err := r.dataReader.Read(p)
	r.index += n

	return n, err
}

func (r *NzbPostResourceReader) loadPostFromServer() error {
	body, err := r.resource.GetSegment(r.resource.Group, r.resource.ID)
	if err != nil {
		return fmt.Errorf("failed getting segment '%s': %w", r.resource.ID, err)
	}

	// The nzb only hinted at the size; now it is known
	r.resource.length.Store(int64(len(body)))
	r.resource.lengthExact.Store(true)

	r.dataReader = bytes.NewReader(body)

	return nil
}
