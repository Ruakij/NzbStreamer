package harness

import (
	"os"
	"path/filepath"
	"testing"
)

// rigDir lays out the subset of test/ postedStamp reads, so the stamp can be
// tested without the real tree.
func rigDir(t *testing.T) (*Runner, string) {
	t.Helper()
	dir := t.TempDir()
	for _, f := range []string{
		"archives/Dockerfile", "archives/archives.sh",
		"inn/Dockerfile", "inn/entrypoint.sh", "inn/post.sh",
	} {
		p := filepath.Join(dir, f)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("# "+f), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return &Runner{ComposeFile: filepath.Join(dir, "compose.yaml")}, dir
}

// TestPostedStamp pins the inputs the repost skip trusts: same inputs hash the
// same, and every input a changed post would depend on moves the hash.
func TestPostedStamp(t *testing.T) {
	r, dir := rigDir(t)
	base, err := r.postedStamp([]string{"plain"})
	if err != nil {
		t.Fatal(err)
	}
	again, err := r.postedStamp([]string{"plain"})
	if err != nil {
		t.Fatal(err)
	}
	if base != again {
		t.Fatal("same inputs produced different stamps")
	}

	// restore rewrites the tree for the next case, since every case is judged
	// against the same base stamp.
	restore := func() {
		r.Ram = false
		t.Setenv("SIZE_MB", "")
		if err := os.WriteFile(filepath.Join(dir, "archives", "archives.sh"), []byte("# archives/archives.sh"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	restore()

	for _, tc := range []struct {
		name   string
		mutate func()
		sets   []string
	}{
		{"edited builder", func() {
			if err := os.WriteFile(filepath.Join(dir, "archives", "archives.sh"), []byte("# changed"), 0o644); err != nil {
				t.Fatal(err)
			}
		}, []string{"plain"}},
		{"other sets", func() {}, []string{"plain", "damaged"}},
		{"other payload size", func() { t.Setenv("SIZE_MB", "128") }, []string{"plain"}},
		{"other storage flavor", func() { r.Ram = true }, []string{"plain"}},
	} {
		tc.mutate()
		got, err := r.postedStamp(tc.sets)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if got == base {
			t.Errorf("%s: stamp did not change", tc.name)
		}
		restore()
	}
}
