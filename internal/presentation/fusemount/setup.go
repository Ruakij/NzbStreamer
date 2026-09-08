package fusemount

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
)

var ErrUnexpectedUnmount = errors.New("unexpected unmount, unmounted from external?")

const (
	// unmountRetryInterval is how often a busy mount is asked again while it
	// waits. What holds a mount is a file another program has open, and the
	// reads behind those run over the network, so this is not a busy wait.
	unmountRetryInterval = 500 * time.Millisecond

	// unmountReserve is what the mount leaves of the shutdown budget for the
	// rest of the shutdown, by detaching that much before it is up. A shutdown
	// that runs out of time is killed where it stands, which leaves behind the
	// mountpoint that the detach exists to clear.
	unmountReserve = 5 * time.Second
)

func Setup(batchDelay time.Duration, narrowMissSize int64) *FileSystem {
	// Create root directory node
	root := &dirNode{
		modTime: time.Now(),
	}

	// Initialize filesystem
	return &FileSystem{root: root, batchDelay: batchDelay, narrowMissSize: narrowMissSize}
}

// Mount attaches the tree at path. The root inode only accepts children once it
// is mounted, so this happens before anything is added to the filesystem; Serve
// then runs until the context ends.
func (fsManager *FileSystem) Mount(path string, mountOptions []string, maxBackground, maxReadAhead int) error {
	server, err := fs.Mount(path, fsManager.root, &fs.Options{
		MountOptions: fuse.MountOptions{
			FsName:        "nzbstreamer",
			Name:          "nzbstreamer",
			DisableXAttrs: true,
			Options:       mountOptions,
			MaxBackground: maxBackground,
			MaxReadAhead:  maxReadAhead,
		},
	})
	if err != nil {
		return fmt.Errorf("failed mounting: %w", err)
	}
	slog.Info("Mounted", "path", path)

	fsManager.server = server
	fsManager.path = path
	fsManager.mounted.Store(true)
	return nil
}

// Serve runs until the context ends and then takes the mount down within what
// is left of shutdownBudget, which is the time the whole shutdown has.
func (fsManager *FileSystem) Serve(ctx context.Context, shutdownBudget time.Duration) error {
	server := fsManager.server
	defer fsManager.mounted.Store(false)

	mountWaitCtx := make(chan struct{})
	go func() {
		server.Wait()
		close(mountWaitCtx)
	}()

	select {
	case <-ctx.Done():
		slog.Debug("Context cancelled, unmounting")
		if err := fsManager.unmount(max(shutdownBudget-unmountReserve, 0)); err != nil {
			return fmt.Errorf("unmounting failed: %w", err)
		}
	case <-mountWaitCtx:
		return ErrUnexpectedUnmount
	}
	return nil
}

// unmount takes the mount down, waiting for whoever still holds a file open.
func (fsManager *FileSystem) unmount(grace time.Duration) error {
	return unmountWithin(grace, fsManager.server.Unmount, func() error {
		return detach(fsManager.path)
	})
}

// unmountWithin retries a busy unmount until grace is up and detaches the
// mountpoint if it never comes free.
func unmountWithin(grace time.Duration, unmount, detach func() error) error {
	deadline := time.Now().Add(grace)

	for {
		err := unmount()
		if err == nil {
			return nil
		}

		if time.Now().After(deadline) {
			slog.Warn("Mount is still busy, detaching it", "waited", grace, "error", err)
			return detach()
		}

		time.Sleep(unmountRetryInterval)
	}
}
