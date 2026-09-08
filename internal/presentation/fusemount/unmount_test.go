package fusemount

import (
	"errors"
	"testing"
	"time"
)

var errBusy = errors.New("device or resource busy")

func TestUnmountWaitsForTheMountToGoIdle(t *testing.T) {
	tries, detached := 0, false
	err := unmountWithin(time.Minute, func() error {
		tries++
		if tries < 3 {
			return errBusy
		}

		return nil
	}, func() error {
		detached = true

		return nil
	})

	if err != nil || tries != 3 || detached {
		t.Errorf("unmounted after %d tries, detached %v, err %v; want 3 tries and no detach", tries, detached, err)
	}
}

func TestAMountThatNeverComesFreeIsDetached(t *testing.T) {
	detached := false
	err := unmountWithin(0, func() error { return errBusy }, func() error {
		detached = true

		return nil
	})

	if err != nil || !detached {
		t.Errorf("detached %v, err %v; want a detach and no error", detached, err)
	}
}
