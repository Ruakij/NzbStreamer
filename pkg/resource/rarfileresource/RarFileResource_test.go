package rarfileresource

import (
	"errors"
	"io"
	"strings"
	"testing"

	"git.ruekov.eu/ruakij/nzbStreamer/pkg/resource"
)

var errUnreachable = errors.New("volume was read")

// unreachableVolume fails on every access, so any attempt to reach the archive
// shows up as an error rather than as a silent fetch.
type unreachableVolume struct{}

func (unreachableVolume) Open() (io.ReadSeekCloser, error) { return nil, errUnreachable }
func (unreachableVolume) SizeHint() (int64, error)         { return 0, errUnreachable }

// Stat is answered for every entry of a mount, so it has to come from the
// header the archive listing already produced, not from opening the archive.
func TestSizeFromHeaderTouchesNoVolume(t *testing.T) {
	volumes := []resource.ReadSeekCloseableResource{unreachableVolume{}, unreachableVolume{}}
	rar := NewRarFileResource(volumes, "", "movie.mkv", 4242)

	size, err := rar.Size()
	if err != nil {
		t.Fatalf("Size() = %v, want no error", err)
	}
	if size != 4242 {
		t.Errorf("Size() = %d, want 4242", size)
	}
}

// A password longer than what WinRAR keys with is only correct cut to that
// length, which is how the archive was encrypted.
func TestPasswordIsCutToWhatWinrarKeysWith(t *testing.T) {
	long := strings.Repeat("a", maxPasswordLen) + "bcd"
	if got := truncatePassword(long); len(got) != maxPasswordLen {
		t.Errorf("want %d characters, got %d", maxPasswordLen, len(got))
	}

	// Multi-byte characters count as one each, the way WinRAR counts them
	short := strings.Repeat("ü", maxPasswordLen)
	if got := truncatePassword(short); got != short {
		t.Errorf("a password of %d characters was cut", maxPasswordLen)
	}
}
