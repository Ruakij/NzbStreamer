package main

import (
	"log/slog"
	"slices"
	"sync"
	"time"

	"git.ruekov.eu/ruakij/nzbStreamer/internal/nzbstore/sqlstore"
	"git.ruekov.eu/ruakij/nzbStreamer/internal/service/nzbservice"
)

// libraryRefresh is how long a measurement is served before it is taken again.
// Measuring the active bytes scans the segment table, and both numbers move at
// the pace of adds and reads, so nothing is gained from taking it per request.
const libraryRefresh = time.Minute

// windowActivity is what was read within one span, which is the data that was in
// circulation over it. A day against a month is what a cache holding all of it
// would have to be, and how much a larger one would buy.
type windowActivity struct {
	Window time.Duration
	sqlstore.SegmentActivity
}

// libraryStats is the nominal size of everything added next to what of it was
// actually read. Nominal against the cache size says how far the library is
// overprovisioned; active says how much of the cache the reads would need to
// never fetch the same bytes twice.
type libraryStats struct {
	nzbservice.Library
	// Active is one measurement per configured window, widest last
	Active []windowActivity
	// RefetchedBytes is what has been downloaded twice since the process
	// started, a running total rather than a window: a refetch is an event with
	// a size, where the active library is a set of segments
	RefetchedBytes    int64
	RefetchedSegments int64
}

// Widest is the longest window measured, which is the one a single number to
// show alongside the library is taken from.
func (s libraryStats) Widest() windowActivity {
	if len(s.Active) == 0 {
		return windowActivity{}
	}
	return s.Active[len(s.Active)-1]
}

type libraryMeter struct {
	library   func() nzbservice.Library
	activity  func(cutoffs []time.Time) ([]sqlstore.SegmentActivity, error)
	refetches func() (bytes, segments int64)
	windows   []time.Duration

	mutex sync.Mutex
	taken time.Time
	last  libraryStats
}

func newLibraryMeter(library func() nzbservice.Library, activity func([]time.Time) ([]sqlstore.SegmentActivity, error), refetches func() (int64, int64), windows []time.Duration) *libraryMeter {
	windows = slices.Clone(windows)
	slices.Sort(windows)

	return &libraryMeter{library: library, activity: activity, refetches: refetches, windows: windows}
}

// read returns the current measurement, taking it again once it has aged out.
// The refetches are read every time regardless: they are two atomics rather
// than a table scan, and a counter served from a minute-old cache would step.
func (m *libraryMeter) read() libraryStats {
	m.mutex.Lock()
	defer m.mutex.Unlock()

	if !m.taken.IsZero() && time.Since(m.taken) < libraryRefresh {
		last := m.last
		last.RefetchedBytes, last.RefetchedSegments = m.refetches()
		return last
	}

	stats := libraryStats{Library: m.library(), Active: m.last.Active}
	stats.RefetchedBytes, stats.RefetchedSegments = m.refetches()

	now := time.Now()
	cutoffs := make([]time.Time, len(m.windows))
	for i, window := range m.windows {
		cutoffs[i] = now.Add(-window)
	}

	activity, err := m.activity(cutoffs)
	if err != nil {
		// The nominal side is still worth reporting, and the active side is a
		// number to tune a cache by rather than one anything depends on
		slog.Warn("Failed measuring the active library", "error", err)
	} else {
		stats.Active = make([]windowActivity, len(m.windows))
		for i, window := range m.windows {
			stats.Active[i] = windowActivity{Window: window, SegmentActivity: activity[i]}
		}
	}

	m.taken, m.last = time.Now(), stats
	return stats
}
