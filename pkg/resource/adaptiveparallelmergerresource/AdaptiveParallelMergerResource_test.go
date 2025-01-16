package adaptiveparallelmergerresource_test

import (
	"bytes"
	"io"
	"testing"

	"git.ruekov.eu/ruakij/nzbStreamer/pkg/resource"
	"git.ruekov.eu/ruakij/nzbStreamer/pkg/resource/adaptiveparallelmergerresource"
	"git.ruekov.eu/ruakij/nzbStreamer/pkg/resource/bytesresource"
)

type TestResource struct {
	resource     resource.ReadSeekCloseableResource
	size         int64
	sizeAccurate bool
}

type TestResourceReader struct {
	resource *TestResource
	reader   io.ReadSeekCloser
}

func NewTestResouce(data []byte, fakeSize int64) resource.ReadSeekCloseableResource {
	return &TestResource{
		resource:     &bytesresource.BytesResource{Content: data},
		size:         fakeSize,
		sizeAccurate: int64(len(data)) == fakeSize,
	}
}

func (r *TestResource) Open() (io.ReadSeekCloser, error) {
	reader, err := r.resource.Open()
	return &TestResourceReader{
		resource: r,
		reader:   reader,
	}, err
}

func (r *TestResource) Size() (int64, error) {
	return r.size, nil
}

func (r *TestResource) IsSizeAccurate() bool {
	return r.sizeAccurate
}

func (r *TestResource) SetFakeSize(size int64) {
	r.size = size
	r.sizeAccurate = false
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
	t.Parallel()

	resources := []resource.ReadSeekCloseableResource{
		&bytesresource.BytesResource{Content: []byte("Hel")},
		&bytesresource.BytesResource{Content: []byte("lo")},
		&bytesresource.BytesResource{Content: []byte("World")},
	}

	merger := adaptiveparallelmergerresource.NewAdaptiveParallelMergerResource(resources)

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
	t.Parallel()

	resources := []resource.ReadSeekCloseableResource{
		NewTestResouce([]byte("Hel"), 1),
		NewTestResouce([]byte("lo"), 1),
		NewTestResouce([]byte("World"), 2),
	}

	merger := adaptiveparallelmergerresource.NewAdaptiveParallelMergerResource(resources)

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

func TestSeekForward(t *testing.T) {
	t.Parallel()

	resources := []resource.ReadSeekCloseableResource{
		NewTestResouce([]byte("Hel"), 1),
		NewTestResouce([]byte("lo"), 1),
		NewTestResouce([]byte("World"), 2),
	}

	merger := adaptiveparallelmergerresource.NewAdaptiveParallelMergerResource(resources)

	readSeeker, err := merger.Open()
	if err != nil {
		t.Errorf("unexpected error: %v", err)
		return
	}

	data, err := io.ReadAll(io.LimitReader(readSeeker, 2))
	if err != nil {
		t.Errorf("unexpected error: %v", err)
		return
	}
	if string(data) != "He" {
		t.Errorf("expected 'He', got: %s", data)
		return
	}

	n, err := readSeeker.Seek(4, io.SeekCurrent)
	if err != nil {
		t.Errorf("unexpected error: %v", err)
		return
	}
	if n != 6 {
		t.Errorf("expected seek-result 6, got: %d", n)
		return
	}

	data, err = io.ReadAll(io.LimitReader(readSeeker, 2))
	if err != nil {
		t.Errorf("unexpected error: %v", err)
		return
	}
	if string(data) != "or" {
		t.Errorf("expected 'or', got: %s", data)
		return
	}

	n, err = readSeeker.Seek(-1, io.SeekEnd)
	if err != nil {
		t.Errorf("unexpected error: %v", err)
		return
	}
	if n != 9 {
		t.Errorf("expected seek-result 9, got: %d", n)
		return
	}

	data, err = io.ReadAll(io.LimitReader(readSeeker, 1))
	if err != nil {
		t.Errorf("unexpected error: %v", err)
		return
	}
	if string(data) != "d" {
		t.Errorf("expected 'd', got: %s", data)
		return
	}
}

func TestSeekBackward(t *testing.T) {
	t.Parallel()

	resources := []resource.ReadSeekCloseableResource{
		NewTestResouce([]byte("Hel"), 1),
		NewTestResouce([]byte("lo"), 1),
		NewTestResouce([]byte("World"), 2),
	}

	merger := adaptiveparallelmergerresource.NewAdaptiveParallelMergerResource(resources)

	readSeeker, err := merger.Open()
	if err != nil {
		t.Errorf("unexpected error: %v", err)
		return
	}

	n, err := readSeeker.Seek(2, io.SeekCurrent)
	if err != nil {
		t.Errorf("unexpected error: %v", err)
		return
	}
	if n != 2 {
		t.Errorf("expected seek-result 2, got: %d", n)
		return
	}

	data, err := io.ReadAll(io.LimitReader(readSeeker, 2))
	if err != nil {
		t.Errorf("unexpected error: %v", err)
		return
	}
	if string(data) != "ll" {
		t.Errorf("expected 'll', got: %s", data)
		return
	}

	n, err = readSeeker.Seek(-2, io.SeekCurrent)
	if err != nil {
		t.Errorf("unexpected error: %v", err)
		return
	}
	if n != 2 {
		t.Errorf("expected seek-result 2, got: %d", n)
		return
	}

	data, err = io.ReadAll(io.LimitReader(readSeeker, 2))
	if err != nil {
		t.Errorf("unexpected error: %v", err)
		return
	}
	if string(data) != "ll" {
		t.Errorf("expected 'll', got: %s", data)
		return
	}

	n, err = readSeeker.Seek(0, io.SeekEnd)
	if err != nil {
		t.Errorf("unexpected error: %v", err)
		return
	}
	if n != 10 {
		t.Errorf("expected seek-result 10, got: %d", n)
		return
	}

	n, err = readSeeker.Seek(3, io.SeekStart)
	if err != nil {
		t.Errorf("unexpected error: %v", err)
		return
	}
	if n != 3 {
		t.Errorf("expected seek-result 3, got: %d", n)
		return
	}

	data, err = io.ReadAll(io.LimitReader(readSeeker, 2))
	if err != nil {
		t.Errorf("unexpected error: %v", err)
		return
	}
	if string(data) != "lo" {
		t.Errorf("expected 'lo', got: %s", data)
		return
	}
}
