package sqlstore

import (
	"path/filepath"
	"strings"
	"testing"

	"git.ruekov.eu/ruakij/nzbStreamer/pkg/nzbparser"
)

const nzbXML = `<?xml version="1.0" encoding="utf-8" ?>
<nzb>
	<head><meta type="password">secret</meta></head>
	<file poster="p@example.com" date="1700000000" subject="Release &#34;file.rar&#34; yEnc (1/1)">
		<groups><group>alt.binaries.test</group></groups>
		<segments><segment bytes="100" number="1">a@example.com</segment></segments>
	</file>
</nzb>`

func storeAt(t *testing.T, dir string) *Store {
	t.Helper()

	store, err := New(filepath.Join(dir, "sub", "metadata.db"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { store.Close() })

	return store
}

func TestSetSurvivesReopening(t *testing.T) {
	dir := t.TempDir()

	data, err := nzbparser.ParseNzb(strings.NewReader(nzbXML), "Some.Release.nzb")
	if err != nil {
		t.Fatalf("ParseNzb: %v", err)
	}

	store := storeAt(t, dir)
	if err := store.Set(data); err != nil {
		t.Fatalf("Set: %v", err)
	}
	// Twice, because a re-add of the same nzb must not be a primary-key conflict
	if err := store.Set(data); err != nil {
		t.Fatalf("Set again: %v", err)
	}
	store.Close()

	list, err := storeAt(t, dir).List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("expected one nzb, got %d", len(list))
	}

	got := list[0]
	if got.MetaName != data.MetaName {
		t.Errorf("name: got %q, want %q", got.MetaName, data.MetaName)
	}
	if len(got.Files) != 1 || got.Files[0].Filename != data.Files[0].Filename {
		t.Errorf("files: got %v, want %v", got.Files, data.Files)
	}
	if got.Meta[nzbparser.MetaKeyPassword] != "secret" {
		t.Errorf("meta: got %v", got.Meta)
	}
}

func TestDelete(t *testing.T) {
	data, err := nzbparser.ParseNzb(strings.NewReader(nzbXML), "Some.Release.nzb")
	if err != nil {
		t.Fatalf("ParseNzb: %v", err)
	}

	store := storeAt(t, t.TempDir())
	if err := store.Set(data); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if err := store.Delete(data); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	list, err := store.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 0 {
		t.Fatalf("expected an empty store, got %v", list)
	}
}
