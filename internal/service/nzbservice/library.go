package nzbservice

import "errors"

// ErrLibraryFull reports an add refused because the library is already as large
// as it is allowed to be.
var ErrLibraryFull = errors.New("library is full")

// Library is what has been added, as the nzbs describe it. Bytes is the nominal
// size: what the cache would have to hold to serve all of it at once, which is
// the number the cache is overprovisioned against rather than the disk used.
type Library struct {
	Nzbs  int
	Bytes int64
	// Exact says no nzb in the total is contributing an estimate
	Exact bool
	// MaxBytes is what Bytes may reach before adds are refused; 0 is unlimited
	MaxBytes int64
}

// Full reports whether the library is at its limit.
func (l Library) Full() bool {
	return l.MaxBytes > 0 && l.Bytes >= l.MaxBytes
}

// SetMaxLibraryBytes bounds the nominal size the library may reach. An add that
// would take it past this is refused with ErrLibraryFull, which is what keeps a
// library from growing so far past the cache that nothing stays cached long
// enough to be read twice. 0 or less is unlimited.
func (s *Service) SetMaxLibraryBytes(bytes int64) {
	s.maxLibraryBytes.Store(bytes)
}

// Library reports the nominal size of everything added. A failed or cancelled
// add presents nothing, so it counts for nothing.
func (s *Service) Library() Library {
	s.queueMutex.Lock()
	defer s.queueMutex.Unlock()

	return s.library()
}

// library requires queueMutex.
func (s *Service) library() Library {
	lib := Library{Exact: true, MaxBytes: s.maxLibraryBytes.Load()}
	for _, item := range s.queue {
		if item.Stage == StageFailed || item.Stage == StageCancelled {
			continue
		}
		lib.Nzbs++
		lib.Bytes += item.Bytes
		lib.Exact = lib.Exact && item.BytesExact
	}

	return lib
}
