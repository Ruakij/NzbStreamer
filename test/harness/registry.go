package harness

import (
	"path"
	"slices"

	"git.ruekov.eu/ruakij/nzbStreamer/test/payload"
)

// Test is one thing to read: a fixture set and the path of one file in it.
// Read type and the rest of the sweep axes live in the matrix, not here; every
// selected test runs the whole matrix.
type Test struct {
	name      string
	fixture   string // directory in the work volume the fixture lives under
	path      string // path under /webdav/, e.g. "plain/plain.mkv"
	path2     string // a second file, read in parallel with the first when set
	fusePath  string // presentation through the FUSE mount, e.g. "/app/mnt/plain/plain.mkv", "" = no FUSE read
	content   string // source payload the bytes must equal, "" if unknown
	validate  bool   // assert a full-coverage read returns the payload bytes
	expectErr bool   // a read is supposed to fail (the damaged set)
	enabled   bool
}

func (t Test) Name() string     { return t.name }
func (t Test) Fixture() string  { return t.fixture }
func (t Test) Path() string     { return t.path }
func (t Test) Path2() string    { return t.path2 }
func (t Test) Content() string  { return t.content }
func (t Test) ExpectErr() bool  { return t.expectErr }
func (t Test) FusePath() string { return t.fusePath }
func (t Test) Fuse() bool       { return t.fusePath != "" }

// Concurrent reports whether this test reads two files at once: the second path
// is what makes it one, so there is nothing else to keep in step with it.
func (t Test) Concurrent() bool { return t.path2 != "" }

// Paths are every webdav path the test reads, which is what has to resolve
// before it can run.
func (t Test) Paths() []string {
	if t.path2 == "" {
		return []string{t.path}
	}
	return []string{t.path, t.path2}
}

// CanValidate reports whether a read of the given pattern covers the whole
// file and therefore has a digest to check against.
func (t Test) CanValidate(readtype string) bool {
	// tail and sequential cover everything; random shuffles a full cover;
	// stride deliberately leaves chunks out
	return t.validate && slices.Contains([]string{"sequential", "tail", "random"}, readtype)
}

// ExpectedSize is the byte length a full read has to deliver.
func (t Test) ExpectedSize() int64 {
	if t.content == "" {
		return -1
	}
	return payload.Size()
}

// Tests is the registry. Every enabled test runs unless specific ones are
// named. The paths are what the running stack actually presents (verified by
// listing the webdav tree): an unpacked rar member appears as
// <set>/<archive>/<set>.mkv, a posted file as <set>/<file>.
//
// Names are <transport>-<fixture>: webdav-* read over HTTP, fuse-* through the
// FUSE mount. Without a transport the axis-to-test binding is unclear, and a
// read prefix would be noise since every test is a read.
var Tests = []Test{
	{
		name: "webdav-plain", fixture: "plain",
		path: "plain/plain.mkv", content: "plain.mkv", validate: true, enabled: true,
	},
	{
		// Parallel read of the two plain files; per-request latency is the
		// signal as the readahead chunk sweeps. Needs both fixtures posted, so
		// it is all-at-once only (the sequential lifecycle posts one fixture
		// group at a time and would not guarantee plain2).
		name: "webdav-plain-concurrent", fixture: "plain",
		path: "plain/plain.mkv", path2: "plain2/plain.mkv", content: "plain.mkv", enabled: true,
	},
	{
		name: "webdav-rar-stored", fixture: "rar-stored",
		path: "rar-stored/movie.rar/rar-stored.mkv", content: "movie.mkv", validate: true, enabled: true,
	},
	{
		name: "webdav-rar-multi", fixture: "rar-multi",
		path: "rar-multi/movie.part.rar/rar-multi.mkv", content: "movie.mkv", validate: true, enabled: true,
	},
	{
		name: "webdav-rar-compressed", fixture: "rar-compressed",
		path: "rar-compressed/movie.rar/rar-compressed.mkv", content: "movie.mkv", validate: true, enabled: true,
	},
	{
		// Solid rar (-ms -m3): members can't be extracted cheaply, so the stack
		// has to decompress the stream to serve a member. Same member name as
		// the other rar sets - the archive builder keeps the entry name.
		name: "webdav-rar-solid", fixture: "rar-solid",
		path: "rar-solid/movie.rar/movie.mkv", content: "movie.mkv", validate: true, enabled: true,
	},
	{
		// The 7z set is posted as 7z.7z and the stack presents that container as
		// a file, not as unpacked members: measured as a whole file, never
		// validated.
		name: "webdav-7z", fixture: "7z", path: "7z/7z.7z", enabled: true,
	},
	{
		// Not unpacked by the stack either, so its bytes are the archive
		// container, not a payload source: measured, never validated.
		name: "webdav-zip", fixture: "zip", path: "zip/zip.zip", enabled: true,
	},
	{
		name: "webdav-par2", fixture: "par2",
		path: "par2/plain.mkv", content: "plain.mkv", validate: true, enabled: true,
	},
	{
		// The damaged set is refused by the health check, so reading it must
		// fail; a read that succeeds is the bug this test exists for.
		name: "webdav-damaged", fixture: "damaged",
		path: "damaged/plain.mkv", content: "plain.mkv", expectErr: true, enabled: true,
	},

	// Every fixture is also read through the FUSE mount: a presentation-layer
	// read of the program, measured and validated inside the mount namespace.
	// Only meaningful with -fuse; otherwise the probe flags it unsupported.
	{
		name: "fuse-plain", fixture: "plain",
		path: "plain/plain.mkv", fusePath: "/app/mnt/plain/plain.mkv",
		content: "plain.mkv", validate: true, enabled: true,
	},
	{
		name: "fuse-rar-stored", fixture: "rar-stored",
		path: "rar-stored/movie.rar/rar-stored.mkv", fusePath: "/app/mnt/rar-stored/movie.rar/rar-stored.mkv",
		content: "movie.mkv", validate: true, enabled: true,
	},
	{
		name: "fuse-rar-multi", fixture: "rar-multi",
		path: "rar-multi/movie.part.rar/rar-multi.mkv", fusePath: "/app/mnt/rar-multi/movie.part.rar/rar-multi.mkv",
		content: "movie.mkv", validate: true, enabled: true,
	},
	{
		name: "fuse-rar-compressed", fixture: "rar-compressed",
		path: "rar-compressed/movie.rar/rar-compressed.mkv", fusePath: "/app/mnt/rar-compressed/movie.rar/rar-compressed.mkv",
		content: "movie.mkv", validate: true, enabled: true,
	},
	{
		name: "fuse-rar-solid", fixture: "rar-solid",
		path: "rar-solid/movie.rar/movie.mkv", fusePath: "/app/mnt/rar-solid/movie.rar/movie.mkv",
		content: "movie.mkv", validate: true, enabled: true,
	},
	{
		name: "fuse-7z", fixture: "7z",
		path: "7z/7z.7z", fusePath: "/app/mnt/7z/7z.7z", enabled: true,
	},
	{
		name: "fuse-zip", fixture: "zip",
		path: "zip/zip.zip", fusePath: "/app/mnt/zip/zip.zip", enabled: true,
	},
	{
		name: "fuse-par2", fixture: "par2",
		path: "par2/plain.mkv", fusePath: "/app/mnt/par2/plain.mkv",
		content: "plain.mkv", validate: true, enabled: true,
	},
	{
		name: "fuse-damaged", fixture: "damaged",
		path: "damaged/plain.mkv", fusePath: "/app/mnt/damaged/plain.mkv",
		content: "plain.mkv", expectErr: true, enabled: true,
	},
}

// Select resolves user-supplied names against the registry. An empty list
// means every enabled test; otherwise each name is matched as a glob
// (path.Match), so "*plain", "webdav-*", "*-rar-*" all work.
func Select(names []string) []Test {
	var out []Test
	for _, t := range Tests {
		if !t.enabled {
			continue
		}
		if len(names) == 0 || matchAny(t.name, names) {
			out = append(out, t)
		}
	}
	return out
}

func matchAny(name string, patterns []string) bool {
	for _, p := range patterns {
		if ok, err := path.Match(p, name); err == nil && ok {
			return true
		}
	}
	return false
}
