package rarfileresource

import (
	"fmt"
	"io"

	"git.ruekov.eu/ruakij/nzbStreamer/pkg/resource"
)

// storedReader is one path into a stored member, holding the position itself.
//
// rardecode bounds a member by counting the bytes it has handed out rather than
// by where the reader sits, and a seek leaves that count where it was. A read
// that then runs past the end of the member reports a short file, and one after
// a backwards seek reports the end of the member early, so the count has to stay
// at or behind the position: reopening the member restarts it, which costs a
// volume open and no download.
//
// Seeking is arithmetic here and the underlying reader is placed only when a
// read needs it, so seeking to the end to learn a length touches no volume.
type storedReader struct {
	reopen func() (io.ReadSeekCloser, error)
	reader io.ReadSeekCloser
	size   int64
	// Where the caller is, where the underlying reader is, and how many bytes it
	// has been asked to hand out
	position int64
	at       int64
	handed   int64
}

func newStoredReader(res *RarFileResource, reader io.ReadSeekCloser, size int64) *storedReader {
	return &storedReader{
		reopen: func() (io.ReadSeekCloser, error) {
			reader, _, err := res.openStoredMember()
			return reader, err
		},
		reader: reader,
		size:   size,
	}
}

func (s *storedReader) Read(p []byte) (int, error) {
	if s.position >= s.size {
		return 0, io.EOF
	}
	if int64(len(p)) > s.size-s.position {
		p = p[:s.size-s.position]
	}

	if err := s.sync(); err != nil {
		return 0, err
	}

	n, err := s.reader.Read(p)
	s.position += int64(n)
	s.at += int64(n)
	s.handed += int64(n)

	//nolint:wrapcheck // io.EOF has to reach the caller unwrapped
	return n, err
}

// sync places the underlying reader where the caller is, on a freshly opened
// member when its byte count has passed that point.
func (s *storedReader) sync() error {
	if s.handed > s.position {
		reader, err := s.reopen()
		if err != nil {
			return err
		}

		//nolint:errcheck // Nothing to do with a failure of a reader we are replacing
		s.reader.Close()
		s.reader, s.at, s.handed = reader, 0, 0
	}

	if s.at != s.position {
		if _, err := s.reader.Seek(s.position, io.SeekStart); err != nil {
			return fmt.Errorf("failed seeking rar member to %d: %w", s.position, err)
		}
		s.at = s.position
	}

	return nil
}

func (s *storedReader) Seek(offset int64, whence int) (int64, error) {
	var position int64

	switch whence {
	case io.SeekStart:
		position = offset
	case io.SeekCurrent:
		position = s.position + offset
	case io.SeekEnd:
		position = s.size + offset
	default:
		return 0, resource.ErrInvalidSeek
	}

	if position < 0 || position > s.size {
		return 0, resource.ErrInvalidSeek
	}

	s.position = position

	return s.position, nil
}

func (s *storedReader) Close() error {
	if err := s.reader.Close(); err != nil {
		return fmt.Errorf("failed closing rar member: %w", err)
	}

	return nil
}
