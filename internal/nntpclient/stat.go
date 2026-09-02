package nntpclient

import (
	"errors"
	"fmt"

	"astuart.co/nntp"
)

const (
	articleExists   = 223
	noArticleWithID = 430
)

var ErrUnexpectedStatResponse = errors.New("unexpected STAT response")

// SegmentExists reports whether an article is present on the server without
// transferring its body. STAT addressed by message-id needs no group selection
// (RFC 3977 6.2.4), so any pooled connection can serve it.
func SegmentExists(client *nntp.Client, id string) (bool, error) {
	res, err := client.Do("STAT <%s>", id)
	if err != nil {
		return false, fmt.Errorf("failed stat for '%s': %w", id, err)
	}

	switch res.Code {
	case articleExists:
		return true, nil
	case noArticleWithID:
		return false, nil
	default:
		return false, fmt.Errorf("%w for '%s': %d %s", ErrUnexpectedStatResponse, id, res.Code, res.Message)
	}
}
