package adaptiveparallelmergerresource_test

import (
	"bytes"
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"git.ruekov.eu/ruakij/nzbStreamer/pkg/resource"
	"git.ruekov.eu/ruakij/nzbStreamer/pkg/resource/adaptiveparallelmergerresource"
	"git.ruekov.eu/ruakij/nzbStreamer/pkg/resource/bytesresource"
)

const content = "HelloWorldAndGoodbye"

// parts splits content the way segments split a file, in uneven pieces so a read
// crossing a boundary is the normal case rather than the exception.
func parts() []resource.ReadSeekCloseableResource {
	return []resource.ReadSeekCloseableResource{
		&bytesresource.BytesResource{Content: []byte("Hello")},
		&bytesresource.BytesResource{Content: []byte("World")},
		&bytesresource.BytesResource{Content: []byte("AndGoodbye")},
	}
}

func TestReadAtAcrossResources(t *testing.T) {
	t.Parallel()

	reader, err := adaptiveparallelmergerresource.NewAdaptiveParallelMergerResource(parts()).Open()
	if err != nil {
		t.Fatalf("Open() = %v, want no error", err)
	}
	defer reader.Close()

	readerAt, ok := reader.(io.ReaderAt)
	if !ok {
		t.Fatal("reader does not answer positional reads")
	}

	for off := range len(content) {
		for length := 1; off+length <= len(content); length++ {
			buffer := make([]byte, length)

			n, err := readerAt.ReadAt(buffer, int64(off))
			if err != nil && !errors.Is(err, io.EOF) {
				t.Fatalf("ReadAt(%d, %d) = %v, want no error", length, off, err)
			}
			if got, want := string(buffer[:n]), content[off:off+length]; got != want {
				t.Errorf("ReadAt(%d, %d) = %q, want %q", length, off, got, want)
			}
		}
	}
}

// A read running off the end reports what it got and io.EOF, which is what
// io.ReaderAt promises and what tells a presenter where the file ends.
func TestReadAtPastEnd(t *testing.T) {
	t.Parallel()

	reader, err := adaptiveparallelmergerresource.NewAdaptiveParallelMergerResource(parts()).Open()
	if err != nil {
		t.Fatalf("Open() = %v, want no error", err)
	}
	defer reader.Close()

	readerAt, _ := reader.(io.ReaderAt)

	buffer := make([]byte, 8)
	n, err := readerAt.ReadAt(buffer, int64(len(content))-3)
	if !errors.Is(err, io.EOF) {
		t.Errorf("ReadAt at end = %v, want io.EOF", err)
	}
	if got, want := string(buffer[:n]), content[len(content)-3:]; got != want {
		t.Errorf("ReadAt at end = %q, want %q", got, want)
	}

	if _, err := readerAt.ReadAt(buffer, int64(len(content))); !errors.Is(err, io.EOF) {
		t.Errorf("ReadAt past end = %v, want io.EOF", err)
	}
}

// A resource that cannot answer its own size is measured, which is what keeps
// the offsets exact where a hint would send a read to the wrong byte.
func TestReadAtMeasuresInexactResources(t *testing.T) {
	t.Parallel()

	resources := []resource.ReadSeekCloseableResource{
		NewTestResouce([]byte("Hello"), 3),
		NewTestResouce([]byte("World"), 8),
	}

	reader, err := adaptiveparallelmergerresource.NewAdaptiveParallelMergerResource(resources).Open()
	if err != nil {
		t.Fatalf("Open() = %v, want no error", err)
	}
	defer reader.Close()

	readerAt, _ := reader.(io.ReaderAt)

	buffer := make([]byte, 7)
	n, err := readerAt.ReadAt(buffer, 3)
	if err != nil && !errors.Is(err, io.EOF) {
		t.Fatalf("ReadAt = %v, want no error", err)
	}
	if got, want := string(buffer[:n]), "loWorld"; got != want {
		t.Errorf("ReadAt = %q, want %q", got, want)
	}
}

// barrier releases nothing until as many readers have arrived as there are
// resources to read, so it completes only where they run at once.
type barrier struct {
	n        int
	mu       sync.Mutex
	arrived  int
	all      chan struct{}
	timedOut atomic.Bool
}

func (b *barrier) wait() {
	b.mu.Lock()
	b.arrived++
	if b.arrived == b.n {
		close(b.all)
	}
	b.mu.Unlock()

	select {
	case <-b.all:
	case <-time.After(2 * time.Second):
		b.timedOut.Store(true)
	}
}

type barrierResource struct {
	content []byte
	barrier *barrier
	// inexact holds the resource to the barrier where its length is measured
	// rather than where it is read
	inexact bool
}

func (r *barrierResource) Open() (io.ReadSeekCloser, error) { return &barrierReader{resource: r}, nil }
func (r *barrierResource) SizeHint() (int64, error)         { return int64(len(r.content)), nil }

func (r *barrierResource) Size() (int64, error) {
	if r.inexact {
		return 0, resource.ErrSizeNotExact
	}

	return int64(len(r.content)), nil
}

type barrierReader struct {
	resource *barrierResource
	index    int64
}

func (r *barrierReader) Close() error { return nil }

func (r *barrierReader) ReadAt(p []byte, off int64) (int, error) {
	if !r.resource.inexact {
		r.resource.barrier.wait()
	}

	if off >= int64(len(r.resource.content)) {
		return 0, io.EOF
	}
	n := copy(p, r.resource.content[off:])
	if n < len(p) {
		return n, io.EOF
	}

	return n, nil
}

func (r *barrierReader) Read(p []byte) (int, error) {
	n, err := r.ReadAt(p, r.index)
	r.index += int64(n)

	return n, err
}

func (r *barrierReader) Seek(offset int64, whence int) (int64, error) {
	switch whence {
	case io.SeekStart:
		r.index = offset
	case io.SeekCurrent:
		r.index += offset
	case io.SeekEnd:
		if r.resource.inexact {
			r.resource.barrier.wait()
		}
		r.index = int64(len(r.resource.content)) + offset
	}

	return r.index, nil
}

// A read crossing resources has to issue them at once: serially it would pay the
// latency of every part it touches, which for a segment is a download.
func TestReadAtCrossingResourcesRunsInParallel(t *testing.T) {
	t.Parallel()

	pieces := []string{"Hello", "World", "AndGoodbye"}
	gate := &barrier{n: len(pieces), all: make(chan struct{})}

	resources := make([]resource.ReadSeekCloseableResource, 0, len(pieces))
	for _, piece := range pieces {
		resources = append(resources, &barrierResource{content: []byte(piece), barrier: gate})
	}

	reader, err := adaptiveparallelmergerresource.NewAdaptiveParallelMergerResource(resources).Open()
	if err != nil {
		t.Fatalf("Open() = %v, want no error", err)
	}
	defer reader.Close()

	readerAt, _ := reader.(io.ReaderAt)

	buffer := make([]byte, len(content))
	n, err := readerAt.ReadAt(buffer, 0)
	if err != nil {
		t.Fatalf("ReadAt = %v, want no error", err)
	}
	if gate.timedOut.Load() {
		t.Error("ReadAt issued its parts one after the other, want all at once")
	}
	if got := string(buffer[:n]); got != content {
		t.Errorf("ReadAt = %q, want %q", got, content)
	}
}

// Measuring a resource that cannot answer its own size is a download, so the
// ones a read has to cross are measured at once rather than one after the other.
func TestReadAtMeasuresInexactResourcesInParallel(t *testing.T) {
	t.Parallel()

	pieces := []string{"Hello", "World", "AndGoodbye"}
	gate := &barrier{n: len(pieces), all: make(chan struct{})}

	resources := make([]resource.ReadSeekCloseableResource, 0, len(pieces))
	for _, piece := range pieces {
		resources = append(resources, &barrierResource{content: []byte(piece), barrier: gate, inexact: true})
	}

	reader, err := adaptiveparallelmergerresource.NewAdaptiveParallelMergerResource(resources).Open()
	if err != nil {
		t.Fatalf("Open() = %v, want no error", err)
	}
	defer reader.Close()

	readerAt, _ := reader.(io.ReaderAt)

	buffer := make([]byte, len(content))
	n, err := readerAt.ReadAt(buffer, 0)
	if err != nil {
		t.Fatalf("ReadAt = %v, want no error", err)
	}
	if gate.timedOut.Load() {
		t.Error("ReadAt measured the resources one after the other, want all at once")
	}
	if got := string(buffer[:n]); got != content {
		t.Errorf("ReadAt = %q, want %q", got, content)
	}
}

// Positional reads carry no position, so concurrent ones must not move anything
// out from under each other. Run with -race.
func TestReadAtConcurrent(t *testing.T) {
	t.Parallel()

	reader, err := adaptiveparallelmergerresource.NewAdaptiveParallelMergerResource(parts()).Open()
	if err != nil {
		t.Fatalf("Open() = %v, want no error", err)
	}
	defer reader.Close()

	readerAt, _ := reader.(io.ReaderAt)

	var group sync.WaitGroup
	for off := range len(content) {
		group.Add(1)
		go func() {
			defer group.Done()

			for range 20 {
				buffer := make([]byte, 4)
				n, err := readerAt.ReadAt(buffer, int64(off))
				if err != nil && !errors.Is(err, io.EOF) {
					t.Errorf("ReadAt(4, %d) = %v, want no error", off, err)
					return
				}

				want := content[off:min(off+4, len(content))]
				if !bytes.Equal(buffer[:n], []byte(want)) {
					t.Errorf("ReadAt(4, %d) = %q, want %q", off, buffer[:n], want)
					return
				}
			}
		}()
	}
	group.Wait()
}
