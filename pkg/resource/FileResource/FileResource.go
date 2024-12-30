package FileResource

import (
	"io"
	"net/http"
	"os"
)

// FileResource allows using a file as Resource
type FileResource struct {
	Filepath string
	Options  FileResourceOptions
}

type FileResourceOptions struct {
	// If specified will use this filesystem to open file
	Filesystem http.FileSystem
}

func (r *FileResource) Open() (reader io.ReadSeekCloser, err error) {
	if r.Options.Filesystem != nil {
		reader, err = r.Options.Filesystem.Open(r.Filepath)
		return
	}

	reader, err = os.Open(r.Filepath)
	return
}

func (r *FileResource) Size() (size int64, err error) {
	var file http.File
	if r.Options.Filesystem != nil {
		file, err = r.Options.Filesystem.Open(r.Filepath)
	}

	file, err = os.Open(r.Filepath)
	if err != nil {
		return
	}

	fileinfo, err := file.Stat()
	if err != nil {
		return
	}

	fileinfo.Size()
	return
}
