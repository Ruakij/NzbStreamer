package fileresource

import (
	"fmt"
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

func (r *FileResource) Open() (io.ReadSeekCloser, error) {
	var reader io.ReadSeekCloser
	var err error
	if r.Options.Filesystem != nil {
		reader, err = r.Options.Filesystem.Open(r.Filepath)
	} else {
		reader, err = os.Open(r.Filepath)
	}

	if err != nil {
		return nil, fmt.Errorf("failed opening underlying resource: %w", err)
	}
	return reader, nil
}

func (r *FileResource) Size() (size int64, err error) {
	var file http.File
	if r.Options.Filesystem != nil {
		file, err = r.Options.Filesystem.Open(r.Filepath)
	} else {
		file, err = os.Open(r.Filepath)
	}
	if err != nil {
		return 0, fmt.Errorf("failed opening underlying resource: %w", err)
	}

	fileinfo, err := file.Stat()
	if err != nil {
		return 0, fmt.Errorf("failed getting fileinfo from underlying resource: %w", err)
	}

	return fileinfo.Size(), nil
}
