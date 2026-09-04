package adaptiveparallelmergerresource

import (
	"log/slog"
	"sync/atomic"
	"time"

	"git.ruekov.eu/ruakij/nzbStreamer/pkg/resource"
)

// PrefetchSettings sizes prefetching. Concurrency is process-wide, since the
// connections are shared by every open file.
type PrefetchSettings struct {
	Concurrency int
	LeadTime    time.Duration
	// Lead bounds in resources, so a lead is measurable before a rate is known
	// and stays bounded once it is. A MaxLead of 0 disables prefetching.
	MinLead int
	MaxLead int
	// Pressure reports fetches already waiting for a connection
	// Nil drops the gate.
	Pressure func() int
	// Overcommit is the pressure prefetch stops at
	Overcommit int
}

type prefetchSettings struct {
	PrefetchSettings
	budget chan struct{}
}

var prefetch atomic.Pointer[prefetchSettings]

// SetPrefetch installs the settings every reader prefetches by.
func SetPrefetch(settings PrefetchSettings) {
	if settings.Concurrency < 1 {
		settings.Concurrency = 1
	}

	prefetch.Store(&prefetchSettings{
		PrefetchSettings: settings,
		budget:           make(chan struct{}, settings.Concurrency),
	})
}

// atCapacity reads the pool before the fetch is launched, so a burst can
// overshoot it by whatever is submitted before any of it reaches a connection;
// the budget bounds that.
func (s *prefetchSettings) atCapacity() bool {
	return s.Pressure != nil && s.Pressure() >= s.Overcommit
}

// consumptionRate is what the consumer has taken per second since the read head
// was last placed, or 0 while the run is too short to measure.
//
// Requires prefetchMutex.
func (r *AdaptiveParallelMergerResourceReader) consumptionRate(settings *prefetchSettings) float64 {
	elapsed := time.Since(r.runStart)
	if r.runStart.IsZero() || elapsed < settings.LeadTime {
		return 0
	}

	return float64(r.runBytes) / elapsed.Seconds()
}

// lead is how many resources ahead of the read head to warm: what the consumer
// takes during the lead time, expressed in resources of the size at hand.
//
// Requires prefetchMutex.
func (r *AdaptiveParallelMergerResourceReader) lead(settings *prefetchSettings, index int) int {
	rate := r.consumptionRate(settings)
	if rate <= 0 || index >= len(r.resource.resources) {
		return settings.MinLead
	}

	resourceSize, err := r.resource.resources[index].SizeHint()
	if err != nil || resourceSize <= 0 {
		return settings.MinLead
	}

	lead := int(rate * settings.LeadTime.Seconds() / float64(resourceSize))

	return min(max(lead, settings.MinLead), settings.MaxLead)
}

// prefetchAt anchors the lead on a positional read, which carries no read head
// of its own. Concurrent readahead arrives out of order, so the anchor only
// advances; one landing outside the warm window is a jump, and drops the lead
// and the rate the way a seek does.
func (r *AdaptiveParallelMergerResourceReader) prefetchAt(index int, read int64) {
	settings := prefetch.Load()
	if settings == nil || settings.MaxLead <= 0 {
		return
	}

	r.prefetchMutex.Lock()
	if index < r.readAtIndex-1 || index > r.readAtIndex+settings.MaxLead {
		r.prefetchedTo = index
		r.runStart = time.Now()
		r.runBytes = 0
	}
	if index > r.readAtIndex {
		r.readAtIndex = index
	}
	if r.runStart.IsZero() {
		r.runStart = time.Now()
	}
	r.runBytes += read
	anchor := r.readAtIndex
	r.prefetchMutex.Unlock()

	r.prefetchFrom(anchor)
}

// noteSeek keeps the lead and the rate when the seek landed inside the window
// already warm ahead of from - a caller chopping a stream into small positional
// reads looks like that - and drops both otherwise, since a jump says nothing
// about what follows.
func (r *AdaptiveParallelMergerResourceReader) noteSeek(from int) {
	r.prefetchMutex.Lock()
	defer r.prefetchMutex.Unlock()

	if r.readerIndex >= from && r.readerIndex <= r.prefetchedTo {
		return
	}

	r.prefetchedTo = r.readerIndex
	r.readAtIndex = r.readerIndex
	r.runStart = time.Time{}
	r.runBytes = 0
}

// noteRead folds what a read served into the rate estimate.
func (r *AdaptiveParallelMergerResourceReader) noteRead(read int64) {
	r.prefetchMutex.Lock()
	defer r.prefetchMutex.Unlock()

	if r.runStart.IsZero() {
		r.runStart = time.Now()
	}
	r.runBytes += read
}

// prefetchFrom warms the resources ahead of index. It works on resources rather
// than readers, so what it fetches lands in their cache and outlives this
// reader; a demand read arriving later just finds it there.
//
// The lead refills per Read rather than the moment a fetch finishes. A read that
// consumes a whole lead in one call still issues the next one; a stalled consumer
// simply stops asking.
func (r *AdaptiveParallelMergerResourceReader) prefetchFrom(index int) {
	settings := prefetch.Load()
	if settings == nil || settings.MaxLead <= 0 {
		return
	}

	r.prefetchMutex.Lock()
	defer r.prefetchMutex.Unlock()

	if r.prefetchedTo < index {
		r.prefetchedTo = index
	}
	limit := min(index+r.lead(settings, index), len(r.resource.resources))

	for i := r.prefetchedTo; i < limit; i++ {
		prefetcher, ok := r.resource.resources[i].(resource.Prefetcher)
		if !ok {
			continue
		}

		// What is queued already keeps the connections busy; more only lengthens
		// the wait of a demand read behind it
		if settings.atCapacity() {
			r.prefetchedTo = i
			return
		}

		select {
		case settings.budget <- struct{}{}:
		default:
			// Budget is spent; the next Read picks up from here
			r.prefetchedTo = i
			return
		}

		go func() {
			defer func() { <-settings.budget }()

			if err := prefetcher.Prefetch(); err != nil {
				slog.Debug("Prefetch failed", "index", i, "error", err)
			}
		}()
	}

	r.prefetchedTo = limit
}
