// Package rarfileresource serves one file out of a rar archive whose volumes
// arrive as resources.
package rarfileresource

import (
	"errors"
	"fmt"
	"io"
	"path"
	"strings"
	"sync"

	"github.com/nwaples/rardecode/v2"

	"git.ruekov.eu/ruakij/nzbStreamer/pkg/readeratwrapper"
	"git.ruekov.eu/ruakij/nzbStreamer/pkg/resource"
)

var ErrFileNotFound = errors.New("file not found")

// RarFileResource exposes one member of a rar archive as a resource. The archive
// itself is the ordered set of volume resources it is built from; an empty
// filename means the archive rather than a member inside it.
type RarFileResource struct {
	volumes  *volumeFS
	password string
	filename string

	mutex sync.Mutex
	rarFS *rardecode.RarFS
	size  int64
}

// NewRarFileResource builds a member resource. size is the members unpacked
// size, which its file header carries and the caller already holds from
// listing the archive; -1 means unknown and makes Size() open the archive to
// find out, which costs one segment per volume.
func NewRarFileResource(volumes []resource.ReadSeekCloseableResource, password, filename string, size int64) *RarFileResource {
	return &RarFileResource{
		volumes:  newVolumeFS(volumes),
		password: password,
		filename: filename,
		size:     size,
	}
}

// volumeBufferSize is how much rardecode reads from a volume at a time. Its
// default of 4 KiB would chop a 128 KiB read of a member into 32 trips down the
// stack; anything below the segment size costs no extra download, since the cache
// stores a whole segment either way.
const volumeBufferSize = 128 * 1024

// options configures rardecode to read through the volume resources.
//
// SkipCheck is deliberate: verifying a members checksum requires reading all of
// it, which is the one thing streaming exists to avoid, and the checksum wrapper
// is not seekable, so it would cost every stored member its direct addressing.
func (r *RarFileResource) options() []rardecode.Option {
	return []rardecode.Option{
		rardecode.FileSystem(r.volumes),
		rardecode.Password(r.password),
		rardecode.SkipCheck,
		rardecode.BufferSize(volumeBufferSize),
	}
}

// GetRarFiles lists up to limit members, or all of them when limit is not
// positive. It reads block headers only and stops once limit is reached, so a
// bounded listing touches no more volumes than it has to.
func (r *RarFileResource) GetRarFiles(limit int) ([]*rardecode.FileHeader, error) {
	reader, err := rardecode.OpenReader(firstVolumeName, r.options()...)
	if err != nil {
		return nil, fmt.Errorf("failed opening rar archive: %w", err)
	}
	defer reader.Close()

	headers := make([]*rardecode.FileHeader, 0, 1) // Expect at least 1 file
	for limit <= 0 || len(headers) < limit {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("failed getting fileheader from rar archive: %w", err)
		}
		if !header.IsDir {
			headers = append(headers, header)
		}
	}

	return headers, nil
}

// maxMemberReaders bounds how many independent paths into one member positional
// reads may open. A flat cap, sized for what a FUSE mount keeps in flight.
const maxMemberReaders = 8

func (r *RarFileResource) Open() (io.ReadSeekCloser, error) {
	reader, size, err := r.openMember()
	if err != nil {
		return nil, err
	}

	r.mutex.Lock()
	r.size = size
	r.mutex.Unlock()

	// A stored member is a byte range in the volumes, so rardecode hands back a
	// reader that seeks. A compressed one is a one-way decoder stream.
	seeker, ok := reader.(io.ReadSeekCloser)
	if !ok {
		return &decoderSeeker{resource: r, reader: reader, size: size}, nil
	}

	return &storedMember{
		storedReader: newStoredReader(r, seeker, size),
		pool:         readeratwrapper.NewPooledReadSeekerAt(r.openStoredReader, maxMemberReaders),
	}, nil
}

// storedMember is a handle on a stored member. Being addressable, it answers
// positional reads too, and does so from readers of their own so concurrent ones
// run in parallel rather than queueing behind a single position.
type storedMember struct {
	*storedReader
	pool *readeratwrapper.PooledReadSeekerAt
}

func (m *storedMember) ReadAt(p []byte, off int64) (int, error) {
	//nolint:wrapcheck // io.EOF has to reach the caller unwrapped
	return m.pool.ReadAt(p, off)
}

func (m *storedMember) Close() error {
	return errors.Join(m.pool.Close(), m.storedReader.Close())
}

// openStoredReader opens another independent path into a stored member, which is
// what the pool hands to a positional read.
func (r *RarFileResource) openStoredReader() (io.ReadSeekCloser, error) {
	reader, size, err := r.openStoredMember()
	if err != nil {
		return nil, err
	}

	return newStoredReader(r, reader, size), nil
}

var ErrMemberNotStored = errors.New("member is not stored")

// openStoredMember opens the member and reports its unpacked size, refusing one
// that only decodes forwards.
func (r *RarFileResource) openStoredMember() (io.ReadSeekCloser, int64, error) {
	reader, size, err := r.openMember()
	if err != nil {
		return nil, 0, err
	}

	seeker, ok := reader.(io.ReadSeekCloser)
	if !ok {
		//nolint:errcheck // Nothing to do with a failure of a reader we are rejecting
		reader.Close()
		return nil, 0, fmt.Errorf("%w: %s", ErrMemberNotStored, r.filename)
	}

	return seeker, size, nil
}

// openMember opens the member and reports its unpacked size.
func (r *RarFileResource) openMember() (io.ReadCloser, int64, error) {
	rarFS, err := r.archiveFS()
	if err != nil {
		return nil, 0, err
	}

	file, err := rarFS.Open(memberPath(r.filename))
	if errors.Is(err, rardecode.ErrSolidOpen) {
		return r.openSolidMember()
	}
	if err != nil {
		return nil, 0, fmt.Errorf("failed opening '%s' in rar archive: %w", r.filename, err)
	}

	info, err := file.Stat()
	if err != nil {
		file.Close()
		return nil, 0, fmt.Errorf("failed getting size of '%s': %w", r.filename, err)
	}
	return file, info.Size(), nil
}

// openSolidMember reaches a member whose decoder state is shared with the members
// before it, which can only be rebuilt by decoding all of them in order.
func (r *RarFileResource) openSolidMember() (io.ReadCloser, int64, error) {
	reader, err := rardecode.OpenReader(firstVolumeName, r.options()...)
	if err != nil {
		return nil, 0, fmt.Errorf("failed opening rar archive: %w", err)
	}

	for {
		header, err := reader.Next()
		if err != nil {
			reader.Close()
			if errors.Is(err, io.EOF) {
				return nil, 0, fmt.Errorf("%w: %s", ErrFileNotFound, r.filename)
			}
			return nil, 0, fmt.Errorf("failed getting fileheader from rar archive: %w", err)
		}
		if header.Name == r.filename {
			return &readCloser{Reader: reader, Closer: reader}, header.UnPackedSize, nil
		}
	}
}

// archiveFS builds the member index once. It walks the block headers of every
// volume, which is what makes a member directly addressable afterwards.
func (r *RarFileResource) archiveFS() (*rardecode.RarFS, error) {
	r.mutex.Lock()
	defer r.mutex.Unlock()

	if r.rarFS == nil {
		rarFS, err := rardecode.OpenFS(firstVolumeName, r.options()...)
		if err != nil {
			return nil, fmt.Errorf("failed reading rar archive: %w", err)
		}
		r.rarFS = rarFS
	}
	return r.rarFS, nil
}

// memberPath normalises a member name the way rardecode keys its file tree.
func memberPath(name string) string {
	return strings.TrimPrefix(path.Clean(name), "/")
}

func (r *RarFileResource) SizeHint() (int64, error) {
	// Without a member, report the total packed size of the volumes
	if r.filename == "" {
		var totalSize int64
		for i, volume := range r.volumes.volumes {
			size, err := volume.SizeHint()
			if err != nil {
				return 0, fmt.Errorf("failed getting size from underlying resource %d: %w", i, err)
			}
			totalSize += size
		}
		return totalSize, nil
	}

	r.mutex.Lock()
	size := r.size
	r.mutex.Unlock()
	if size >= 0 {
		return size, nil
	}

	reader, err := r.Open()
	if err != nil {
		return 0, fmt.Errorf("failed opening rar member: %w", err)
	}
	reader.Close()

	r.mutex.Lock()
	defer r.mutex.Unlock()
	return r.size, nil
}

// Size is exact once the members header has been read, since rar records the
// unpacked length there.
func (r *RarFileResource) Size() (int64, error) {
	if r.filename == "" {
		var totalSize int64
		for i, volume := range r.volumes.volumes {
			sized, ok := volume.(resource.Sized)
			if !ok {
				return 0, resource.ErrSizeNotExact
			}

			size, err := sized.Size()
			if err != nil {
				return 0, fmt.Errorf("failed getting size from underlying resource %d: %w", i, err)
			}
			totalSize += size
		}
		return totalSize, nil
	}

	r.mutex.Lock()
	defer r.mutex.Unlock()
	if r.size < 0 {
		return 0, resource.ErrSizeNotExact
	}

	return r.size, nil
}

type readCloser struct {
	io.Reader
	io.Closer
}

// decoderSeeker emulates seeking on a compressed member. Its decoder only runs
// forwards, so a backward seek reopens the member and decodes from zero again.
type decoderSeeker struct {
	resource *RarFileResource
	reader   io.ReadCloser
	size     int64
	index    int64
}

func (d *decoderSeeker) Read(p []byte) (int, error) {
	n, err := d.reader.Read(p)
	d.index += int64(n)
	return n, err
}

func (d *decoderSeeker) Close() error {
	return d.reader.Close()
}

func (d *decoderSeeker) Seek(offset int64, whence int) (int64, error) {
	var index int64
	switch whence {
	case io.SeekStart:
		index = offset
	case io.SeekCurrent:
		index = d.index + offset
	case io.SeekEnd:
		index = d.size + offset
	default:
		return 0, resource.ErrInvalidSeek
	}

	if index < 0 || index > d.size {
		return 0, resource.ErrInvalidSeek
	}
	if index == d.index {
		return index, nil
	}

	if index < d.index {
		reader, _, err := d.resource.openMember()
		if err != nil {
			return 0, err
		}
		d.reader.Close()
		d.reader = reader
		d.index = 0
	}

	skipped, err := io.CopyN(io.Discard, d.reader, index-d.index)
	d.index += skipped
	if err != nil {
		return 0, fmt.Errorf("failed skipping forwards in rar member: %w", err)
	}
	return d.index, nil
}
