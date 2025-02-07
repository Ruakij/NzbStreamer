package rarfileresource

import (
	"errors"
	"fmt"
	"io"

	"git.ruekov.eu/ruakij/nzbStreamer/pkg/rardecode"
	"git.ruekov.eu/ruakij/nzbStreamer/pkg/resource"
	"golang.org/x/sync/errgroup"
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

func (r *RarFileResource) Open() (io.ReadSeekCloser, error) {
	reader, err := r.open()
	if err != nil {
		return nil, err
	}

	fileheader, err := skipToFile(reader.rarReader, r.filename)
	if err != nil {
		return nil, err
	}
	r.size = fileheader.UnPackedSize

	return reader, nil
}

func (r *RarFileResource) open() (*RarFileResourceReader, error) {
	// Open all
	openResources := make([]io.Reader, len(r.resources))
	for i, resource := range r.resources {
		reader, err := resource.Open()
		openResources[i] = reader

		if err != nil {
			return nil, fmt.Errorf("failed opening underlying resource %d: %w", i, err)
		}
	}

	// Create RarReader
	rarReader, err := rardecode.NewMultiReader(openResources, rardecode.Password(r.password))
	if err != nil {
		return nil, fmt.Errorf("failed opening rar reader: %w", err)
	}

	return &RarFileResourceReader{
		resource:      r,
		openResources: openResources,
		rarReader:     rarReader,
		index:         0,
	}, nil
}

func (r *RarFileResource) GetRarFiles(limit int) ([]*rardecode.FileHeader, error) {
	reader, err := r.open()
	if err != nil {
		return nil, err
	}

	fileheaders := make([]*rardecode.FileHeader, 0, 1) // Expect at least 1 file
	header, err := reader.rarReader.Next()
	if err != nil {
		return nil, fmt.Errorf("failed getting initial fileheader from rar reader: %w", err)
	}
	for err == nil {
		if header.IsDir {
			continue
		}

		fileheaders = append(fileheaders, header)
		if len(fileheaders) >= limit {
			break
		}
		header, err = reader.rarReader.Next()
	}

	return fileheaders, nil
}

func (r *RarFileResource) Size() (int64, error) {
	// If not filename specified, return total packed-size
	if r.filename == "" {
		var totalSize int64
		for i, resource := range r.resources {
			size, err := resource.Size()
			if err != nil {
				return size, fmt.Errorf("failed getting size from underlying resource %d: %w", i, err)
			}
			totalSize += size
		}
		return totalSize, nil
	}

	// If size unset, open reader for first time
	if r.size < 0 {
		reader, err := r.Open()
		if err != nil {
			return 0, fmt.Errorf("failed creating new multi reader: %w", err)
		}
		reader.Close()
	}

	return r.size, nil
}

func (r *RarFileResourceReader) Close() error {
	return nil
}

func (r *RarFileResourceReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}

	n, err := r.rarReader.Read(p)
	r.index += int64(n)

	return n, err
}

func (r *RarFileResourceReader) Seek(offset int64, whence int) (int64, error) {
	var newIndex int64

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
		group := errgroup.Group{}
		for i, reader := range r.openResources {
			readerIndex := i
			localReader := reader
			group.Go(func() (err error) {
				// Check if reader implements seeker
				if seeker, ok := localReader.(io.Seeker); ok {
					_, err = seeker.Seek(0, io.SeekStart)
					if err != nil {
						return fmt.Errorf("failed seeking resource %d: %w", readerIndex, err)
					}
				} else {
					// If it doesnt, reopen resource
					r.openResources[readerIndex], err = r.resource.resources[readerIndex].Open()
					if err != nil {
						return fmt.Errorf("failed reopening resource %d: %w", readerIndex, err)
					}
				}
				return nil
			})
		}
		err := group.Wait()
		if err != nil {
			return 0, fmt.Errorf("failed waiting for resource operations: %w", err)
		}

		r.rarReader, err = rardecode.NewMultiReader(r.openResources)
		if err != nil {
			return 0, err
		}

		_, err = skipToFile(r.rarReader, r.resource.filename)
		if err != nil {
			return 0, err
		}

		r.index = 0
	}

	delta := newIndex - r.index

	// Skip forwards
	// TODO: Move to library and also use in sevenzipresource
	var err error
	var n int
	var totalN int64
	buf := make([]byte, 16*1024*1024)
	for err == nil && totalN < delta {
		if totalN+int64(len(buf)) > delta {
			buf = buf[:delta-totalN]
		}
		n, err = r.rarReader.Read(buf)
		totalN += int64(n)
	}
	if err != nil {
		return 0, fmt.Errorf("failed skipping forwards in rar reader: %w", err)
	}
	if totalN != newIndex-r.index {
		return 0, io.ErrUnexpectedEOF
	}

	r.index = newIndex
	return r.index, nil
}

var ErrFileNotFound = errors.New("file not found")

func skipToFile(reader *rardecode.Reader, filename string) (*rardecode.FileHeader, error) {
	fileheader, err := reader.Next()
	if err != nil {
		return fileheader, fmt.Errorf("failed getting initial fileheader from rar reader: %w", err)
	}
	for err == nil {
		if fileheader.Name == filename {
			return fileheader, nil
		}

		fileheader, err = reader.Next()
	}
	return nil, ErrFileNotFound
}
