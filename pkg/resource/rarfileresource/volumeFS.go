package rarfileresource

import (
	"fmt"
	"io"
	"io/fs"
	"time"

	"git.ruekov.eu/ruakij/nzbStreamer/pkg/resource"
)

// firstVolumeName is the name the archive is opened under. rardecode derives
// every following volume name from it by incrementing the digit run, so
// volumeName has to stay in step with it.
const firstVolumeName = "volume.part0001.rar"

func volumeName(index int) string {
	return fmt.Sprintf("volume.part%04d.rar", index+1)
}

// volumeFS presents an ordered set of rar volumes to rardecode, which addresses
// volumes by filename. The names are synthetic: the order of the resources is
// what identifies a volume, so the names an nzb happens to use - frequently
// obfuscated, and following no rar naming scheme at all - are never parsed here.
type volumeFS struct {
	volumes []resource.ReadSeekCloseableResource
	index   map[string]int
}

func newVolumeFS(volumes []resource.ReadSeekCloseableResource) *volumeFS {
	index := make(map[string]int, len(volumes))
	for i := range volumes {
		index[volumeName(i)] = i
	}
	return &volumeFS{volumes: volumes, index: index}
}

// Open returns a fresh reader for a volume. Asking past the last volume reports
// fs.ErrNotExist, which rardecode reads as the end of the archive.
func (v *volumeFS) Open(name string) (fs.File, error) {
	i, ok := v.index[name]
	if !ok {
		return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrNotExist}
	}

	reader, err := v.volumes[i].Open()
	if err != nil {
		return nil, &fs.PathError{Op: "open", Path: name, Err: err}
	}
	return &volumeFile{ReadSeekCloser: reader, resource: v.volumes[i], name: name}, nil
}

// volumeFile adapts a volume resource to fs.File. Seek is promoted from the
// embedded reader, which is what lets rardecode seek within the archive.
type volumeFile struct {
	io.ReadSeekCloser
	resource resource.ReadSeekCloseableResource
	name     string
}

func (f *volumeFile) Stat() (fs.FileInfo, error) {
	size, err := f.resource.SizeHint()
	if err != nil {
		return nil, fmt.Errorf("failed getting size of volume %s: %w", f.name, err)
	}
	return volumeFileInfo{name: f.name, size: size}, nil
}

type volumeFileInfo struct {
	name string
	size int64
}

func (i volumeFileInfo) Name() string       { return i.name }
func (i volumeFileInfo) Size() int64        { return i.size }
func (i volumeFileInfo) Mode() fs.FileMode  { return 0o444 }
func (i volumeFileInfo) ModTime() time.Time { return time.Time{} }
func (i volumeFileInfo) IsDir() bool        { return false }
func (i volumeFileInfo) Sys() any           { return nil }
