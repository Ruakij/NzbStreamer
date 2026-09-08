package nzbrecordfactory

import (
	"errors"
	"testing"

	"git.ruekov.eu/ruakij/nzbStreamer/internal/presentation"
	"git.ruekov.eu/ruakij/nzbStreamer/pkg/resource"
	"git.ruekov.eu/ruakij/nzbStreamer/pkg/resource/bytesresource"
)

// An archive that cannot be opened, here because its bytes are not a rar at
// all, leaves its volumes presented so a client can fetch and unpack them
// itself.
func TestUnopenableArchiveIsPresentedAsItsVolumes(t *testing.T) {
	entries := map[string]resource.ReadSeekCloseableResource{
		"x.part01.rar": &bytesresource.BytesResource{Content: []byte("not a rar")},
		"x.part02.rar": &bytesresource.BytesResource{Content: []byte("not a rar")},
	}

	factory := NewNzbFileFactory(nil, nil, nil, 0, 2)
	files := make(map[string]presentation.Openable)
	err := factory.expand(entries, "", 0, "wrong-password", files, &buildProgress{})

	if !errors.Is(err, ErrArchiveLeftPacked) {
		t.Fatalf("want ErrArchiveLeftPacked, got %v", err)
	}
	for name := range entries {
		if _, ok := files[name]; !ok {
			t.Errorf("volume %s is not presented", name)
		}
	}
}
