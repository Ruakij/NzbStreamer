package adaptiveparallelmergerresource_test

import (
	"bytes"
	"errors"
	"io"
	"sync"
	"testing"

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
