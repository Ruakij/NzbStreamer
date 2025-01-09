package AdaptiveParallelMergerResource

import (
	"bytes"
	"git.ruekov.eu/ruakij/nzbStreamer/pkg/resource"
	"git.ruekov.eu/ruakij/nzbStreamer/pkg/resource/BytesResource"
	"io"
	"testing"
)

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
