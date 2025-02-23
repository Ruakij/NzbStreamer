package parallelmergerresource_test

import (
	"bytes"
	"io"
	"testing"

	"git.ruekov.eu/ruakij/nzbStreamer/pkg/resource"
	"git.ruekov.eu/ruakij/nzbStreamer/pkg/resource/bytesresource"
	"git.ruekov.eu/ruakij/nzbStreamer/pkg/resource/parallelmergerresource"
)

func TestParallelMergerResource(t *testing.T) {
	t.Parallel()

	resources := []resource.ReadSeekCloseableResource{
		&bytesresource.BytesResource{Content: []byte("Hel")},
		&bytesresource.BytesResource{Content: []byte("lo")},
		&bytesresource.BytesResource{Content: []byte("World")},
	}

	merger := parallelmergerresource.NewParallelMergerResource(resources)

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

	_, err = readSeeker.Seek(20, io.SeekStart)
	if err == nil {
		t.Errorf("failed to error on SeekStart with offset=%d", 1)
	}

	seekStartOffset, err = readSeeker.Seek(-7, io.SeekEnd)
	if err != nil {
		t.Errorf("failed to SeekEnd with offset=%d: %v", -7, err)
	}
	if seekStartOffset != 3 {
		t.Errorf("failed to SeekCurrent with offset=%d, returned seekStartOffset=%d: %v", 1, seekStartOffset, err)
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
