//go:build !linux

package fusemount

import (
	"fmt"

	"golang.org/x/sys/unix"
)

// detach forces the mountpoint down. Deferring the release until the last
// handle is gone is a Linux thing, so here the reads in flight are what is
// given up to keep the mountpoint from outliving the process.
func detach(path string) error {
	if err := unix.Unmount(path, unix.MNT_FORCE); err != nil {
		return fmt.Errorf("failed detaching %s: %w", path, err)
	}

	return nil
}
