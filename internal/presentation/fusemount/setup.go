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
	fsManager.mounted.Store(true)
	return nil
}

func (fsManager *FileSystem) Serve(ctx context.Context) error {
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
		if err := server.Unmount(); err != nil {
			return fmt.Errorf("unmounting failed: %w", err)
		}
	case <-mountWaitCtx:
		return ErrUnexpectedUnmount
	}
	return nil
}
