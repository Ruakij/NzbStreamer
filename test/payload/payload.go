// Package payload generates the file contents the test news server is filled
// with. The bytes come from a seeded PRNG rather than a fixture, so a test can
// recompute what a read of any offset should have returned without the file
// existing anywhere near it.
package payload

import (
	"hash/fnv"
	"io"
	"os"
	"path/filepath"
	"strconv"
)

// DefaultSourceSize is the size of every source file. Big enough that the
// server, yenc, the archive and the cache are all exercised; tame enough that
// one rig does not fill the disk. Apex builds override it with SIZE_MB.
const DefaultSourceSize int64 = 256 << 20

// Size returns the source size the current run uses, SIZE_MB over the default.
func Size() int64 {
	if mb, err := strconv.ParseInt(os.Getenv("SIZE_MB"), 10, 64); err == nil && mb > 0 {
		return mb << 20
	}
	return DefaultSourceSize
}

// Source is one file posted to the server, before any archiving.
type Source struct {
	Name string
	Size int64
}

// Sources are the payloads every fixture set is built from.
func Sources() []Source {
	s := Size()
	return []Source{
		{"plain.mkv", s},
		{"movie.mkv", s},
	}
}

// Reader returns the bytes of the named source file as a stream. Same name,
// same bytes, on any machine and in any process; the whole file never has to
// be in memory at once, which is what lets a test hash a 256 MiB source
// without allocating it.
func Reader(name string, size int64) io.Reader {
	h := fnv.New64a()
	h.Write([]byte(name))
	return &reader{state: h.Sum64() | 1, remaining: size}
}

type reader struct {
	state     uint64
	remaining int64
}

func (r *reader) Read(p []byte) (int, error) {
	if r.remaining <= 0 {
		return 0, io.EOF
	}
	n := int64(len(p))
	if n > r.remaining {
		n = r.remaining
	}
	for i := int64(0); i < n; i++ {
		// xorshift64*, which is short enough to be obviously the same
		// everywhere and random enough that no archiver compresses it away
		r.state ^= r.state >> 12
		r.state ^= r.state << 25
		r.state ^= r.state >> 27
		p[i] = byte(r.state * 0x2545f4914f6cdd1d >> 56)
	}
	r.remaining -= n
	return int(n), nil
}

// Write puts every source file in dir, at the current runs size.
func Write(dir string) error {
	return WriteSized(dir, Size())
}

// WriteSized puts every source file in dir, at the given size.
func WriteSized(dir string, _ int64) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	for _, source := range Sources() {
		f, err := os.Create(filepath.Join(dir, source.Name))
		if err != nil {
			return err
		}
		if _, err := io.Copy(f, Reader(source.Name, source.Size)); err != nil {
			f.Close()
			return err
		}
		if err := f.Close(); err != nil {
			return err
		}
	}
	return nil
}
