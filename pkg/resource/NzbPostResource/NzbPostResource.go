package NzbPostResource

import (
	"bytes"
	"io"
	"sync"

	"astuart.co/nntp"
	"github.com/chrisfarms/yenc"
)

// NzbPostResource allows reading the post-content from a Newsserver
type NzbPostResource struct {
	Id         string
	Group      string
	Encoding   string
	SizeHint   int64
	NntpClient *nntp.Client
}

type NzbPostResourceReader struct {
	resource   *NzbPostResource
	dataReader io.Reader
	index      int
	fetchOnce  sync.Once
}

func (m *NzbPostResource) Open() (io.ReadCloser, error) {
	return &NzbPostResourceReader{
		resource: m,
		index:    0,
	}, nil
}

func (r *NzbPostResourceReader) Close() (err error) {
	r.dataReader = nil
	return
}

func (r *NzbPostResource) Size() (int64, error) {
	return r.SizeHint, nil
}

func (r *NzbPostResourceReader) Read(p []byte) (n int, err error) {
	if len(p) == 0 {
		return
	}

	if r.dataReader == nil {
		err = r.loadPostFromServer()
		if err != nil {
			return
		}
	}

	n, err = r.dataReader.Read(p)

	r.index += n

	return n, err
}

func (r *NzbPostResourceReader) loadPostFromServer() (err error) {
	res, err := r.resource.NntpClient.GetArticle(r.resource.Group, r.resource.Id)
	if err != nil {
		return
	}
	defer res.Body.Close()

	part, err := yenc.Decode(res.Body)
	if err != nil {
		return
	}

	// Plausability checks
	if part.Size != r.resource.SizeHint {
		r.resource.SizeHint = part.Size
	}

	r.dataReader = bytes.NewReader(part.Body)

	return
}
