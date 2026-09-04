package fusemount

import (
	"bytes"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"git.ruekov.eu/ruakij/nzbStreamer/internal/logging"
	"git.ruekov.eu/ruakij/nzbStreamer/pkg/readeratwrapper"
)

const missSize = int64(8 * 1024 * 1024)

// readSeekCloser stands strings.Reader in for a streaming Close source.
type readSeekCloser struct {
	*strings.Reader
}

func (readSeekCloser) Close() error { return nil }

// recordingSeekCloser counts reaches into the underlying stream.
type recordingSeekCloser struct {
	io.ReadSeekCloser
	seeks  int
	closed int
}

func (r *recordingSeekCloser) Seek(offset int64, whence int) (int64, error) {
	r.seeks++
	return r.ReadSeekCloser.Seek(offset, whence)
}

func (r *recordingSeekCloser) Close() error {
	r.closed++
	return r.ReadSeekCloser.Close()
}

// erroringReader fails every Read so cursor invalidation can be exercised.
type erroringReader struct {
	io.ReadSeekCloser
}

func (erroringReader) Read(p []byte) (int, error) { return 0, errors.New("boom") }

// captureSlog points slog at a buffer for assertion.
func captureSlog(t *testing.T) (*bytes.Buffer, func()) {
	t.Helper()
	var out bytes.Buffer
	orig := slog.Default()
	slog.SetDefault(slog.New(logging.New(&out, slog.LevelDebug)))
	return &out, func() { slog.SetDefault(orig) }
}

func TestSeekLoggingReadSeekCloserLogsSeekOrdering(t *testing.T) {
	buf, restore := captureSlog(t)
	defer restore()

	s := &seekLoggingReadSeekCloser{handle: 7, name: "file.bin", r: readSeekCloser{strings.NewReader("0123456789")}}
	if _, err := s.Seek(3, io.SeekStart); err != nil {
		t.Fatal(err)
	}

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	var seekStart, seekDone string
	for _, ln := range lines {
		switch {
		case strings.Contains(ln, "Seek start"):
			seekStart = ln
		case strings.Contains(ln, "Seek done"):
			seekDone = ln
		}
	}
	if seekStart == "" || seekDone == "" {
		t.Fatalf("expected Seek start and done lines, got %q", lines)
	}
	for _, want := range []string{"handle=7", "name=file.bin", "offset=3"} {
		if !strings.Contains(seekStart, want) || !strings.Contains(seekDone, want) {
			t.Errorf("line missing %s: start=%q done=%q", want, seekStart, seekDone)
		}
	}
	if strings.Index(buf.String(), "Seek start") > strings.Index(buf.String(), "Seek done") {
		t.Error("Seek start must precede Seek done")
	}
}

// A backward seek within narrowMissLimit is a reorder worth a warning.
func TestSeekLoggingReadSeekCloserWarnsOnNarrowBackwardMiss(t *testing.T) {
	buf, restore := captureSlog(t)
	defer restore()

	s := &seekLoggingReadSeekCloser{handle: 7, name: "file.bin", r: readSeekCloser{strings.NewReader("0123456789")}, narrowMissSize: missSize}
	if _, err := s.Seek(4, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}

	line := buf.String()
	for _, want := range []string{"WARN [fusemount] Read narrowly missed ordering", "offset=0", "cursor=4", "missed_by=4"} {
		if !strings.Contains(line, want) {
			t.Errorf("log missing %q: %s", want, line)
		}
	}
}

// A backward step beyond narrowMissLimit, or any forward step, never warns.
func TestSeekLoggingReadSeekCloserDoesNotWarnOnLargeBackwardOrForward(t *testing.T) {
	buf, restore := captureSlog(t)
	defer restore()

	s := &seekLoggingReadSeekCloser{handle: 7, name: "file.bin", r: readSeekCloser{strings.NewReader("0123456789")}, narrowMissSize: missSize}
	if _, err := s.Seek(missSize+1, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Seek(0, io.SeekStart); err != nil { // backward, but > the warned size
		t.Fatal(err)
	}
	if _, err := s.Seek(2, io.SeekStart); err != nil { // forward
		t.Fatal(err)
	}

	if strings.Contains(buf.String(), "Read narrowly missed ordering") {
		t.Error("unexpected ordering warning")
	}
}

// Read advances the cursor, so a sequential seek onto it is no miss.
func TestSeekLoggingReadSeekCloserReadAdvancesCursor(t *testing.T) {
	buf, restore := captureSlog(t)
	defer restore()

	s := &seekLoggingReadSeekCloser{handle: 7, name: "file.bin", r: readSeekCloser{strings.NewReader("0123456789")}}
	if _, err := s.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	if n, err := s.Read(make([]byte, 4)); err != nil || n != 4 {
		t.Fatalf("Read: n=%d err=%v", n, err)
	}
	if s.pos != 4 || !s.posSet {
		t.Fatalf("pos=%d posSet=%v, want 4 true", s.pos, s.posSet)
	}
	if _, err := s.Seek(4, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(buf.String(), "Read narrowly missed ordering") {
		t.Error("sequential seek warned as a miss")
	}
}

// A read failure invalidates the cursor, so no false miss warning.
func TestSeekLoggingReadSeekCloserReadErrorInvalidatesCursor(t *testing.T) {
	buf, restore := captureSlog(t)
	defer restore()

	s := &seekLoggingReadSeekCloser{handle: 7, name: "file.bin", r: erroringReader{readSeekCloser{strings.NewReader("0123456789")}}}
	if _, err := s.Seek(4, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Read(make([]byte, 4)); err == nil {
		t.Fatal("expected read error")
	}
	if s.posSet {
		t.Error("posSet not invalidated by read error")
	}
	if _, err := s.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(buf.String(), "Read narrowly missed ordering") {
		t.Error("errored cursor warned as a miss")
	}
}

// Close forwards; the once-only guarantee lives in the batching adapter.
func TestSeekLoggingReadSeekCloserCloseForwards(t *testing.T) {
	rec := &recordingSeekCloser{ReadSeekCloser: readSeekCloser{strings.NewReader("abc")}}
	s := &seekLoggingReadSeekCloser{handle: 1, name: "x", r: rec}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if rec.closed != 1 {
		t.Fatalf("underlying closed %d times, want 1", rec.closed)
	}
}

// Composed with the batching adapter: out-of-order reads, EOF, close-once.
func TestSeekLoggingReadSeekCloserComposesWithBatchingAdapter(t *testing.T) {
	rec := &recordingSeekCloser{ReadSeekCloser: readSeekCloser{strings.NewReader("0123456789")}}
	logged := &seekLoggingReadSeekCloser{handle: 7, name: "file.bin", r: rec}
	b := readeratwrapper.NewReadSeekerBatchedAt(logged, time.Millisecond)

	for _, tc := range []struct {
		off  int64
		want string
	}{
		{0, "0123"},
		{5, "5678"},
		{2, "2345"}, // backward, exercised through the batch
		{7, "789"},  // short read at the end
	} {
		buf := make([]byte, 4)
		n, err := b.ReadAt(buf, tc.off)
		if err != nil && !errors.Is(err, io.EOF) {
			t.Fatalf("ReadAt(%d): %v", tc.off, err)
		}
		if string(buf[:n]) != tc.want {
			t.Fatalf("ReadAt(%d)=%q, want %q", tc.off, buf[:n], tc.want)
		}
	}

	// beyond the end reads as zero bytes with EOF
	if n, err := b.ReadAt(make([]byte, 4), 100); !errors.Is(err, io.EOF) || n != 0 {
		t.Fatalf("ReadAt(100): n=%d err=%v, want 0 io.EOF", n, err)
	}

	if err := b.Close(); err != nil {
		t.Fatal(err)
	}
	if rec.closed != 1 {
		t.Fatalf("underlying closed %d times, want 1", rec.closed)
	}
}

// The in-order fast path reaches the stream without a second seek.
func TestSeekLoggingReadSeekCloserCompositionSequentialReadSkipsSeek(t *testing.T) {
	rec := &recordingSeekCloser{ReadSeekCloser: readSeekCloser{strings.NewReader("0123456789abcdef")}}
	logged := &seekLoggingReadSeekCloser{handle: 7, name: "file.bin", r: rec}
	b := readeratwrapper.NewReadSeekerBatchedAt(logged, time.Millisecond)
	defer b.Close()

	if n, err := b.ReadAt(make([]byte, 4), 0); err != nil || n != 4 {
		t.Fatalf("first ReadAt: n=%d err=%v", n, err)
	}
	seeks := rec.seeks
	if n, err := b.ReadAt(make([]byte, 4), 4); err != nil || n != 4 {
		t.Fatalf("sequential ReadAt: n=%d err=%v", n, err)
	}
	if rec.seeks != seeks {
		t.Fatalf("sequential read sought: %d seeks, want %d", rec.seeks, seeks)
	}
}
