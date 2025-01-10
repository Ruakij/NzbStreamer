package AdaptiveParallelMergerResource

import (
	"bytes"
	"git.ruekov.eu/ruakij/nzbStreamer/pkg/resource"
	"git.ruekov.eu/ruakij/nzbStreamer/pkg/resource/BytesResource"
	"io"
	"testing"
)

type TestResource struct {
	resource resource.ReadSeekCloseableResource
	size     int64
}

type TestResourceReader struct {
	resource *TestResource
	reader   io.ReadSeekCloser
}

func NewTestResouce(data []byte, fakeSize int64) resource.ReadSeekCloseableResource {
	return &TestResource{
		resource: &BytesResource.BytesResource{Content: data},
		size:     fakeSize,
	}
}

func (r *TestResource) Open() (io.ReadSeekCloser, error) {
	reader, _ := r.resource.Open()
	return &TestResourceReader{
		resource: r,
		reader:   reader,
	}, nil
}
func (r *TestResource) Size() (int64, error) {
	return r.size, nil
}
func (r *TestResource) SetFakeSize(size int64) {
	r.size = size
}
func (r *TestResourceReader) Close() error {
	return r.reader.Close()
}
func (r *TestResourceReader) Read(p []byte) (n int, err error) {
	return r.reader.Read(p)
}
func (r *TestResourceReader) Seek(offset int64, whence int) (int64, error) {
	return r.reader.Seek(offset, whence)
}

func TestAdaptiveParallelMergerResource(t *testing.T) {
	resources := []resource.ReadSeekCloseableResource{
		&BytesResource.BytesResource{Content: []byte("Hel")},
		&BytesResource.BytesResource{Content: []byte("lo")},
		&BytesResource.BytesResource{Content: []byte("World")},
	}

	merger := NewAdaptiveParallelMergerResource(resources)

	size, err := merger.Size()
	if err != nil {
		t.Errorf("failed get Size() %v", err)
	}

	expectedSize := int64(10)
	if size != expectedSize {
		t.Errorf("expected size %d, got %d", expectedSize, size)
	}

	readSeeker, err := merger.Open()
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}

	seekStartOffset, err := readSeeker.Seek(1, io.SeekCurrent)
	if err != nil {
		t.Errorf("failed to SeekCurrent with offset=%d: %v", 1, err)
	}
	if seekStartOffset != 1 {
		t.Errorf("failed to SeekCurrent with offset=%d, returned seekStartOffset=%d: %v", 1, seekStartOffset, err)
	}

	seekStartOffset, err = readSeeker.Seek(-5, io.SeekEnd)
	if err != nil {
		t.Errorf("failed to SeekEnd with offset=%d: %v", -5, err)
	}
	if seekStartOffset != 5 {
		t.Errorf("failed to SeekEnd with offset=%d, returned seekStartOffset=%d: %v", -5, seekStartOffset, err)
	}

	seekStartOffset, err = readSeeker.Seek(-1, io.SeekCurrent)
	if err != nil {
		t.Errorf("failed to SeekCurrent with offset=%d: %v", -1, err)
	}
	if seekStartOffset != 4 {
		t.Errorf("failed to SeekCurrent with offset=%d, returned seekStartOffset=%d: %v", -1, seekStartOffset, err)
	}

	seekStartOffset, err = readSeeker.Seek(3, io.SeekStart)
	if err != nil {
		t.Errorf("failed to SeekStart with offset=%d: %v", 3, err)
	}
	if seekStartOffset != 3 {
		t.Errorf("failed to SeekStart with offset=%d, returned seekStartOffset=%d: %v", 3, seekStartOffset, err)
	}

	buf := new(bytes.Buffer)
	_, err = buf.ReadFrom(readSeeker)
	if err != nil {
		t.Errorf("failed to read from resource merger: %v", err)
	}

	expectedContent := "loWorld"
	actualContent := buf.String()
	if expectedContent != actualContent {
		t.Errorf("expected content %s, got %s", expectedContent, actualContent)
	}

	err = readSeeker.Close()
	if err != nil {
		t.Errorf("failed to close: %v", err)
	}
}

func TestAdaptiveParallelMergerResourceWithFakeSize(t *testing.T) {
	resources := []resource.ReadSeekCloseableResource{
		NewTestResouce([]byte("Hel"), 1),
		NewTestResouce([]byte("lo"), 1),
		NewTestResouce([]byte("World"), 2),
	}

	merger := NewAdaptiveParallelMergerResource(resources)

	readSeeker, err := merger.Open()
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}

	seekStartOffset, err := readSeeker.Seek(1, io.SeekCurrent)
	if err != nil {
		t.Errorf("failed to SeekCurrent with offset=%d: %v", 1, err)
	}
	if seekStartOffset != 1 {
		t.Errorf("failed to SeekCurrent with offset=%d, returned seekStartOffset=%d: %v", 1, seekStartOffset, err)
	}

	seekStartOffset, err = readSeeker.Seek(-5, io.SeekEnd)
	if err != nil {
		t.Errorf("failed to SeekEnd with offset=%d: %v", -5, err)
	}
	if seekStartOffset != 5 {
		t.Errorf("failed to SeekEnd with offset=%d, returned seekStartOffset=%d: %v", -5, seekStartOffset, err)
	}

	seekStartOffset, err = readSeeker.Seek(-1, io.SeekCurrent)
	if err != nil {
		t.Errorf("failed to SeekCurrent with offset=%d: %v", -1, err)
	}
	if seekStartOffset != 4 {
		t.Errorf("failed to SeekCurrent with offset=%d, returned seekStartOffset=%d: %v", -1, seekStartOffset, err)
	}

	seekStartOffset, err = readSeeker.Seek(0, io.SeekStart)
	if err != nil {
		t.Errorf("failed to SeekStart with offset=%d: %v", 0, err)
	}
	if seekStartOffset != 0 {
		t.Errorf("failed to SeekStart with offset=%d, returned seekStartOffset=%d: %v", 0, seekStartOffset, err)
	}

	n, err := readSeeker.Read([]byte{0, 0, 0})
	if err != nil {
		t.Errorf("failed to read from resource merger: %v", err)
	}
	if n != 3 {
		t.Errorf("failed to read from resource expected len=%d, but got len=%d", 3, n)
	}

	seekStartOffset, err = readSeeker.Seek(0, io.SeekStart)
	if err != nil {
		t.Errorf("failed to SeekStart with offset=%d: %v", 0, err)
	}
	if seekStartOffset != 0 {
		t.Errorf("failed to SeekStart with offset=%d, returned seekStartOffset=%d: %v", 0, seekStartOffset, err)
	}

	buf := new(bytes.Buffer)
	nn, err := io.CopyN(buf, readSeeker, 5)
	if err != nil {
		t.Errorf("failed to read from resource merger: %v", err)
	}
	if nn != 5 {
		t.Errorf("failed to read from resource expected len=%d, but got len=%d", 5, nn)
	}

	expectedContent := "Hello"
	actualContent := buf.String()
	if expectedContent != actualContent {
		t.Errorf("expected content %s, got %s", expectedContent, actualContent)
	}

	err = readSeeker.Close()
	if err != nil {
		t.Errorf("failed to close: %v", err)
	}
}
