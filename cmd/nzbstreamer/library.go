package main

import (
	"log/slog"
	"sync"
	"time"

	"git.ruekov.eu/ruakij/nzbStreamer/internal/nzbstore/sqlstore"
	"git.ruekov.eu/ruakij/nzbStreamer/internal/service/nzbservice"
)

// libraryRefresh is how long a measurement is served before it is taken again.
// Measuring the active bytes scans the segment table, and both numbers move at
// the pace of adds and reads, so nothing is gained from taking it per request.
const libraryRefresh = time.Minute

// libraryStats is the nominal size of everything added next to what of it was
// actually read within the window. Nominal against the cache size says how far
// the library is overprovisioned; active says how much of the cache the reads
// would need to never fetch the same bytes twice.
type libraryStats struct {
	nzbservice.Library
	sqlstore.SegmentActivity
	Window time.Duration
}

type libraryMeter struct {
	library  func() nzbservice.Library
	activity func(since time.Time) (sqlstore.SegmentActivity, error)
	window   time.Duration

	mutex sync.Mutex
	taken time.Time
	last  libraryStats
}

func newLibraryMeter(library func() nzbservice.Library, activity func(time.Time) (sqlstore.SegmentActivity, error), window time.Duration) *libraryMeter {
	return &libraryMeter{library: library, activity: activity, window: window}
}

// read returns the current measurement, taking it again once it has aged out.
func (m *libraryMeter) read() libraryStats {
	m.mutex.Lock()
	defer m.mutex.Unlock()

	if !m.taken.IsZero() && time.Since(m.taken) < libraryRefresh {
		return m.last
	}

	stats := libraryStats{Library: m.library(), Window: m.window}

	activity, err := m.activity(time.Now().Add(-m.window))
	if err != nil {
		// The nominal side is still worth reporting, and the active side is a
		// number to tune a cache by rather than one anything depends on
		slog.Warn("Failed measuring the active library", "error", err)
		activity = m.last.SegmentActivity
	}
	stats.SegmentActivity = activity

	m.taken, m.last = time.Now(), stats
	return stats
}
