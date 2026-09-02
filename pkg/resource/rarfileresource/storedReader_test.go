package rarfileresource

import (
	"bytes"
	"errors"
	"io"
	"testing"
)

var errShortFile = errors.New("short file")

// handedOutLimiter reproduces how rardecode bounds a member: it counts the bytes
// it has handed out, not where it sits, and a seek leaves that count behind. A
// read that reaches the end of the data before the count reaches the members
// length reports a short file, and one whose count has already reached it
// reports the end early. storedReader exists to keep both out of reach, so the
// model is what the test is against - there is no rar fixture in the repo.
type handedOutLimiter struct {
	content []byte
	size    int64
	at      int64
	handed  int64
}

func (l *handedOutLimiter) Read(p []byte) (int, error) {
	left := l.size - l.handed
	if left <= 0 {
		return 0, io.EOF
	}
	if int64(len(p)) > left {
		p = p[:left]
	}

	if l.at >= int64(len(l.content)) {
		if l.handed < l.size {
			return 0, errShortFile
		}
		return 0, io.EOF
	}

	n := copy(p, l.content[l.at:])
	l.at += int64(n)
	l.handed += int64(n)

	return n, nil
}

func (l *handedOutLimiter) Seek(offset int64, whence int) (int64, error) {
	if whence != io.SeekStart {
		return 0, errors.New("only SeekStart is modelled")
	}
	l.at = offset

	return offset, nil
}

func (l *handedOutLimiter) Close() error { return nil }

func newStoredReaderOver(content []byte, opened *int) *storedReader {
	reopen := func() (io.ReadSeekCloser, error) {
		*opened++
		return &handedOutLimiter{content: content, size: int64(len(content))}, nil
	}

	reader, _ := reopen()

	return &storedReader{reopen: reopen, reader: reader, size: int64(len(content))}
}

// A player reading the tail of a member skips there first, which leaves the byte
// count behind the position.
func TestStoredReaderTailAfterForwardSeek(t *testing.T) {
	t.Parallel()

	content := []byte("HelloWorldAndGoodbye")
	opened := 0
	reader := newStoredReaderOver(content, &opened)

	if _, err := reader.Seek(int64(len(content))-5, io.SeekStart); err != nil {
		t.Fatalf("Seek = %v, want no error", err)
	}

	got, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("ReadAll after forward seek = %v, want no error", err)
	}
	if want := content[len(content)-5:]; !bytes.Equal(got, want) {
		t.Errorf("ReadAll after forward seek = %q, want %q", got, want)
	}
}

// Seeking back is what a slider does, and the member has to serve it rather than
// report an end that is not there.
func TestStoredReaderReadAfterBackwardSeek(t *testing.T) {
	t.Parallel()

	content := []byte("HelloWorldAndGoodbye")
	opened := 0
	reader := newStoredReaderOver(content, &opened)

	if _, err := io.ReadAll(reader); err != nil {
		t.Fatalf("ReadAll = %v, want no error", err)
	}
	if _, err := reader.Seek(0, io.SeekStart); err != nil {
		t.Fatalf("Seek = %v, want no error", err)
	}

	got, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("ReadAll after backward seek = %v, want no error", err)
	}
	if !bytes.Equal(got, content) {
		t.Errorf("ReadAll after backward seek = %q, want %q", got, content)
	}
	if opened != 2 {
		t.Errorf("opened %d members, want 2 - one to seek back on", opened)
	}
}

// Seeking to the end is how a caller asks for a length, and it must not touch a
// volume to answer.
func TestStoredReaderSeekEndOpensNothing(t *testing.T) {
	t.Parallel()

	content := []byte("HelloWorldAndGoodbye")
	opened := 0
	reader := newStoredReaderOver(content, &opened)

	size, err := reader.Seek(0, io.SeekEnd)
	if err != nil {
		t.Fatalf("Seek(0, io.SeekEnd) = %v, want no error", err)
	}
	if size != int64(len(content)) {
		t.Errorf("Seek(0, io.SeekEnd) = %d, want %d", size, len(content))
	}
	if opened != 1 {
		t.Errorf("opened %d members, want 1 - the one it started with", opened)
	}
}
