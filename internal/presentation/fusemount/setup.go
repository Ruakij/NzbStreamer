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

func Setup() *FileSystem {
	// Create root directory node
	root := &dirNode{
		modTime: time.Now(),
	}

	// Initialize filesystem
	return &FileSystem{root: root}
}

func (fsManager *FileSystem) Mount(ctx context.Context, path string, mountOptions []string) error {
	server, err := fs.Mount(path, fsManager.root, &fs.Options{
		MountOptions: fuse.MountOptions{
			FsName:        "nzbstreamer",
			Name:          "nzbstreamer",
			DisableXAttrs: true,
			SyncRead:      true,
			Options:       mountOptions,
		},
	})
	if err != nil {
		return fmt.Errorf("failed mounting: %w", err)
	}
	slog.Info("Mounted", "path", path)

	mountWaitCtx := make(chan struct{})
	go func() {
		server.Wait()
		close(mountWaitCtx)
	}()

	select {
	case <-ctx.Done():
		slog.Debug("Context cancelled, unmounting")
		err = server.Unmount()
		if err != nil {
			return fmt.Errorf("unmounting failed: %w", err)
		}
	case <-mountWaitCtx:
		return ErrUnexpectedUnmount
	}
	return nil
}
