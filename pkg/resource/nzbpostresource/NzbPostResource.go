package nzbpostresource

import (
	"bytes"
	"fmt"
	"io"

	"astuart.co/nntp"
	"github.com/chrisfarms/yenc"
)

// NzbPostResource allows reading the post-content from a Newsserver
type NzbPostResource struct {
	ID            string
	Group         string
	Encoding      string
	SizeHint      int64
	SizeHintExact bool
	NntpClient    *nntp.Client
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
	res, err := r.resource.NntpClient.GetArticle(r.resource.Group, r.resource.ID)
	if err != nil {
		return fmt.Errorf("failed getting article: %w", err)
	}
	defer res.Body.Close()

	part, err := yenc.Decode(res.Body)
	if err != nil {
		return fmt.Errorf("failed yenc-decoding body: %w", err)
	}

	// Update size if differs
	if part.Size != r.resource.SizeHint {
		r.resource.SizeHint = part.Size
		r.resource.SizeHintExact = true
	}

	r.dataReader = bytes.NewReader(part.Body)

	return nil
}
