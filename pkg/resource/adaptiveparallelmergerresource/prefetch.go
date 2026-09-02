package adaptiveparallelmergerresource

import (
	"log/slog"
	"sync/atomic"
	"time"

	"git.ruekov.eu/ruakij/nzbStreamer/pkg/resource"
)

type prefetchSettings struct {
	// Connections are shared by every open file, so the budget is process-wide
	budget chan struct{}
	// How far ahead of the read head the cache should be warm, in time
	leadTime time.Duration
	// Lead bounds in resources, so a lead is measurable before a rate is known
	// and stays bounded once it is
	minLead int
	maxLead int
}

var prefetch atomic.Pointer[prefetchSettings]

// SetPrefetch sizes prefetching: how many fetches may run at once across all
// readers, how far ahead of the read head to stay warm, and the bounds the lead
// derived from the observed read speed is clamped to. A maximum lead of 0
// disables prefetching.
func SetPrefetch(concurrency int, leadTime time.Duration, minLead, maxLead int) {
	if concurrency < 1 {
		concurrency = 1
	}

	prefetch.Store(&prefetchSettings{
		budget:   make(chan struct{}, concurrency),
		leadTime: leadTime,
		minLead:  minLead,
		maxLead:  maxLead,
	})
}

// consumptionRate is what the consumer has taken per second since the read head
// was last placed, or 0 while the run is too short to measure.
func (r *AdaptiveParallelMergerResourceReader) consumptionRate(settings *prefetchSettings) float64 {
	elapsed := time.Since(r.runStart)
	if r.runStart.IsZero() || elapsed < settings.leadTime {
		return 0
	}

	return float64(r.runBytes) / elapsed.Seconds()
}

// lead is how many resources ahead of the read head to warm: what the consumer
// takes during the lead time, expressed in resources of the size at hand.
func (r *AdaptiveParallelMergerResourceReader) lead(settings *prefetchSettings) int {
	rate := r.consumptionRate(settings)
	if rate <= 0 {
		return settings.minLead
	}

	resourceSize, err := r.resource.resources[r.readerIndex].SizeHint()
	if err != nil || resourceSize <= 0 {
		return settings.minLead
	}

	lead := int(rate * settings.leadTime.Seconds() / float64(resourceSize))

	return min(max(lead, settings.minLead), settings.maxLead)
}

// prefetch warms the resources ahead of the read head. It works on resources
// rather than readers, so what it fetches lands in their cache and outlives this
// reader; a demand read arriving later just finds it there.
//
// ponytail: refill happens per Read, not the moment a fetch finishes. A read that
// consumes a whole lead in one call still issues the next one; a stalled consumer
// simply stops asking.
func (r *AdaptiveParallelMergerResourceReader) prefetch() {
	settings := prefetch.Load()
	if settings == nil || settings.maxLead <= 0 {
		return
	}

	if r.prefetchedTo < r.readerIndex {
		r.prefetchedTo = r.readerIndex
	}
	limit := min(r.readerIndex+r.lead(settings), len(r.resource.resources))

	for i := r.prefetchedTo; i < limit; i++ {
		prefetcher, ok := r.resource.resources[i].(resource.Prefetcher)
		if !ok {
			continue
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
