package folderwatcher

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"git.ruekov.eu/ruakij/nzbStreamer/pkg/nzbparser"
)

func nzbContent(id string) string {
	return fmt.Sprintf(`<?xml version="1.0" encoding="utf-8" ?>
<nzb>
	<file poster="p@example.com" date="1700000000" subject="Release &#34;file.%s.rar&#34; yEnc (1/1)">
		<groups><group>alt.binaries.test</group></groups>
		<segments><segment bytes="100" number="1">%s@example.com</segment></segments>
	</file>
</nzb>`, id, id)
}

// watcherWithHook returns a watcher on a fresh dir plus the names it was notified about
func watcherWithHook(t *testing.T, consume bool) (*folderWatcher, *[]string) {
	t.Helper()

	fw := NewFolderWatcher(t.TempDir(), consume)

	var mu sync.Mutex
	added := []string{}
	_, err := fw.AddListener(func(nzbData *nzbparser.NzbData) error {
		mu.Lock()
		defer mu.Unlock()
		added = append(added, nzbData.MetaName)
		return nil
	}, nil)
	if err != nil {
		t.Fatalf("AddListener: %v", err)
	}

	return fw, &added
}

func write(t *testing.T, fw *folderWatcher, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(fw.watchFolder, name), []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}

func TestProcessesOnlyOnceAFileStopsChanging(t *testing.T) {
	fw, added := watcherWithHook(t, false)

	write(t, fw, "a.nzb", nzbContent("a")[:40]) // truncated, as if caught mid-write
	fw.scanDirectory()
	if len(*added) != 0 {
		t.Fatalf("processed a file on its first sighting: %v", *added)
	}

	write(t, fw, "a.nzb", nzbContent("a"))
	fw.scanDirectory()
	if len(*added) != 0 {
		t.Fatalf("processed a file that changed since the last scan: %v", *added)
	}

	fw.scanDirectory()
	if len(*added) != 1 || (*added)[0] != "a" {
		t.Fatalf("expected the settled file to be added once, got %v", *added)
	}

	fw.scanDirectory()
	if len(*added) != 1 {
		t.Fatalf("processed the same file twice: %v", *added)
	}
}

func TestConsumeDeletesTheFileOnceItIsAdded(t *testing.T) {
	fw, added := watcherWithHook(t, true)

	write(t, fw, "a.nzb", nzbContent("a"))
	fw.scanDirectory()
	fw.scanDirectory()

	if len(*added) != 1 {
		t.Fatalf("expected the file to be added, got %v", *added)
	}
	if _, err := os.Stat(filepath.Join(fw.watchFolder, "a.nzb")); !os.IsNotExist(err) {
		t.Fatalf("expected the file to be gone, stat gave %v", err)
	}
}

func TestConsumeKeepsAFileNoListenerTook(t *testing.T) {
	fw := NewFolderWatcher(t.TempDir(), true)
	_, err := fw.AddListener(func(*nzbparser.NzbData) error {
		return errors.New("rejected")
	}, nil)
	if err != nil {
		t.Fatalf("AddListener: %v", err)
	}

	write(t, fw, "a.nzb", nzbContent("a"))
	fw.scanDirectory()
	fw.scanDirectory()

	if _, err := os.Stat(filepath.Join(fw.watchFolder, "a.nzb")); err != nil {
		t.Fatalf("expected the rejected file to stay: %v", err)
	}
}

func TestNameReusedByAnotherReleaseIsProcessedAgain(t *testing.T) {
	fw, added := watcherWithHook(t, false)

	write(t, fw, "release.nzb", nzbContent("first"))
	fw.scanDirectory()
	fw.scanDirectory()

	write(t, fw, "release.nzb", nzbContent("second"))
	fw.scanDirectory()
	fw.scanDirectory()

	if len(*added) != 2 {
		t.Fatalf("expected both releases to be added, got %v", *added)
	}
}

func TestSameContentUnderAnotherNameIsProcessedOnce(t *testing.T) {
	fw, added := watcherWithHook(t, false)

	write(t, fw, "a.nzb", nzbContent("a"))
	fw.scanDirectory()
	fw.scanDirectory()

	write(t, fw, "copy.nzb", nzbContent("a"))
	fw.scanDirectory()
	fw.scanDirectory()

	if len(*added) != 1 {
		t.Fatalf("expected the copy to be skipped, got %v", *added)
	}
}
