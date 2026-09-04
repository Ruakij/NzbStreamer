package nzbservice

import (
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"slices"
	"sync"

	"git.ruekov.eu/ruakij/nzbStreamer/internal/nzbstore"
	"git.ruekov.eu/ruakij/nzbStreamer/internal/presentation"
	"git.ruekov.eu/ruakij/nzbStreamer/pkg/nzbparser"
	"git.ruekov.eu/ruakij/nzbStreamer/pkg/resource"
)

// storeFiles records what an nzb presents, so the next start lists it from one
// query instead of walking its archives over the network again. A size it could
// not ask for leaves the whole tree unstored: a partial one would present fewer
// files than the nzb has.
func (s *Service) storeFiles(metaName string, tree map[string]presentation.Openable) {
	if s.treeKey == "" {
		return
	}

	files := make([]nzbstore.File, 0, len(tree))
	for fullPath, file := range tree {
		size, exact, err := presentedSize(file)
		if err != nil {
			slog.Warn("Failed sizing a file, so the next start rebuilds this tree",
				"nzb", metaName, "file", fullPath, "error", err)
			return
		}
		files = append(files, nzbstore.File{Path: fullPath, Size: size, Exact: exact})
	}

	if err := s.store.SetFiles(metaName, s.treeKey, files); err != nil {
		slog.Error("Failed storing the files of an nzb, so the next start rebuilds its tree",
			"nzb", metaName, "error", err)
	}
}

// presentedSize is the size a listing would report for a file, and whether that
// is measured or the hint.
func presentedSize(file presentation.Openable) (size int64, exact bool, err error) {
	if sized, ok := file.(resource.Sized); ok {
		if size, err := sized.Size(); err == nil {
			return size, true, nil
		}
	}

	size, err = file.SizeHint()
	if err != nil {
		return 0, false, fmt.Errorf("failed asking for a size hint: %w", err)
	}
	return size, false, nil
}

// restoreTree presents what the store recorded an nzb presenting, without
// building anything or touching the news server. It reports whether it could: a
// tree from other settings, or one from before they were stored, is rebuilt.
func (s *Service) restoreTree(record nzbstore.Record) bool {
	if s.treeKey == "" || record.TreeKey != s.treeKey {
		slog.Debug("Rebuilding a tree, since it was stored under other settings",
			"nzb", record.Data.MetaName, "stored", record.TreeKey, "settings", s.treeKey)
		return false
	}

	files, err := s.store.Files(record.Data.MetaName)
	if err != nil {
		slog.Error("Failed reading the stored files of an nzb, rebuilding its tree",
			"nzb", record.Data.MetaName, "error", err)
		return false
	}
	if len(files) == 0 {
		slog.Debug("Rebuilding a tree, since nothing is stored of what it presents",
			"nzb", record.Data.MetaName)
		return false
	}

	slog.Debug("Restored a tree from the store", "nzb", record.Data.MetaName, "files", len(files))

	tree := &lazyTree{service: s, data: record.Data}
	presented := make(map[string]presentation.Openable, len(files))
	for _, file := range files {
		presented[file.Path] = &restoredFile{tree: tree, path: file.Path, size: file.Size, exact: file.Exact}
	}

	s.mutex.Lock()
	s.nzbFiledata[record.Data.MetaName] = record.Data
	s.mutex.Unlock()

	s.register(record.Data, presented)

	return true
}

// lazyTree is an nzb presented from its stored rows, with nothing built behind
// them. The archive walk that produces one member produces all of them, so one
// build serves every path of the nzb and a client opening two members of one
// archive waits for it once.
type lazyTree struct {
	service *Service
	data    *nzbparser.NzbData

	mutex sync.Mutex
	files map[string]presentation.Openable
}

// open builds the stack unless it is built, and hands back the file behind
// fullPath. A build that failed is not remembered as one: the entry stays
// listed and the next read tries again, so what a client sees is the failure it
// would have had from a file whose articles are gone, rather than a release
// missing from the listing.
func (t *lazyTree) open(fullPath string) (presentation.Openable, error) {
	t.mutex.Lock()
	defer t.mutex.Unlock()

	if t.files == nil {
		files, err := t.service.buildTree(t.data)
		if err != nil {
			return nil, err
		}
		t.files = files
		t.service.reconcile(t.data, files)
	}

	file, ok := t.files[fullPath]
	if !ok {
		return nil, fmt.Errorf("%w: %s", fs.ErrNotExist, fullPath)
	}
	return file, nil
}

// reconcile replaces the restored tree where the build disagrees with it. The
// stored rows were a cache of the builds answer and never an authority over it,
// so the build wins and the rows are rewritten.
//
// It runs in the background because a presenter calls open with its own tree
// locked, and re-registering takes that same lock.
func (s *Service) reconcile(nzbData *nzbparser.NzbData, tree map[string]presentation.Openable) {
	built := make([]string, 0, len(tree))
	for fullPath := range tree {
		built = append(built, fullPath)
	}
	slices.Sort(built)

	s.mutex.RLock()
	presented := slices.Clone(s.nzbFiles[nzbData.MetaName])
	s.mutex.RUnlock()
	slices.Sort(presented)

	if slices.Equal(built, presented) {
		return
	}

	slog.Warn("An nzb built a different tree than was stored for it, presenting the built one",
		"nzb", nzbData.MetaName, "stored", presented, "built", built)

	go func() {
		s.mutex.Lock()
		s.unregister(nzbData.MetaName)
		s.nzbFiledata[nzbData.MetaName] = nzbData
		s.mutex.Unlock()

		s.register(nzbData, tree)
		s.storeFiles(nzbData.MetaName, tree)
	}()
}

// restoredFile is one path of a lazyTree: listed and sized from its row, built
// on the read that wants bytes.
type restoredFile struct {
	tree  *lazyTree
	path  string
	size  int64
	exact bool
}

func (f *restoredFile) SizeHint() (int64, error) {
	return f.size, nil
}

// Size answers exactly only where the stored size was measured, which is the
// same answer the built stack gave when it was stored.
func (f *restoredFile) Size() (int64, error) {
	if !f.exact {
		return f.size, resource.ErrSizeNotExact
	}
	return f.size, nil
}

func (f *restoredFile) Open() (io.ReadSeekCloser, error) {
	file, err := f.tree.open(f.path)
	if err != nil {
		return nil, err
	}
	return file.Open()
}
