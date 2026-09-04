package readaheadresource_test

import (
	"errors"
	"io"
	"sync/atomic"
	"testing"
	"time"

	"git.ruekov.eu/ruakij/nzbStreamer/pkg/resource/bytesresource"
	"git.ruekov.eu/ruakij/nzbStreamer/pkg/resource/readaheadresource"
)

func TestReadAndSeek(t *testing.T) {
	reader, err := readaheadresource.New(&bytesresource.BytesResource{Content: []byte("0123456789")}, 4, 3).Open()
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()

	buf := make([]byte, 5)
	if _, err := io.ReadFull(reader, buf); err != nil || string(buf) != "01234" {
		t.Fatalf("first read = %q, %v", buf, err)
	}
	if _, err := reader.Seek(2, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadFull(reader, buf); err != nil || string(buf) != "23456" {
		t.Fatalf("read after seek = %q, %v", buf, err)
	}
	if _, err := io.ReadAll(reader); err != nil && !errors.Is(err, io.EOF) {
		t.Fatal(err)
	}
}

type slowReader struct {
	data   []byte
	active atomic.Int64
	peak   atomic.Int64
}

func (r *slowReader) Open() (io.ReadSeekCloser, error) { return &slowHandle{resource: r}, nil }
func (r *slowReader) SizeHint() (int64, error)         { return int64(len(r.data)), nil }

type slowHandle struct {
	resource *slowReader
	position int64
}

func (r *slowHandle) Read(p []byte) (int, error) {
	n, err := r.ReadAt(p, r.position)
	r.position += int64(n)
	return n, err
}
func (r *slowHandle) ReadAt(p []byte, off int64) (int, error) {
	active := r.resource.active.Add(1)
	defer r.resource.active.Add(-1)
	for {
		peak := r.resource.peak.Load()
		if active <= peak || r.resource.peak.CompareAndSwap(peak, active) {
			break
		}
	}
	time.Sleep(time.Millisecond)
	if off >= int64(len(r.resource.data)) {
		return 0, io.EOF
	}
	n := copy(p, r.resource.data[off:])
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}
func (r *slowHandle) Seek(offset int64, whence int) (int64, error) {
	switch whence {
	case io.SeekStart:
		r.position = offset
	case io.SeekCurrent:
		r.position += offset
	case io.SeekEnd:
		r.position = int64(len(r.resource.data)) + offset
	}
	return r.position, nil
}
func (r *slowHandle) Close() error { return nil }

func TestReadsAheadInParallel(t *testing.T) {
	underlying := &slowReader{data: make([]byte, 32)}
	reader, err := readaheadresource.New(underlying, 16, 4).Open()
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	if _, err := io.ReadFull(reader, make([]byte, 16)); err != nil {
		t.Fatal(err)
	}
	if underlying.peak.Load() != 4 {
		t.Fatalf("peak reads = %d, want 4", underlying.peak.Load())
	}
}
