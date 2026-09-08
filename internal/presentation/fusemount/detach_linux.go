package fusemount

import (
	"fmt"
	"os/exec"
	"syscall"
)

// detach takes the mountpoint out of the tree at once and leaves the kernel to
// release it when the last handle is gone.
func detach(path string) error {
	if err := syscall.Unmount(path, syscall.MNT_DETACH); err == nil {
		return nil
	}

	var err error
	for _, helper := range []string{"fusermount3", "fusermount"} {
		out, helperErr := exec.Command(helper, "-u", "-z", "--", path).CombinedOutput()
		if helperErr == nil {
			return nil
		}
		err = fmt.Errorf("%s: %w: %s", helper, helperErr, out)
	}

	return fmt.Errorf("failed detaching %s: %w", path, err)
}
