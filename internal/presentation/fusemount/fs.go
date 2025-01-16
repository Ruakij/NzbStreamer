package fusemount

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"syscall"
	"time"

	"git.ruekov.eu/ruakij/nzbStreamer/internal/presentation"
	"git.ruekov.eu/ruakij/nzbStreamer/pkg/readeratwrapper"
	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
)

// fileNode represents a file in the read-only filesystem.
type fileNode struct {
	fs.Inode
	modTime  time.Time
	openable presentation.Openable
	size     int64
}

type file struct {
	reader io.ReaderAt
}

// dirNode represents a directory in the filesystem.
type dirNode struct {
	fs.Inode
	modTime time.Time
}

var (
	_ = fs.InodeEmbedder((*fileNode)(nil))
	_ = fs.InodeEmbedder((*dirNode)(nil))
)

// ReadDir reads the directory and generates a directory stream.
func (n *dirNode) Readdir(ctx context.Context) (fs.DirStream, syscall.Errno) {
	children := n.Children()
	r := make([]fuse.DirEntry, 0, len(children))
	for name, child := range children {
		mode := uint32(fuse.S_IFDIR)
		if _, ok := child.Operations().(*fileNode); ok {
			mode = fuse.S_IFREG
		}
		r = append(r, fuse.DirEntry{
			Name: name,
			Mode: mode,
			Ino:  child.StableAttr().Ino,
		})
	}
	return fs.NewListDirStream(r), 0
}

var _ = fs.NodeLookuper((*dirNode)(nil))

// Lookup finds the child specified by name in the current directory node.
func (n *dirNode) Lookup(ctx context.Context, name string, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	if child := n.GetChild(name); child != nil {
		return child, 0
	}
	return nil, syscall.ENOENT
}

var _ = fs.NodeGetattrer((*dirNode)(nil))

func (n *dirNode) Getattr(ctx context.Context, f fs.FileHandle, out *fuse.AttrOut) syscall.Errno {
	out.Ino = n.StableAttr().Ino

	modTime, modTimeNs, err := convertTimeToFuseAttr(n.modTime)
	if err != nil {
		return syscall.EINVAL
	}

	out.Ctime = modTime
	out.Ctimensec = modTimeNs
	out.Mtime = modTime
	out.Mtimensec = modTimeNs
	out.Atime = modTime
	out.Atimensec = modTimeNs
	return 0
}

var _ = fs.NodeGetattrer((*fileNode)(nil))

func (n *fileNode) Getattr(ctx context.Context, f fs.FileHandle, out *fuse.AttrOut) syscall.Errno {
	size, err := n.openable.Size()
	if err != nil {
		return syscall.EIO
	}
	out.Ino = n.StableAttr().Ino

	if size < 0 {
		// size not convertable to fuse-size
		return syscall.EIO
	}
	out.Size = uint64(size)

	modTime, modTimeNs, err := convertTimeToFuseAttr(n.modTime)
	if err != nil {
		return syscall.EINVAL
	}

	out.Ctime = modTime
	out.Ctimensec = modTimeNs
	out.Mtime = modTime
	out.Mtimensec = modTimeNs
	out.Atime = modTime
	out.Atimensec = modTimeNs
	return 0
}

var ErrTimeNotConvertableToFuseTime = errors.New("time is not convertable to fuse-time")

// Safely converts time (like modTime) to fuse time and timensec attributes
func convertTimeToFuseAttr(t time.Time) (time uint64, timeNs uint32, err error) {
	unixTime := t.Unix()
	if unixTime < 0 {
		return 0, 0, ErrTimeNotConvertableToFuseTime
	}
	time = uint64(unixTime)

	modTimeNs := t.Nanosecond()
	if modTimeNs < 0 || modTimeNs > int(^uint32(0)) {
		return 0, 0, ErrTimeNotConvertableToFuseTime
	}
	timeNs = uint32(modTimeNs)

	return time, timeNs, nil
}

var _ = fs.NodeOpener((*fileNode)(nil))

func (n *fileNode) Open(ctx context.Context, flags uint32) (fs.FileHandle, uint32, syscall.Errno) {
	reader, err := n.openable.Open()
	if err != nil {
		slog.Error("Error opening file", "error", err)
		return nil, 0, syscall.EIO
	}

	fh := file{
		reader: readeratwrapper.NewReadSeekerAt(reader),
	}

	return fh, 0, syscall.F_OK
}

type readResult struct {
	buf []byte
	n   int
	err int32
}

var _ = fuse.ReadResult((*readResult)(nil))

func (r *readResult) Bytes(buf []byte) ([]byte, fuse.Status) {
	return r.buf, fuse.Status(r.err)
}

func (r *readResult) Size() int {
	return r.n
}
func (r *readResult) Done() {}

var _ = fs.NodeReader((*fileNode)(nil))

func (n *fileNode) Read(ctx context.Context, f fs.FileHandle, dest []byte, off int64) (fuse.ReadResult, syscall.Errno) {
	file, ok := f.(file)
	if !ok {
		slog.Error("Error reading, invalid filehandle", "handle", f, "len", len(dest), "offset", off)
		return nil, syscall.EINVAL // Invalid argument error
	}
	return file.Read(ctx, dest, off)
}

var _ = fs.FileReader((*file)(nil))

func (f *file) Read(ctx context.Context, dest []byte, off int64) (fuse.ReadResult, syscall.Errno) {
	n, err := f.reader.ReadAt(dest, off)
	if err != nil && err != io.EOF {
		slog.Error("Error reading", "handle", f, "len", len(dest), "offset", off, "error", err)
		return nil, syscall.EIO
	}

	return &readResult{
		buf: dest[:n],
		n:   n,
		err: 0,
	}, 0
}

// FileSystem manages the root directory and dynamic file modifications.
type FileSystem struct {
	root *dirNode
}

var _ = presentation.Presenter((*FileSystem)(nil))

func (fsManager *FileSystem) AddFile(fullpath string, modTime time.Time, openable presentation.Openable) error {
	size, err := openable.Size()
	if err != nil {
		return fmt.Errorf("adding file failed: %w", err)
	}

	parts := strings.Split(strings.Trim(fullpath, "/"), "/")
	currentInode := &fsManager.root.Inode

	// Ensure path exists
	for _, part := range parts[:len(parts)-1] {
		nextInode := currentInode.GetChild(part)
		if nextInode == nil {
			newDir := &dirNode{
				modTime: modTime,
			}
			nextInode = currentInode.NewInode(context.Background(), newDir, fs.StableAttr{Mode: fuse.S_IFDIR})
			currentInode.AddChild(part, nextInode, true)
		}
		currentInode = nextInode
	}

	// Add file
	fileName := parts[len(parts)-1]
	file := &fileNode{modTime: modTime, openable: openable, size: size}
	fileInode := currentInode.NewInode(context.Background(), file, fs.StableAttr{Mode: fuse.S_IFREG})
	currentInode.AddChild(fileName, fileInode, true)
	return nil
}

const DirSuffixCount = 2

func (fsManager *FileSystem) RemoveFile(fullpath string) error {
	parts := strings.Split(strings.Trim(fullpath, "/"), "/")
	currentInode := &fsManager.root.Inode

	// Traverse to the file's directory
	for _, part := range parts[:len(parts)-1] {
		nextInode := currentInode.GetChild(part)
		if nextInode == nil {
			return syscall.ENOENT
		}
		currentInode = nextInode
	}

	// Remove the file
	fileName := parts[len(parts)-1]
	currentInode.RmChild(fileName)

	// Remove empty directories
	for i := len(parts) - DirSuffixCount; i >= 0; i-- {
		parentParts := parts[:i]
		parentInode := &fsManager.root.Inode

		if len(parentParts) > 0 {
			var err error
			targetPath := strings.Join(parentParts, "/")
			parentInode, err = pathWalker(fsManager.root.Root(), targetPath)
			if err != nil {
				return fmt.Errorf("failed walking path from %s to %s: %w", fsManager.root, targetPath, err)
			}
		}

		childName := parts[i]
		currentInode = parentInode.GetChild(childName)

		if currentInode != nil && len(currentInode.Children()) == 0 {
			parentInode.RmChild(childName)
		} else {
			break // Stop if we find a non-empty directory
		}
	}

	return nil
}

var ErrPathNotFound = errors.New("path not found")

// pathWalker tries to resolve the given path starting from the provided inode.
func pathWalker(startInode *fs.Inode, path string) (*fs.Inode, error) {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	currentInode := startInode

	for _, part := range parts {
		if nextInode := currentInode.GetChild(part); nextInode != nil {
			currentInode = nextInode
		} else {
			return nil, ErrPathNotFound
		}
	}

	return currentInode, nil
}
