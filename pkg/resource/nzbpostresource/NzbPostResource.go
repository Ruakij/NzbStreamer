package nzbpostresource

import (
	"bytes"
	"fmt"
	"io"
)

// GetSegmentFunc returns the decoded content of one segment. Fetching, decoding
// and retrying all live behind it, so this package needs no news server to test.
type GetSegmentFunc func(group, id string) ([]byte, error)

// NzbPostResource allows reading the post-content from a Newsserver
type NzbPostResource struct {
	ID            string
	Group         string
	SizeHint      int64
	SizeHintExact bool
	GetSegment    GetSegmentFunc
}

type NzbPostResourceReader struct {
	resource   *NzbPostResource
	dataReader io.Reader
	index      int
}

func (r *NzbPostResource) Open() (io.ReadCloser, error) {
	return &NzbPostResourceReader{
		resource: r,
		index:    0,
	}, nil
}

func (r *NzbPostResource) Size() (int64, error) {
	return r.SizeHint, nil
}

func (r *NzbPostResource) IsSizeAccurate() bool {
	return r.SizeHintExact
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
	if size := int64(len(body)); size != r.resource.SizeHint {
		r.resource.SizeHint = size
		r.resource.SizeHintExact = true
	}

	r.dataReader = bytes.NewReader(body)

	return nil
}
