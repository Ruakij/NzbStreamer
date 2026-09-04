package rarfileresource

import (
	"errors"
	"io"
	"io/fs"
	"testing"

	"git.ruekov.eu/ruakij/nzbStreamer/pkg/resource"
	"git.ruekov.eu/ruakij/nzbStreamer/pkg/resource/bytesresource"
)

func newTestFS(contents ...string) *volumeFS {
	volumes := make([]resource.ReadSeekCloseableResource, len(contents))
	for i, content := range contents {
		volumes[i] = &bytesresource.BytesResource{Content: []byte(content)}
	}
	return newVolumeFS(volumes)
}

// rardecode walks volumes by incrementing the digit run of the previous name,
// so the generated names have to form exactly that sequence.
func TestVolumeNamesFormRarSequence(t *testing.T) {
	want := []string{"volume.part0001.rar", "volume.part0002.rar", "volume.part0003.rar"}
	for i, name := range want {
		if got := volumeName(i); got != name {
			t.Errorf("volumeName(%d) = %q, want %q", i, got, name)
		}
	}
	if firstVolumeName != want[0] {
		t.Errorf("firstVolumeName = %q, want %q", firstVolumeName, want[0])
	}
}

// An archive whose header asks for the old naming scheme is walked as .r00
// upwards from the first volumes name, so the same volume answers to both.
func TestVolumeFSServesOldVolumeNames(t *testing.T) {
	fsys := newTestFS("first", "second", "third")

	for name, want := range map[string]string{
		"volume.part0001.r00": "second",
		"volume.part0001.r01": "third",
	} {
		file, err := fsys.Open(name)
		if err != nil {
			t.Fatalf("opening %s: %v", name, err)
		}
		got, err := io.ReadAll(file)
		file.Close()
		if err != nil {
			t.Fatalf("reading %s: %v", name, err)
		}
		if string(got) != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
}

func TestOldVolumeNamesFormRarSequence(t *testing.T) {
	cases := map[string]string{
		"volume.part0001.rar": "volume.part0001.r00",
		"volume.part0001.r00": "volume.part0001.r01",
		"volume.part0001.r09": "volume.part0001.r10",
		"volume.part0001.r99": "volume.part0001.s00",
	}
	for previous, want := range cases {
		if got := oldVolumeName(previous); got != want {
			t.Errorf("oldVolumeName(%q) = %q, want %q", previous, got, want)
		}
	}
}

func TestVolumeFSServesVolumesInOrder(t *testing.T) {
	fsys := newTestFS("first", "second")

	for i, want := range []string{"first", "second"} {
		file, err := fsys.Open(volumeName(i))
		if err != nil {
			t.Fatalf("opening volume %d: %v", i, err)
		}
		got, err := io.ReadAll(file)
		file.Close()
		if err != nil {
			t.Fatalf("reading volume %d: %v", i, err)
		}
		if string(got) != want {
			t.Errorf("volume %d = %q, want %q", i, got, want)
		}
	}
}

func TestVolumeFSReportsEndOfArchive(t *testing.T) {
	fsys := newTestFS("only")

	_, err := fsys.Open(volumeName(1))
	if !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("opening past the last volume = %v, want fs.ErrNotExist", err)
	}
}

func TestVolumeFileSeeks(t *testing.T) {
	fsys := newTestFS("0123456789")

	file, err := fsys.Open(firstVolumeName)
	if err != nil {
		t.Fatalf("opening volume: %v", err)
	}
	defer file.Close()

	seeker, ok := file.(io.Seeker)
	if !ok {
		t.Fatal("volume file does not seek, rardecode cannot address blocks in it")
	}
	if _, err := seeker.Seek(4, io.SeekStart); err != nil {
		t.Fatalf("seeking: %v", err)
	}

	got := make([]byte, 3)
	if _, err := io.ReadFull(file, got); err != nil {
		t.Fatalf("reading after seek: %v", err)
	}
	if string(got) != "456" {
		t.Errorf("read after seek = %q, want %q", got, "456")
	}
}
