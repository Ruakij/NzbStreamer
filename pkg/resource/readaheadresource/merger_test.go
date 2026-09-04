package readaheadresource_test

import (
	"bytes"
	"errors"
	"io"
	"math/rand"
	"testing"

	"git.ruekov.eu/ruakij/nzbStreamer/pkg/diskcache"
	"git.ruekov.eu/ruakij/nzbStreamer/pkg/resource"
	"git.ruekov.eu/ruakij/nzbStreamer/pkg/resource/adaptiveparallelmergerresource"
	"git.ruekov.eu/ruakij/nzbStreamer/pkg/resource/fullcacheresource"
	"git.ruekov.eu/ruakij/nzbStreamer/pkg/resource/readaheadresource"
)

// post serves fixed content and over-reports its size, the way an nzb hint does.
type post struct{ content []byte }

func (p *post) Open() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(p.content)), nil }
func (p *post) SizeHint() (int64, error)     { return int64(len(p.content)) + 37, nil }

func windowOverMerger(t *testing.T) (io.ReadSeekCloser, []byte) {
	t.Helper()

	cache, err := diskcache.NewCache(&diskcache.CacheOptions{CacheDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}

	rng := rand.New(rand.NewSource(1))
	var want []byte
	var parts []resource.ReadSeekCloseableResource
	for i := range 40 {
		part := make([]byte, 100+rng.Intn(400))
		rng.Read(part)
		want = append(want, part...)
		parts = append(parts, fullcacheresource.NewFullCacheResource(
			&post{content: part},
			diskcache.Key{"nzb", string(rune('a' + i))},
			cache,
			&fullcacheresource.FullCacheResourceOptions{},
		))
	}

	reader, err := readaheadresource.New(adaptiveparallelmergerresource.NewAdaptiveParallelMergerResource(parts), 2048, 512).Open()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { reader.Close() })

	return reader, want
}

func TestWindowOverMerger(t *testing.T) {
	reader, want := windowOverMerger(t)

	got, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("ReadAll = %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("ReadAll: %d bytes, want %d", len(got), len(want))
	}
}

// A Read that returns bytes reports no end, whatever io.ReaderAt does: rardecode
// keeps the error of a filled read and carries it into the next volume.
func TestReadReportsEndOnlyWhenEmpty(t *testing.T) {
	reader, want := windowOverMerger(t)

	buffer := make([]byte, len(want))
	for read := 0; ; {
		n, err := reader.Read(buffer)
		if errors.Is(err, io.EOF) {
			if n != 0 {
				t.Fatalf("Read = %d bytes with io.EOF", n)
			}
			if read != len(want) {
				t.Fatalf("read %d bytes, want %d", read, len(want))
			}
			return
		}
		if err != nil {
			t.Fatalf("Read = %v", err)
		}
		read += n
	}
}
