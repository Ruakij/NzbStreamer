package MergerResource

import (
	"git.ruekov.eu/ruakij/nzbStreamer/pkg/resource"
	"io"
)

// MergerResource is a Resource type which allows combining multiple Resources as if it was one
type MergerResource struct {
	resources []resource.ReadSeekCloseableResource
}

func NewMergerResource(resources []resource.ReadSeekCloseableResource) *MergerResource {
	return &MergerResource{
		resources: resources,
	}
}

type MergerResourceReader struct {
	resource *MergerResource
	readers  []io.ReadSeekCloser
	// Position in data
	index int64
	// Active reader index
	readerIndex int
	// Active reader byte index
	readerByteIndex int64
}

// Open will also eagerly open all underlying Resources
func (mrm *MergerResource) Open() (reader io.ReadSeekCloser, err error) {
	readers := make([]io.ReadSeekCloser, len(mrm.resources), len(mrm.resources))
	for i, resource := range mrm.resources {
		readers[i], err = resource.Open()
		if err != nil {
			return
		}
	}

	reader = &MergerResourceReader{
		resource: mrm,
		readers:  readers,
		index:    0,
	}
	return
}

func (mrm *MergerResource) Size() (int64, error) {
	var totalSize int64
	for _, resource := range mrm.resources {
		size, err := resource.Size()
		if err != nil {
			return int64(totalSize), err
		}

		totalSize += size
	}
	return totalSize, nil
}

func (mrmr *MergerResourceReader) Read(p []byte) (totalRead int, err error) {
	if len(p) == 0 {
		return
	}

	for mrmr.readerIndex < len(mrmr.readers) {
		var readerRead int
		readerRead, err = mrmr.readers[mrmr.readerIndex].Read(p[totalRead:])
		totalRead += readerRead
		mrmr.index += int64(readerRead)
		mrmr.readerByteIndex += int64(readerRead)

		// If buffer p is full, return
		if totalRead == len(p) {
			return totalRead, nil
		}

		// Erro-check, ignore EOF in the middle
		if err != nil && err != io.EOF {
			return
		}

		mrmr.readerIndex++
	}

	// All readers exausted, EOF
	return totalRead, io.EOF
}

func (mrmr *MergerResourceReader) Close() (err error) {
	for _, reader := range mrmr.readers {
		err = reader.Close()
		if err != nil {
			return
		}
	}
	return
}

func (r *MergerResourceReader) Seek(offset int64, whence int) (newIndex int64, err error) {

	switch whence {
	case io.SeekStart:
		newIndex = offset
	case io.SeekCurrent:
		newIndex = r.index + offset
	case io.SeekEnd:
		resourceSize, err := r.resource.Size()
		if err != nil {
			return 0, err
		}

		newIndex = resourceSize - offset
	}

	// Seek to same pos we are at
	if newIndex == r.index {
		return r.index, nil
	}
	// Out of range
	if newIndex < 0 {
		return 0, resource.ErrInvalidSeek
	}

	// Find affected sub-reader
	readerIndex, readerByteIndex, err := r.getReaderIndexAndByteIndexAtByteIndex(newIndex)
	if err != nil {
		if err != io.ErrUnexpectedEOF {
			return
		}
		// Unexpected EOF here just means we couldnt get a reader, because newIndex is behind everything
		// Set reader to after last
		err = nil
		readerIndex = len(r.readers)
	}

	if readerIndex < len(r.readers) {
		// Seek to position in reader
		r.readers[readerIndex].Seek(readerByteIndex, io.SeekStart)
		// Seek to start for all following readers
		for i := readerIndex + 1; i < len(r.readers); i++ {
			r.readers[i].Seek(0, io.SeekStart)
		}
	}

	r.readerIndex = readerIndex
	r.index = newIndex
	return
}

func (mrmr *MergerResourceReader) getReaderIndexAndByteIndexAtByteIndex(index int64) (int, int64, error) {
	var byteIndex int64 = 0
	for readerIndex := range mrmr.readers {
		resource := mrmr.resource.resources[readerIndex]
		size, err := resource.Size()
		if err != nil {
			return 0, 0, err
		}

		if index <= byteIndex+size {
			byteIndex = index - byteIndex
			return readerIndex, byteIndex, nil
		}
		byteIndex += size
	}
	// Arriving here means our byteIndex is higher than the actual data
	return 0, 0, io.ErrUnexpectedEOF
}
