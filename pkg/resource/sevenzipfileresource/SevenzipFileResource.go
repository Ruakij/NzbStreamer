package sevenzipfileresource

import (
	"errors"
	"fmt"
	"io"
	"io/fs"

	"git.ruekov.eu/ruakij/nzbStreamer/pkg/iofsops"
	"git.ruekov.eu/ruakij/nzbStreamer/pkg/readeratwrapper"
	"git.ruekov.eu/ruakij/nzbStreamer/pkg/resource"
	"github.com/bodgit/sevenzip"
)

// SevenzipFileResource is a utility type that allows using a byte-slice resource.
type SevenzipFileResource struct {
	resource resource.ReadSeekCloseableResource
	password string
	filename string
	size     int64
}

func NewSevenzipFileResource(resource resource.ReadSeekCloseableResource, password, filename string) *SevenzipFileResource {
	return &SevenzipFileResource{
		resource: resource,
		password: password,
		filename: filename,
		size:     -1,
	}
}

type SevenzipFileResourceReader struct {
	resource       *SevenzipFileResource
	sevenzipReader *sevenzip.Reader
	fileReader     io.ReadCloser
	index          int64
}

func (r *SevenzipFileResource) Open() (io.ReadSeekCloser, error) {
	reader, err := r.open()
	if err != nil {
		return nil, err
	}

	file, err := skipToFile(reader.sevenzipReader, r.filename)
	if err != nil {
		return nil, err
	}
	reader.fileReader, err = file.Open()
	if err != nil {
		return nil, fmt.Errorf("failed opening 7z file: %w", err)
	}

	r.size = int64(file.UncompressedSize)

	return reader, nil
}

// Internal open, which gets the reader ready for basic operations
func (r *SevenzipFileResource) open() (*SevenzipFileResourceReader, error) {
	// Open all
	reader, err := r.resource.Open()
	if err != nil {
		return nil, fmt.Errorf("failed opening underlying resource: %w", err)
	}
	size, err := r.resource.Size()
	if err != nil {
		return nil, fmt.Errorf("failed getting size from underlying resource: %w", err)
	}

	// Create RarReader
	sevenzipReader, err := sevenzip.NewReaderWithPassword(
		readeratwrapper.NewReadSeekerAt(reader),
		size,
		r.password,
	)
	if err != nil {
		return nil, fmt.Errorf("failed creating new 7z reader: %w", err)
	}

	return &SevenzipFileResourceReader{
		resource:       r,
		sevenzipReader: sevenzipReader,
		index:          0,
	}, nil
}

func (r *SevenzipFileResource) GetFiles() (map[string]fs.FileInfo, error) {
	reader, err := r.open()
	if err != nil {
		return nil, err
	}

	fileInfos, err := iofsops.BuildFileList(reader.sevenzipReader, ".")
	if err != nil {
		return nil, fmt.Errorf("failed building filelist from 7z: %w", err)
	}
	return fileInfos, nil
}

func (r *SevenzipFileResource) Size() (int64, error) {
	if r.size < 0 {
		reader, err := r.Open()
		if err != nil {
			return 0, err
		}
		reader.Close()
	}

	return r.size, nil
}

func (r *SevenzipFileResourceReader) Close() error {
	r.sevenzipReader = nil
	return nil
}

func (r *SevenzipFileResourceReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}

	n, err := r.fileReader.Read(p)
	r.index += int64(n)

	return n, err
}

func (r *SevenzipFileResourceReader) Seek(offset int64, whence int) (newIndex int64, err error) {
	switch whence {
	case io.SeekStart:
		newIndex = offset
	case io.SeekCurrent:
		newIndex = r.index + offset
	case io.SeekEnd:
		newIndex = r.resource.size + offset
	default:
		return 0, resource.ErrInvalidSeek
	}

	// Seek to same pos we are at
	if newIndex == r.index {
		return r.index, nil
	}
	// Out of range
	if newIndex < 0 || newIndex > r.resource.size {
		return 0, resource.ErrInvalidSeek
	}

	// We cannot actually seek, so seeking backwards is specially not natively supported
	if newIndex < r.index {
		// Just reopen the reader
		r.fileReader.Close()
		file, err := skipToFile(r.sevenzipReader, r.resource.filename)
		if err != nil {
			return 0, err
		}
		r.fileReader, err = file.Open()
		if err != nil {
			return 0, fmt.Errorf("failed reopening file: %w", err)
		}

		r.index = 0
	}

	// Skip forwards
	n, err := io.CopyN(io.Discard, r.fileReader, newIndex-r.index)
	if err != nil {
		return 0, fmt.Errorf("failed dicarding %d bytes forward: %w", newIndex-r.index, err)
	}
	if n != newIndex-r.index {
		return 0, io.ErrUnexpectedEOF
	}

	r.index = newIndex
	return r.index, nil
}

var ErrFileNotFound = errors.New("file not found")

func skipToFile(reader *sevenzip.Reader, filename string) (*sevenzip.File, error) {
	for _, file := range reader.File {
		if file.Name == filename {
			return file, nil
		}
	}
	return nil, ErrFileNotFound
}
