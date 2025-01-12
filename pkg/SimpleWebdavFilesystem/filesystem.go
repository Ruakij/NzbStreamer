package SimpleWebdavFilesystem

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/emersion/go-webdav"
)

var (
	ErrReadOnlyFilesystem = fmt.Errorf("read-only filesystem")
	ErrFileNotFound       = os.ErrNotExist
)

type Node struct {
	File     *simpleFile
	Parent   *Node
	Children map[string]*Node
}

type FS struct {
	root *Node
	mu   sync.RWMutex
}

// simpleFile now also implements os.FileInfo
type simpleFile struct {
	node     *Node
	fs       *FS
	openable Openable
	size     int64
	modTime  time.Time
	name     string
	isDir    bool
}

type Openable interface {
	Open() (io.ReadSeekCloser, error)
	Size() (int64, error)
}

func NewFS() *FS {
	root := &Node{
		File:     &simpleFile{name: "/", isDir: true},
		Children: make(map[string]*Node),
	}
	root.File.node = root

	fs := &FS{root: root}
	root.File.fs = fs
	return fs
}

// AddFile adds a new file node to the filesystem, creating necessary directories.
func (fs *FS) AddFile(path, filename string, modTime time.Time, openable Openable) error {
	fs.mu.Lock()
	defer fs.mu.Unlock()

	fullPath := filepath.Clean(filepath.Join(path, filename))
	dirPath := filepath.Dir(fullPath)
	parentNode, err := fs.ensurePath(dirPath, modTime)
	if err != nil {
		return err
	}

	if _, exists := parentNode.Children[filename]; exists {
		return fmt.Errorf("file %s already exists", fullPath)
	}

	newNode := &Node{
		Parent:   parentNode,
		Children: make(map[string]*Node),
	}
	newNode.File = &simpleFile{
		fs:       fs,
		node:     newNode,
		openable: openable,
		modTime:  modTime,
		name:     filename,
		isDir:    false,
	}
	parentNode.Children[filename] = newNode
	return nil
}

// RemoveFile removes a file node from the filesystem and cleans up empty directories.
func (fs *FS) RemoveFile(path string) error {
	fs.mu.Lock()
	defer fs.mu.Unlock()

	node, err := fs.pathWalker(path)
	if err != nil {
		return err
	}

	delete(node.Parent.Children, node.File.name)
	fs.cleanupEmptyDirs(node.Parent)
	return nil
}

// pathWalker starts from the root and uses relativePathWalker to traverse the tree.
func (fs *FS) pathWalker(path string) (*Node, error) {
	return fs.relativePathWalker(fs.root, path)
}

// relativePathWalker allows traversal starting at a given node and a relative path.
func (fs *FS) relativePathWalker(startNode *Node, path string) (*Node, error) {
	if path == "" || path == "/" {
		return startNode, nil
	}

	segments := strings.Split(strings.TrimLeft(path, "/"), "/")
	current := startNode
	for _, segment := range segments {
		next, exists := current.Children[segment]
		if !exists {
			return nil, ErrFileNotFound
		}
		current = next
	}
	return current, nil
}

// ensurePath ensures that the given directory path exists, creating directories as necessary.
func (fs *FS) ensurePath(dirPath string, modTime time.Time) (*Node, error) {
	if dirPath == "/" {
		return fs.root, nil
	}

	segments := strings.Split(strings.Trim(dirPath, "/"), "/")
	current := fs.root
	for _, segment := range segments {
		if _, exists := current.Children[segment]; !exists {
			newNode := &Node{
				Parent:   current,
				Children: make(map[string]*Node),
			}
			newNode.File = &simpleFile{
				fs:      fs,
				node:    newNode,
				name:    segment,
				isDir:   true,
				modTime: modTime,
			}
			current.Children[segment] = newNode
		}
		current = current.Children[segment]
	}
	return current, nil
}

// cleanupEmptyDirs recursively removes empty directories up the tree.
func (fs *FS) cleanupEmptyDirs(node *Node) {
	if node == nil || node == fs.root {
		return
	}

	if len(node.Children) == 0 && node.File.isDir {
		parent := node.Parent
		delete(parent.Children, node.File.name)
		fs.cleanupEmptyDirs(parent)
	}
}

func (fs *FS) Mkdir(ctx context.Context, name string) error {
	return ErrReadOnlyFilesystem
}

// Implement Open from the interface (adjusted to match the signature)
func (fs *FS) Open(ctx context.Context, name string) (io.ReadCloser, error) {
	fs.mu.RLock()
	defer fs.mu.RUnlock()

	node, err := fs.pathWalker(name)
	if err != nil {
		return nil, err
	}

	fileReader := &simpleFileReader{
		simpleFile: node.File,
		node:       node,
		fs:         fs,
	}

	slog.Debug("Open", "name", name)

	if !node.File.isDir {
		reader, err := node.File.openable.Open()
		if err != nil {
			return nil, err
		}
		fileReader.reader = reader
	}
	return fileReader, nil
}

// Implement Stat from the interface
func (fs *FS) Stat(ctx context.Context, name string) (*webdav.FileInfo, error) {
	fs.mu.RLock()
	defer fs.mu.RUnlock()

	node, err := fs.pathWalker(name)
	if err != nil {
		return nil, err
	}

	var mimeType string
	if !node.File.IsDir() {
		// Detect MIME type based on file extension
		ext := strings.ToLower(filepath.Ext(name))
		mimeType = mime.TypeByExtension(ext)
		if mimeType == "" {
			mimeType = "application/octet-stream"
		}
	}

	return &webdav.FileInfo{
		Path:     name,
		Size:     node.File.Size(),
		ModTime:  node.File.ModTime(),
		IsDir:    node.File.IsDir(),
		MIMEType: mimeType,
	}, nil
}

// Implement ReadDir from the interface
func (fs *FS) ReadDir(ctx context.Context, name string, recursive bool) ([]webdav.FileInfo, error) {
	fs.mu.RLock()
	defer fs.mu.RUnlock()

	node, err := fs.pathWalker(name)
	if err != nil {
		return nil, err
	}

	if !node.File.isDir {
		return nil, fmt.Errorf("%s is not a directory", node.File.name)
	}

	var entries []webdav.FileInfo
	for _, childNode := range node.Children {
		entries = append(entries, webdav.FileInfo{
			Path:    filepath.Join(name, childNode.File.Name()),
			Size:    childNode.File.Size(),
			ModTime: childNode.File.ModTime(),
			IsDir:   childNode.File.IsDir(),
		})

		if recursive && childNode.File.isDir {
			childEntries, err := fs.ReadDir(ctx, filepath.Join(name, childNode.File.Name()), true)
			if err != nil {
				return nil, err
			}
			entries = append(entries, childEntries...)
		}
	}
	return entries, nil
}

// Implement Create from the interface — note that it's read-only, hence no-op
func (fs *FS) Create(ctx context.Context, name string, body io.ReadCloser) (*webdav.FileInfo, bool, error) {
	return nil, false, ErrReadOnlyFilesystem
}

// Implement RemoveAll from the interface as no-op because it's read-only
func (fs *FS) RemoveAll(ctx context.Context, name string) error {
	return ErrReadOnlyFilesystem
}

// Implement Copy from the interface as no-op because it's read-only
func (fs *FS) Copy(ctx context.Context, name, dest string, options *webdav.CopyOptions) (bool, error) {
	return false, ErrReadOnlyFilesystem
}

// Implement Move from the interface as no-op because it's read-only
func (fs *FS) Move(ctx context.Context, name, dest string, options *webdav.MoveOptions) (bool, error) {
	return false, ErrReadOnlyFilesystem
}

// AddFile and RemoveFile remain unchanged
// pathWalker, relativePathWalker, ensurePath remain unchanged

// Implement simpleFileReader to accommodate new functionality
type simpleFileReader struct {
	simpleFile *simpleFile
	reader     io.ReadSeekCloser
	node       *Node
	fs         *FS
}

func (sf *simpleFileReader) Close() error {
	slog.Debug("Close", "name", sf.simpleFile.name)
	if sf.reader != nil {
		return sf.reader.Close()
	}
	return nil
}

func (sf *simpleFileReader) Read(p []byte) (n int, err error) {
	if sf.reader != nil {
		n, err = sf.reader.Read(p)
		if err != nil && err != io.EOF {
			slog.Error("Read error", "name", sf.simpleFile.name, "len(p)", len(p), "err", err)
		}
		return
	}
	return 0, ErrReadOnlyFilesystem
}

func (sf *simpleFileReader) Seek(offset int64, whence int) (int64, error) {
	// Special seek request to determine content size
	if offset == 0 && whence == io.SeekEnd {
		info, err := sf.Stat()
		if err != nil {
			return 0, err
		}
		return info.Size(), nil
	}

	slog.Debug("Seek", "name", sf.simpleFile.name, "offset", offset, "whence", whence)
	if sf.reader != nil {
		return sf.reader.Seek(offset, whence)
	}
	return 0, ErrReadOnlyFilesystem
}

func (sf *simpleFileReader) Write(p []byte) (n int, err error) {
	return 0, ErrReadOnlyFilesystem
}

func (sf *simpleFileReader) Readdir(count int) ([]os.FileInfo, error) {
	sf.fs.mu.RLock()
	defer sf.fs.mu.RUnlock()

	slog.Debug("Readdir", "name", sf.simpleFile.name)

	if !sf.simpleFile.isDir {
		return nil, fmt.Errorf("%s is not a directory", sf.simpleFile.name)
	}

	var entries []os.FileInfo

	for _, childNode := range sf.node.Children {
		entries = append(entries, childNode.File)
	}

	if count > 0 && len(entries) > count {
		return entries[:count], nil
	}
	return entries, nil
}

func (sf *simpleFileReader) Stat() (os.FileInfo, error) {
	slog.Debug("Stat", "name", sf.simpleFile.name)
	return sf.simpleFile, nil
}

func (sf *simpleFile) Name() string {
	return sf.name
}

func (sf *simpleFile) Size() int64 {
	if sf.isDir {
		return 0
	}
	size, _ := sf.openable.Size()
	return size
}

func (sf *simpleFile) Mode() os.FileMode {
	if sf.isDir {
		return os.ModeDir | 0444 // Directory, read-only
	}
	return 0444 // File, read-only
}

func (sf *simpleFile) ModTime() time.Time {
	return sf.modTime
}

func (sf *simpleFile) IsDir() bool {
	return sf.isDir
}

func (sf *simpleFile) Sys() interface{} {
	return nil
}
