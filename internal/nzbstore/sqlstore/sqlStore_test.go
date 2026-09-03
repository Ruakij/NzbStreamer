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

func TestAnAddAndHowItEndedSurviveReopening(t *testing.T) {
	dir := t.TempDir()

	data, err := nzbparser.ParseNzb(strings.NewReader(nzbXML), "Some.Release.nzb")
	if err != nil {
		t.Fatalf("ParseNzb: %v", err)
	}

	store := storeAt(t, dir)
	if err := store.Add(data, "queued"); err != nil {
		t.Fatalf("Add: %v", err)
	}
	// Twice, because a re-add of the same nzb must not be a primary-key conflict
	if err := store.Add(data, "queued"); err != nil {
		t.Fatalf("Add again: %v", err)
	}
	if err := store.SetStage(data.MetaName, "failed", "posts are gone"); err != nil {
		t.Fatalf("SetStage: %v", err)
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
	if got.Stage != "failed" || got.Err != "posts are gone" {
		t.Errorf("stage: got %q %q", got.Stage, got.Err)
	}
	if got.AddedAt.IsZero() || got.FinishedAt.IsZero() {
		t.Errorf("times: added %v, finished %v", got.AddedAt, got.FinishedAt)
	}
	if got.Data.MetaName != data.MetaName {
		t.Errorf("name: got %q, want %q", got.Data.MetaName, data.MetaName)
	}
	if len(got.Data.Files) != 1 || got.Data.Files[0].Filename != data.Files[0].Filename {
		t.Errorf("files: got %v, want %v", got.Data.Files, data.Files)
	}
	if got.Data.Meta[nzbparser.MetaKeyPassword] != "secret" {
		t.Errorf("meta: got %v", got.Data.Meta)
	}
}

func TestDelete(t *testing.T) {
	data, err := nzbparser.ParseNzb(strings.NewReader(nzbXML), "Some.Release.nzb")
	if err != nil {
		t.Fatalf("ParseNzb: %v", err)
	}

	store := storeAt(t, t.TempDir())
	if err := store.Add(data, "completed"); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := store.Delete(data.MetaName); err != nil {
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
