package fusemount

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
)

func Setup() *FileSystem {
	// Create root directory node
	root := &dirNode{
		modTime: time.Now(),
	}

	// Initialize filesystem
	return &FileSystem{root: root}
}

func (fsManager *FileSystem) Mount(ctx context.Context, path string, mountOptions []string) (err error) {
	server, err := fs.Mount(path, fsManager.root, &fs.Options{
		MountOptions: fuse.MountOptions{
			FsName:           "nzbstreamer",
			Name:             "nzbstreamer",
			DisableXAttrs:    true,
			SyncRead:         true,
			Options:          mountOptions,
			MaxReadAhead:     0,
			DirectMountFlags: fuse.FOPEN_NONSEEKABLE,
		},
	})
	if err != nil {
		return err
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
		return fmt.Errorf("unexpected unmount, unmounted from external?")
	}
	return
}
