package RarFileResource

import (
	"io"

	"git.ruekov.eu/ruakij/nzbStreamer/pkg/resource"
	"github.com/Ruakij/rardecode/v2"
)

// RarFileResource is a utility type that allows using a byte-slice resource.
type RarFileResource struct {
	resources []resource.ReadSeekCloseableResource
	password  string
	filename  string
	size      int64
}

func NewRarFileResource(resources []resource.ReadSeekCloseableResource, password, filename string) *RarFileResource {
	return &RarFileResource{
		resources: resources,
		password:  password,
		filename:  filename,
		size:      -1,
	}
}

type RarFileResourceReader struct {
	resource      *RarFileResource
	openResources []io.Reader
	rarReader     *rardecode.Reader
	index         int64
}

func (m *RarFileResource) Open() (io.ReadSeekCloser, error) {
	// Open all
	openResources := make([]io.Reader, len(m.resources))
	for i, resource := range m.resources {
		reader, err := resource.Open()
		openResources[i] = reader

		if err != nil {
			return nil, err
		}
	}

	// Create RarReader
	rarReader, err := rardecode.NewMultiReader(openResources, rardecode.Password(m.password))
	if err != nil {
		return nil, err
	}

	fileheader, err := SkipToFile(rarReader, m.filename)
	if err != nil {
		return nil, err
	}
	m.size = fileheader.UnPackedSize

	return &RarFileResourceReader{
		resource:      m,
		openResources: openResources,
		rarReader:     rarReader,
		index:         0,
	}, nil
}

func (m *RarFileResource) GetRarFiles(limit int) ([]string, error) {
	// Open all
	openResources := make([]io.Reader, len(m.resources))
	for i, resource := range m.resources {
		reader, err := resource.Open()
		openResources[i] = reader

		if err != nil {
			return nil, err
		}
	}

	// Create RarReader
	rarReader, err := rardecode.NewMultiReader(openResources, rardecode.Password(m.password))
	if err != nil {
		return nil, err
	}

	names := make([]string, 0, 1)
	header, err := rarReader.Next()
	for err == nil {
		names = append(names, header.Name)
		if len(names) >= limit {
			return names, nil
		}
		header, err = rarReader.Next()
	}

	return names, nil
}

func (r *RarFileResource) Size() (int64, error) {
	if r.size < 0 {
		reader, err := r.Open()
		if err != nil {
			return 0, err
		}
		reader.Close()
	}

	return r.size, nil
}

func (r *RarFileResourceReader) Close() error {
	r.rarReader = nil
	return nil
}

func (r *RarFileResourceReader) Read(p []byte) (n int, err error) {
	if len(p) == 0 {
		return
	}

	n, err = r.rarReader.Read(p)

	r.index += int64(n)
	return
}

func (r *RarFileResourceReader) Seek(offset int64, whence int) (newIndex int64, err error) {
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
		r.rarReader, err = rardecode.NewMultiReader(r.openResources)
		if err != nil {
			return 0, err
		}

		_, err = SkipToFile(r.rarReader, r.resource.filename)
		if err != nil {
			return 0, err
		}

		r.index = 0
	}

	// Skip forwards
	n, err := discardBytes(r.rarReader, int(newIndex-r.index))
	if err != nil {
		return 0, err
	}
	if n != int(newIndex-r.index) {
		return 0, io.ErrUnexpectedEOF
	}

	r.index = newIndex
	return r.index, nil
}

func discardBytes(reader io.Reader, amountToDiscard int) (totalDiscarded int, err error) {
	bufferSize := 1 * 1024 * 1024
	buf := make([]byte, bufferSize)
	var n int

	for totalDiscarded < amountToDiscard {
		bytesToRead := bufferSize
		if amountToDiscard-totalDiscarded < bufferSize {
			bytesToRead = amountToDiscard - totalDiscarded
		}

		n, err = io.ReadFull(reader, buf[:bytesToRead])
		totalDiscarded += n

		if err != nil {
			return
		}
	}

	return
}

func SkipToFile(reader *rardecode.Reader, filename string) (fileheader *rardecode.FileHeader, err error) {
	for {
		fileheader, err = reader.Next()
		if err != nil {
			return
		}

		if fileheader.Name == filename {
			return
		}
	}
}
