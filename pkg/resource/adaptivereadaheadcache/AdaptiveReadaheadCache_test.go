package adaptivereadaheadcache_test

import (
	"io"
	"testing"
	"time"

	"git.ruekov.eu/ruakij/nzbStreamer/pkg/resource/adaptivereadaheadcache"
	"git.ruekov.eu/ruakij/nzbStreamer/pkg/resource/bytesresource"
)

func TestAdaptiveReadaheadCache_Read(t *testing.T) {
	t.Parallel()

	res := bytesresource.BytesResource{Content: []byte("Hello, World!")}
	arc := adaptivereadaheadcache.NewAdaptiveReadaheadCache(&res, time.Second, time.Second*2, 5, 20, 10)

	reader, err := arc.Open()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer reader.Close()

	buf := make([]byte, 5)
	n, err := reader.Read(buf)
	if err != nil && err != io.EOF {
		t.Fatalf("unexpected error: %v", err)
	}

	expected := "Hello"
	if string(buf[:n]) != expected {
		t.Errorf("expected %q, got %q", expected, string(buf[:n]))
	}

	// Testing the readahead by reading more
	n, err = reader.Read(buf)
	if err != nil && err != io.EOF {
		t.Fatalf("unexpected error: %v", err)
	}

	expected = ", Wor"
	if string(buf[:n]) != expected {
		t.Errorf("expected %q, got %q", expected, string(buf[:n]))
	}
}

func TestAdaptiveReadaheadCache_Seek(t *testing.T) {
	t.Parallel()

	res := bytesresource.BytesResource{Content: []byte("Hello, World!")}
	arc := adaptivereadaheadcache.NewAdaptiveReadaheadCache(&res, time.Second, time.Second*2, 5, 20, 10)

	reader, err := arc.Open()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer reader.Close()

	// Seek to 'Hello, '
	_, err = reader.Seek(7, io.SeekStart)
	if err != nil {
		t.Fatalf("unexpected error while seeking: %v", err)
	}

	buf := make([]byte, 5)
	n, err := reader.Read(buf)
	if err != nil && err != io.EOF {
		t.Fatalf("unexpected error: %v", err)
	}

	expected := "World"
	if string(buf[:n]) != expected {
		t.Errorf("expected %q, got %q", expected, string(buf[:n]))
	}
}

func TestAdaptiveReadaheadCache_MultipleSeeks(t *testing.T) {
	t.Parallel()

	res := bytesresource.BytesResource{Content: []byte("Hello, World!")}
	arc := adaptivereadaheadcache.NewAdaptiveReadaheadCache(&res, time.Second, time.Second*2, 5, 20, 10)

	reader, err := arc.Open()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer reader.Close()

	// Seek to 'Hello'
	if _, err := reader.Seek(0, io.SeekStart); err != nil {
		t.Fatalf("unexpected error while seeking to start: %v", err)
	}

	buf := make([]byte, 5)
	if _, err := reader.Read(buf); err != nil {
		t.Fatalf("unexpected error on first read: %v", err)
	}
	if string(buf) != "Hello" {
		t.Errorf("expected 'Hello', got %q", string(buf))
	}

	// Seek forwards to ', '
	if _, err := reader.Seek(2, io.SeekCurrent); err != nil {
		t.Fatalf("unexpected error while seeking forward: %v", err)
	}
	if _, err := reader.Read(buf); err != nil {
		t.Fatalf("unexpected error on second read: %v", err)
	}
	if string(buf) != "World" {
		t.Errorf("expected 'World', got %q", string(buf))
	}

	// Seek backwards to 'Hello'
	if _, err := reader.Seek(-5, io.SeekCurrent); err != nil {
		t.Fatalf("unexpected error while seeking backward: %v", err)
	}
	if _, err := reader.Read(buf); err != nil {
		t.Fatalf("unexpected error on third read: %v", err)
	}
	if string(buf) != "World" {
		t.Errorf("expected 'World', got %q", string(buf))
	}

	// Seek to end
	if _, err := reader.Seek(0, io.SeekEnd); err != nil {
		t.Fatalf("unexpected error while seeking to end: %v", err)
	}
}

func TestAdaptiveReadaheadCache_EOF(t *testing.T) {
	t.Parallel()

	res := bytesresource.BytesResource{Content: []byte("Hello")}
	arc := adaptivereadaheadcache.NewAdaptiveReadaheadCache(&res, time.Second, time.Second*2, 5, 20, 10)

	reader, err := arc.Open()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer reader.Close()

	buf := make([]byte, 10)
	n, err := reader.Read(buf)
	if err != nil && err != io.EOF {
		t.Fatalf("unexpected error: %v", err)
	}

	expected := "Hello"
	if string(buf[:n]) != expected {
		t.Errorf("expected %q, got %q", expected, string(buf[:n]))
	}

	if n != 5 {
		t.Fatalf("expected to read 5 bytes, got: %d", n)
	}

	n, _ = reader.Read(buf)
	if n != 0 {
		t.Fatalf("expected EOF, but read %d bytes", n)
	}
}

func TestAdaptiveReadaheadCache_MultipleReads(t *testing.T) {
	t.Parallel()

	res := bytesresource.BytesResource{Content: []byte("Hello, World!")}
	arc := adaptivereadaheadcache.NewAdaptiveReadaheadCache(&res, time.Second, time.Second*2, 5, 20, 10)

	reader, err := arc.Open()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer reader.Close()

	buf := make([]byte, 5)

	// Read multiple times
	for i := 0; i < 3; i++ {
		n, err := reader.Read(buf)
		if err != nil && err != io.EOF {
			t.Fatalf("unexpected error during read: %v", err)
		}
		if n > 0 {
			t.Logf("Read: %q", string(buf[:n]))
		}
	}
}

func TestAdaptiveReadaheadCache_SequentialReadWriteSeek(t *testing.T) {
	t.Parallel()

	content := []byte("Hello, World!")
	res := bytesresource.BytesResource{Content: content}
	arc := adaptivereadaheadcache.NewAdaptiveReadaheadCache(&res, time.Second, time.Second*2, 5, 20, 10)

	reader, err := arc.Open()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer reader.Close()

	buf := make([]byte, 5)

	// Initial read
	if _, err := reader.Read(buf); err != nil {
		t.Fatalf("unexpected error on initial read: %v", err)
	}

	// Seek to 'World'
	if _, err := reader.Seek(7, io.SeekStart); err != nil {
		t.Fatalf("unexpected error while seeking: %v", err)
	}

	// Next read ('World')
	if _, err := reader.Read(buf); err != nil {
		t.Fatalf("unexpected error on second read: %v", err)
	}

	if string(buf) != "World" {
		t.Errorf("expected 'World', got %q", string(buf))
	}

	// Seek back to the start and read again
	if _, err := reader.Seek(0, io.SeekStart); err != nil {
		t.Fatalf("unexpected error while seeking to start: %v", err)
	}

	if _, err := reader.Read(buf); err != nil {
		t.Fatalf("unexpected error on third read: %v", err)
	}

	if string(buf) != "Hello" {
		t.Errorf("expected 'Hello', got %q", string(buf))
	}
}
