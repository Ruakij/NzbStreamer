// Package webdav presents the nzb tree over the WebDAV protocol, with
// optional basic auth.
package webdav

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"git.ruekov.eu/ruakij/nzbStreamer/internal/presentation"
	"github.com/emersion/go-webdav"
)

var (
	ErrReadOnlyFilesystem = errors.New("read-only filesystem")
	ErrFileNotFound       = os.ErrNotExist
)

type Node struct {
	File     *simpleFile
	Parent   *Node
	Children map[string]*Node
}

type FS struct {
	Root *Node
	// prefix the handler is mounted under, stripped where a path is looked up.
	// go-webdav builds both the lookup and the href of a response out of the
	// request path, so stripping it before the handler would answer the right
	// files under hrefs pointing at the server root.
	prefix string
	// lazyExactSize lets the seek ServeContent sizes a GET with reach the reader
	// where that is cheap, rather than answering every one from the hint.
	lazyExactSize bool
	handles       *handleCache
	mu            sync.RWMutex
}

// simpleFile now also implements os.FileInfo
type simpleFile struct {
	node     *Node
	fs       *FS
	openable presentation.Openable
	modTime  time.Time
	name     string
	isDir    bool
}

// NewFS builds the tree. idleTimeout is how long a finished reader is kept for
// the next Range request of the same file and maxIdleReaders how many are kept
// at once, since each one holds its readahead window; either at zero closes
// every reader with the request that opened it.
func NewFS(prefix string, lazyExactSize bool, idleTimeout time.Duration, maxIdleReaders int) *FS {
	root := &Node{
		File:     &simpleFile{name: "", isDir: true},
		Children: make(map[string]*Node),
	}
	root.File.node = root

	fs := &FS{
		Root:          root,
		prefix:        prefix,
		lazyExactSize: lazyExactSize,
		handles:       newHandleCache(idleTimeout, maxIdleReaders),
	}
	root.File.fs = fs
	return fs
}

var _ = presentation.Presenter((*FS)(nil))

// AddFile adds a new file node to the filesystem, creating necessary directories.
func (fs *FS) AddFile(fullPath string, modTime time.Time, openable presentation.Openable) error {
	fs.mu.Lock()
	defer fs.mu.Unlock()

	filename := path.Base(fullPath)
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

	if node.File.openable != nil {
		return fs.handles.discard(node.File.openable)
	}
	return nil
}

// requestPath turns the path of a request into a path in the tree.
func (fs *FS) requestPath(name string) string {
	return strings.TrimPrefix(name, fs.prefix)
}

// pathWalker starts from the root and uses relativePathWalker to traverse the tree.
func (fs *FS) pathWalker(path string) (*Node, error) {
	return fs.relativePathWalker(fs.Root, path)
}

// relativePathWalker allows traversal starting at a given node and a relative path.
func (fs *FS) relativePathWalker(startNode *Node, path string) (*Node, error) {
	if path == "" || path == "/" {
		return startNode, nil
	}

	segments := strings.Split(strings.Trim(path, "/"), "/")
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
		return fs.Root, nil
	}

	segments := strings.Split(strings.Trim(dirPath, "/"), "/")
	current := fs.Root
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
	if node == nil || node == fs.Root {
		return
	}

	if len(node.Children) == 0 && node.File.isDir {
		parent := node.Parent
		delete(parent.Children, node.File.name)
		fs.cleanupEmptyDirs(parent)
	}
}

func (fs *FS) Mkdir(_ context.Context, _ string) error {
	return ErrReadOnlyFilesystem
}

// Implement Open from the interface (adjusted to match the signature)
func (fs *FS) Open(_ context.Context, name string) (io.ReadCloser, error) {
	fs.mu.RLock()
	defer fs.mu.RUnlock()

	node, err := fs.pathWalker(fs.requestPath(name))
	if err != nil {
		return nil, webdav.NewHTTPError(http.StatusNotFound, err)
	}

	// The reader itself is taken on the first read or seek as reusing needs position.
	slog.Debug("Open", "name", name)

	return &simpleFileReader{simpleFile: node.File, node: node, fs: fs}, nil
}

// Implement Stat from the interface
func (fs *FS) Stat(_ context.Context, name string) (*webdav.FileInfo, error) {
	fs.mu.RLock()
	defer fs.mu.RUnlock()

	node, err := fs.pathWalker(fs.requestPath(name))
	if err != nil {
		return nil, webdav.NewHTTPError(http.StatusNotFound, err)
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

	node, err := fs.pathWalker(fs.requestPath(name))
	if err != nil {
		return nil, webdav.NewHTTPError(http.StatusNotFound, err)
	}

	if !node.File.isDir {
		return nil, fmt.Errorf("%s is not a directory", node.File.name)
	}

	var entries []webdav.FileInfo
	for _, childNode := range node.Children {
		entries = append(entries, webdav.FileInfo{
			// Path:    filepath.Join(name, childNode.File.Name()),
			Path:    childNode.File.Name(),
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
func (fs *FS) Create(_ context.Context, _ string, _ io.ReadCloser, _ *webdav.CreateOptions) (*webdav.FileInfo, bool, error) {
	return nil, false, ErrReadOnlyFilesystem
}

// Implement RemoveAll from the interface as no-op because it's read-only
func (fs *FS) RemoveAll(_ context.Context, _ string, _ *webdav.RemoveAllOptions) error {
	return ErrReadOnlyFilesystem
}

// Implement Copy from the interface as no-op because it's read-only
func (fs *FS) Copy(_ context.Context, _, _ string, _ *webdav.CopyOptions) (bool, error) {
	return false, ErrReadOnlyFilesystem
}

// Implement Move from the interface as no-op because it's read-only
func (fs *FS) Move(_ context.Context, _, _ string, _ *webdav.MoveOptions) (bool, error) {
	return false, ErrReadOnlyFilesystem
}

// AddFile and RemoveFile remain unchanged
// pathWalker, relativePathWalker, ensurePath remain unchanged

// simpleFileReader is one request's view of a file. It holds no reader until a
// read or a seek needs one, and hands it back to the cache on Close; position is
// what it wants next, which is both what picks a reader and what it is seeked to.
type simpleFileReader struct {
	simpleFile *simpleFile
	handle     *handle
	node       *Node
	fs         *FS
	position   int64
}

// reader takes a reader placed for position and seeks it there.
func (sf *simpleFileReader) reader() (io.ReadSeekCloser, error) {
	if sf.simpleFile.isDir {
		return nil, ErrReadOnlyFilesystem
	}

	if sf.handle == nil {
		h, err := sf.fs.handles.acquire(sf.simpleFile.openable, sf.position)
		if err != nil {
			return nil, err
		}
		sf.handle = h
	}
	if sf.handle.position != sf.position {
		if _, err := sf.handle.reader.Seek(sf.position, io.SeekStart); err != nil {
			return nil, fmt.Errorf("seek to %d: %w", sf.position, err)
		}
		sf.handle.position = sf.position
	}
	return sf.handle.reader, nil
}

func (sf *simpleFileReader) Close() error {
	slog.Debug("Close", "name", sf.simpleFile.name, "position", sf.position)
	if sf.handle == nil {
		return nil
	}

	h := sf.handle
	sf.handle = nil
	return sf.fs.handles.release(h)
}

func (sf *simpleFileReader) Read(p []byte) (int, error) {
	reader, err := sf.reader()
	if err != nil {
		return 0, err
	}

	n, err := reader.Read(p)
	sf.position += int64(n)
	servedBytes.Add(int64(n))
	sf.handle.position = sf.position
	if err != nil && !errors.Is(err, io.EOF) {
		slog.Error("Read error", "name", sf.simpleFile.name, "len(p)", len(p), "err", err)
	}
	return n, err
}

func (sf *simpleFileReader) Seek(offset int64, whence int) (int64, error) {
	slog.Debug("Seek", "name", sf.simpleFile.name, "offset", offset, "whence", whence)

	switch whence {
	case io.SeekStart:
		sf.position = offset
	case io.SeekCurrent:
		sf.position += offset
	case io.SeekEnd:
		size, err := sf.end()
		if err != nil {
			return 0, err
		}
		sf.position = size + offset
	default:
		return 0, os.ErrInvalid
	}

	if sf.position < 0 {
		return 0, os.ErrInvalid
	}
	return sf.position, nil
}

// end is where the file ends. ServeContent sizes every GET with a seek to it and
// Content-Length comes from that, so an addressable reader measures it for real.
// A decoder stream would decode the whole member to get there, so it answers
// from the hint.
func (sf *simpleFileReader) end() (int64, error) {
	if !sf.fs.lazyExactSize || sf.simpleFile.isDir {
		return sf.simpleFile.Size(), nil
	}

	reader, err := sf.reader()
	if err != nil {
		return 0, err
	}
	if _, addressable := reader.(io.ReaderAt); !addressable {
		return sf.simpleFile.Size(), nil
	}

	size, err := reader.Seek(0, io.SeekEnd)
	if err != nil {
		slog.Error("Seek error", "name", sf.simpleFile.name, "whence", io.SeekEnd, "err", err)
		return 0, err
	}
	sf.handle.position = size
	return size, nil
}

func (sf *simpleFileReader) Write(_ []byte) (n int, err error) {
	return 0, ErrReadOnlyFilesystem
}

func (sf *simpleFileReader) Readdir(count int) ([]os.FileInfo, error) {
	sf.fs.mu.RLock()
	defer sf.fs.mu.RUnlock()

	slog.Debug("Readdir", "reader", fmt.Sprintf("%p", sf.reader), "name", sf.simpleFile.name)

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
	slog.Debug("Stat", "reader", fmt.Sprintf("%p", sf.reader), "name", sf.simpleFile.name)
	return sf.simpleFile, nil
}

func (sf *simpleFile) Name() string {
	return sf.name
}

func (sf *simpleFile) Size() int64 {
	if sf.isDir {
		return 0
	}
	size, _ := sf.openable.SizeHint()
	return size
}

func (sf *simpleFile) Mode() os.FileMode {
	if sf.isDir {
		return os.ModeDir | 0o444 // Directory, read-only
	}
	return 0o444 // File, read-only
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
