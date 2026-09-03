//go:build live

// What building and opening an nzbs resource stack costs against a real news
// server. Run with the servers credentials in the environment and an nzb of a
// rar set:
//
//	set -a; . .compose-test/.env; set +a
//	NNTP_LIVE_NZB=$PWD/.compose-test/watch/some.nzb go test -tags live -v -timeout 20m ./internal/nzbrecordfactory/
package nzbrecordfactory

import (
	"io"
	"os"
	"sort"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"git.ruekov.eu/ruakij/nzbStreamer/internal/nntpclient"
	"git.ruekov.eu/ruakij/nzbStreamer/pkg/diskcache"
	"git.ruekov.eu/ruakij/nzbStreamer/pkg/nzbparser"
	"git.ruekov.eu/ruakij/nzbStreamer/pkg/resource/nzbpostresource"
)

// counter records what a stretch of work cost in segments off the wire, which is
// the only expensive thing any of it does.
type counter struct {
	fetches atomic.Int64
	bytes   atomic.Int64
	wait    atomic.Int64
}

func (c *counter) wrap(get nzbpostresource.GetSegmentFunc) nzbpostresource.GetSegmentFunc {
	return func(group, id string) ([]byte, error) {
		start := time.Now()
		body, err := get(group, id)
		c.wait.Add(int64(time.Since(start)))
		c.fetches.Add(1)
		c.bytes.Add(int64(len(body)))
		return body, err
	}
}

func (c *counter) reset() {
	c.fetches.Store(0)
	c.bytes.Store(0)
	c.wait.Store(0)
}

func (c *counter) report(t *testing.T, what string, elapsed time.Duration) {
	t.Helper()
	t.Logf("%-28s %v wall, %d segments, %d KiB, %v in fetches",
		what, elapsed.Round(time.Millisecond), c.fetches.Load(),
		c.bytes.Load()/1024, time.Duration(c.wait.Load()).Round(time.Millisecond))
}

func liveClient(t *testing.T) *nntpclient.Client {
	t.Helper()

	host := os.Getenv("USENET_HOST")
	if host == "" {
		t.Skip("USENET_HOST unset")
	}
	port, err := strconv.Atoi(os.Getenv("USENET_PORT"))
	if err != nil {
		t.Fatalf("USENET_PORT: %v", err)
	}
	useTLS, _ := strconv.ParseBool(os.Getenv("USENET_TLS"))

	return nntpclient.New(nntpclient.Config{
		Host:     host,
		Port:     port,
		TLS:      useTLS,
		User:     os.Getenv("USENET_USER"),
		Pass:     os.Getenv("USENET_PASS"),
		MaxConns: 10,
		Attempts: 3,
		Backoff:  time.Second,
		Timeout:  30 * time.Second,
	})
}

func liveNzb(t *testing.T) *nzbparser.NzbData {
	t.Helper()

	path := os.Getenv("NNTP_LIVE_NZB")
	if path == "" {
		t.Skip("NNTP_LIVE_NZB unset")
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open nzb: %v", err)
	}
	defer f.Close()

	nzb, err := nzbparser.ParseNzb(f, path)
	if err != nil {
		t.Fatalf("parse nzb: %v", err)
	}
	return nzb
}

func liveCache(t *testing.T, dir string) *diskcache.Cache {
	t.Helper()

	cache, err := diskcache.NewCache(&diskcache.CacheOptions{CacheDir: dir})
	if err != nil {
		t.Fatalf("cache: %v", err)
	}
	return cache
}

// Where the time goes before a single byte of a member is served: listing the
// archive when the nzb is added, and walking the block headers on the first
// Open of each member. Both are metadata work that nothing remembers, so the
// second run measures what a restart would pay again with the segments already
// on disk.
func TestLiveArchiveMetadataCost(t *testing.T) {
	nzb := liveNzb(t)
	client := liveClient(t)
	cacheDir := t.TempDir()
	count := &counter{}
	getSegment := count.wrap(client.GetSegment)

	// pass 1: cold cache
	start := time.Now()
	factory := NewNzbFileFactory(liveCache(t, cacheDir), getSegment, nil)
	files, err := factory.BuildSegmentStackFromNzbData(nzb)
	if err != nil {
		t.Fatalf("build stack: %v", err)
	}
	count.report(t, "build (list archive)", time.Since(start))

	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)

	// members are the paths the archive contributed, so anything the nzb itself
	// did not name
	nzbNames := make(map[string]bool, len(nzb.Files))
	for _, file := range nzb.Files {
		nzbNames[file.Filename] = true
	}

	members := make([]string, 0, 1)
	for _, name := range names {
		if !nzbNames[name] {
			members = append(members, name)
		}
	}
	if len(members) == 0 {
		t.Skipf("nzb holds no archive, nothing to measure")
	}
	t.Logf("%d nzb files, %d archive members", len(nzb.Files), len(members))

	for _, name := range members {
		count.reset()
		start := time.Now()
		reader, err := files[name].Open()
		if err != nil {
			t.Fatalf("open %s: %v", name, err)
		}
		count.report(t, "first Open (header walk)", time.Since(start))

		count.reset()
		start = time.Now()
		if _, err := reader.Seek(0, io.SeekStart); err != nil {
			t.Fatalf("seek %s: %v", name, err)
		}
		buf := make([]byte, 64*1024)
		if _, err := io.ReadFull(reader, buf); err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		count.report(t, "first 64 KiB", time.Since(start))
		reader.Close()
	}

	// pass 2: same segments on disk, everything the process learned thrown away.
	// This is the restart, and what an archive record in the metadata db would
	// have to beat.
	count.reset()
	start = time.Now()
	restarted := NewNzbFileFactory(liveCache(t, cacheDir), getSegment, nil)
	warmFiles, err := restarted.BuildSegmentStackFromNzbData(nzb)
	if err != nil {
		t.Fatalf("rebuild stack: %v", err)
	}
	count.report(t, "rebuild, warm cache", time.Since(start))

	for _, name := range members {
		count.reset()
		start := time.Now()
		reader, err := warmFiles[name].Open()
		if err != nil {
			t.Fatalf("reopen %s: %v", name, err)
		}
		count.report(t, "header walk, warm cache", time.Since(start))
		reader.Close()
	}
}
