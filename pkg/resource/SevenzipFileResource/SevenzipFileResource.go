package SevenzipFileResource

import (
	"errors"
	"io"
	"io/fs"

	"git.ruekov.eu/ruakij/nzbStreamer/pkg/ioFsOps"
	"git.ruekov.eu/ruakij/nzbStreamer/pkg/readerAtWrapper"
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

func (m *SevenzipFileResource) Open() (io.ReadSeekCloser, error) {
	reader, err := m.open()
	if err != nil {
		return nil, err
	}

	file, err := SkipToFile(reader.sevenzipReader, m.filename)
	if err != nil {
		return nil, err
	}
	reader.fileReader, err = file.Open()
	if err != nil {
		return nil, err
	}

	m.size = int64(file.UncompressedSize)

	return reader, nil
}

// Internal open, which gets the reader ready for basic operations
func (m *SevenzipFileResource) open() (*SevenzipFileResourceReader, error) {
	// Open all
	reader, err := m.resource.Open()
	if err != nil {
		return nil, err
	}
	size, err := m.resource.Size()
	if err != nil {
		return nil, err
	}

	// Create RarReader
	sevenzipReader, err := sevenzip.NewReaderWithPassword(
		readerAtWrapper.NewReadSeekerAt(reader),
		size,
		m.password,
	)
	if err != nil {
		return nil, err
	}

	return &SevenzipFileResourceReader{
		resource:       m,
		sevenzipReader: sevenzipReader,
		index:          0,
	}, nil
}

func (m *SevenzipFileResource) GetFiles() (map[string]fs.FileInfo, error) {
	reader, err := m.open()
	if err != nil {
		return nil, err
	}

	return ioFsOps.BuildFileList(reader.sevenzipReader, ".")
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

func (r *SevenzipFileResourceReader) Read(p []byte) (n int, err error) {
	if len(p) == 0 {
		return
	}

	n, err = r.fileReader.Read(p)
	r.index += int64(n)

	return
}

func (r *SevenzipFileResourceReader) Seek(offset int64, whence int) (newIndex int64, err error) {
	resourceSize, _ := r.resource.Size()

	switch whence {
	case io.SeekStart:
		newIndex = offset
	case io.SeekCurrent:
		newIndex = r.index + offset
	case io.SeekEnd:
		newIndex = resourceSize + offset
	default:
		return 0, resource.ErrInvalidSeek
	}

	// Seek to same pos we are at
	if newIndex == r.index {
		return r.index, nil
	}
	// Out of range
	if newIndex < 0 || newIndex > resourceSize {
		return 0, resource.ErrInvalidSeek
	}

	// We cannot actually seek, so seeking backwards is specially not natively supported
	if newIndex < r.index {
		// Just reopen the reader
		r.fileReader.Close()
		file, err := SkipToFile(r.sevenzipReader, r.resource.filename)
		if err != nil {
			return 0, err
		}
		r.fileReader, err = file.Open()
		if err != nil {
			return 0, err
		}

		r.index = 0
	}

	// Skip forwards
	n, err := io.CopyN(io.Discard, r.fileReader, newIndex-r.index)
	if err != nil {
		return 0, err
	}
	if n != newIndex-r.index {
		return 0, io.ErrUnexpectedEOF
	}

	r.index = newIndex
	return r.index, nil
}

func SkipToFile(reader *sevenzip.Reader, filename string) (*sevenzip.File, error) {
	for _, file := range reader.File {
		if file.Name == filename {
			return file, nil
		}
	}
	return nil, errors.New("File not found")
}
