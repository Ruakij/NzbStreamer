package SimpleWebdavFilesystem

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/webdav"
)

var (
	ErrReadOnlyFilesystem = fmt.Errorf("read-only filesystem")
	ErrFileNotFound       = fmt.Errorf("file not found")
)

type FS struct {
	files map[string]*simpleFile
	mu    sync.RWMutex
}

type Openable interface {
	Open() (io.ReadSeekCloser, error)
	Size() (int64, error)
}

func NewFS() *FS {
	files := make(map[string]*simpleFile)

	return &FS{
		files: files,
	}
}

func (fs *FS) Mkdir(ctx context.Context, name string, perm os.FileMode) error {
	return ErrReadOnlyFilesystem
}

func (fs *FS) OpenFile(ctx context.Context, name string, flag int, perm os.FileMode) (webdav.File, error) {
	fs.mu.RLock()
	defer fs.mu.RUnlock()

	if simpleFile, exists := fs.files[name]; exists {
		if simpleFile.isDir {
			return &simpleFileReader{
				simpleFile: simpleFile,
				fs:         fs,
			}, nil
		}

		reader, err := simpleFile.openable.Open()
		if err != nil {
			return nil, err
		}

		fileReader := &simpleFileReader{
			simpleFile: simpleFile,
			reader:     reader,
			fs:         fs,
		}
		return fileReader, nil
	}

	if fs.isDir(name) {
		// Return a directory without opening a reader
		return &simpleFileReader{
			simpleFile: &simpleFile{name: name, isDir: true},
			fs:         fs,
		}, nil
	}

	return nil, ErrFileNotFound
}

func (fs *FS) RemoveAll(ctx context.Context, name string) error {
	return ErrReadOnlyFilesystem
}

func (fs *FS) Rename(ctx context.Context, oldName, newName string) error {
	return ErrReadOnlyFilesystem
}

func (fs *FS) Stat(ctx context.Context, name string) (os.FileInfo, error) {
	fs.mu.RLock()
	defer fs.mu.RUnlock()

	fmt.Printf("Stat\t%s\n", name)

	if f, exists := fs.files[name]; exists {
		return f, nil
	}

	if fs.isDir(name) {
		return &simpleFile{name: name, isDir: true}, nil
	}

	return nil, ErrFileNotFound
}

func (fs *FS) AddFile(path, filename string, openable Openable) error {
	fullPath := filepath.Join(path, filename)

	fs.mu.Lock()
	defer fs.mu.Unlock()

	// Add necessary directories
	fs.addDirectory(path)

	size, err := openable.Size()
	if err != nil {
		return err
	}

	// Add the actual file
	fs.files[fullPath] = &simpleFile{
		openable: openable,
		size:     size,
		modTime:  time.Now(),
		name:     fullPath,
		isDir:    false,
	}

	return nil
}

func (fs *FS) RemoveFile(path string) error {
	fs.mu.Lock()
	defer fs.mu.Unlock()

	if _, exists := fs.files[path]; !exists {
		return ErrFileNotFound
	}

	delete(fs.files, path)

	// Remove empty directories
	// Walk up the directory structure and remove any empty directories
	dir := filepath.Dir(path)
	for dir != "." && dir != string(filepath.Separator) {
		if fs.isDirectoryEmpty(dir) {
			delete(fs.files, dir)
			dir = filepath.Dir(dir)
		} else {
			break
		}
	}

	return nil
}

func (fs *FS) addDirectory(path string) {
	// Ensure all directories in the path exist
	currentPath := ""
	for _, dir := range strings.Split(path, string(filepath.Separator)) {
		if dir == "" {
			continue
		}
		currentPath = filepath.Join(currentPath, dir)
		if _, exists := fs.files[currentPath]; !exists {
			fs.files[currentPath] = &simpleFile{
				isDir: true,
				name:  currentPath,
			}
		}
	}
}

func (fs *FS) isDirectoryEmpty(path string) bool {
	for p, file := range fs.files {
		if file.isDir {
			continue
		}
		if strings.HasPrefix(p, filepath.Join(path, string(filepath.Separator))) {
			return false
		}
	}
	return true
}

func (fs *FS) isDir(path string) bool {
	if !strings.HasSuffix(path, "/") {
		path += "/"
	}
	if path == "/" {
		return true
	}
	for filePath := range fs.files {
		if strings.HasPrefix(filePath, path) {
			return true
		}
	}
	return false
}

type simpleFileReader struct {
	simpleFile *simpleFile
	reader     io.ReadSeekCloser
	fs         *FS
}

func (sf *simpleFileReader) Close() error {
	if sf.reader != nil {
		return sf.reader.Close()
	}
	return nil
}

func (sf *simpleFileReader) Read(p []byte) (n int, err error) {
	if sf.reader != nil {
		return sf.reader.Read(p)
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

	fmt.Printf("%s\tSeek(%d, %d)\n", sf.simpleFile.name, offset, whence)
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

	if !sf.simpleFile.IsDir() {
		return nil, fmt.Errorf("%s is not a directory", sf.simpleFile.Name())
	}

	prefix := sf.simpleFile.Name()
	if !strings.HasSuffix(prefix, "/") {
		prefix += "/"
	}

	entries := make([]os.FileInfo, 0)
	seen := make(map[string]struct{})

	for filePath, f := range sf.fs.files {
		if strings.HasPrefix(filePath, prefix) {
			relPath := strings.TrimPrefix(filePath, prefix)
			if parts := strings.SplitN(relPath, "/", 2); len(parts) == 1 {
				// This entry is in the current directory
				name := parts[0]
				if _, exists := seen[name]; !exists {
					seen[name] = struct{}{}
					entry := &simpleFile{
						name:  filepath.Join(prefix, name),
						isDir: f.isDir,
					}
					entries = append(entries, entry)
				}
			}
		}
	}

	if count > 0 && len(entries) > count {
		return entries[:count], nil
	}
	return entries, nil
}

func (sf *simpleFileReader) Stat() (os.FileInfo, error) {
	return sf.simpleFile, nil
}

type simpleFile struct {
	openable Openable
	size     int64
	modTime  time.Time
	name     string
	isDir    bool
}

func (sf *simpleFile) Name() string {
	return sf.name
}

func (sf *simpleFile) Size() int64 {
	if sf.isDir {
		return 0
	}
	return sf.size
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
