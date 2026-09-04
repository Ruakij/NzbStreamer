package adaptiveparallelmergerresource_test

import (
	"io"
	"sync/atomic"
	"testing"
	"time"

	"git.ruekov.eu/ruakij/nzbStreamer/pkg/resource"
	"git.ruekov.eu/ruakij/nzbStreamer/pkg/resource/adaptiveparallelmergerresource"
	"git.ruekov.eu/ruakij/nzbStreamer/pkg/resource/bytesresource"
)

// CountingResource records what the merger asks of it: how often it was opened,
// how often a reader of it was closed, and whether it was prefetched.
type CountingResource struct {
	resource  resource.ReadSeekCloseableResource
	opens     atomic.Int64
	closes    atomic.Int64
	prefetchs atomic.Int64
}

type CountingResourceReader struct {
	resource *CountingResource
	reader   io.ReadSeekCloser
}

func NewCountingResource(data []byte) *CountingResource {
	return &CountingResource{resource: &bytesresource.BytesResource{Content: data}}
}

func (r *CountingResource) Open() (io.ReadSeekCloser, error) {
	r.opens.Add(1)

	reader, err := r.resource.Open()
	return &CountingResourceReader{resource: r, reader: reader}, err
}

func (r *CountingResource) SizeHint() (int64, error) {
	return r.resource.SizeHint()
}

func (r *CountingResource) Prefetch() error {
	r.prefetchs.Add(1)
	return nil
}

func (r *CountingResourceReader) Read(p []byte) (int, error) {
	return r.reader.Read(p)
}

func (r *CountingResourceReader) Seek(offset int64, whence int) (int64, error) {
	return r.reader.Seek(offset, whence)
}

func (r *CountingResourceReader) Close() error {
	r.resource.closes.Add(1)
	return r.reader.Close()
}

func countingResources(count, size int) ([]resource.ReadSeekCloseableResource, []*CountingResource) {
	resources := make([]resource.ReadSeekCloseableResource, count)
	counters := make([]*CountingResource, count)
	for i := range resources {
		counters[i] = NewCountingResource(make([]byte, size))
		resources[i] = counters[i]
	}
	return resources, counters
}

// waitFor polls until condition holds, so a prefetch running in its own goroutine
// gets a chance to land.
func waitFor(t *testing.T, what string, condition func() bool) {
	t.Helper()

	deadline := time.Now().Add(time.Second)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestPrefetchWarmsMinimumLeadAhead(t *testing.T) {
	const leadMin = 3

	adaptiveparallelmergerresource.SetPrefetch(adaptiveparallelmergerresource.PrefetchSettings{Concurrency: 8, LeadTime: time.Minute, MinLead: leadMin, MaxLead: 8})
	defer adaptiveparallelmergerresource.SetPrefetch(adaptiveparallelmergerresource.PrefetchSettings{})

	resources, counters := countingResources(20, 10)

	reader, err := adaptiveparallelmergerresource.NewAdaptiveParallelMergerResource(resources).Open()
	if err != nil {
		t.Fatalf("failed opening merger: %v", err)
	}
	defer reader.Close()

	if _, err := reader.Read(make([]byte, 5)); err != nil {
		t.Fatalf("failed reading: %v", err)
	}

	waitFor(t, "the minimum lead to be warmed", func() bool {
		return counters[leadMin-1].prefetchs.Load() == 1
	})
	// A lead time far beyond the run keeps the rate unmeasured, so the lead stays
	// at its minimum
	if got := counters[leadMin].prefetchs.Load(); got != 0 {
		t.Errorf("resource %d beyond the lead was prefetched %d times", leadMin, got)
	}
}

// Pressure at the overcommit means the connections are as loaded as they may
// get, so prefetch stops rather than putting a demand read further back.
// Overcommit is signed, so the same gate expresses leaving connections free,
// filling them exactly, and queueing beyond them.
func TestPrefetchStopsAtTheOvercommit(t *testing.T) {
	for _, test := range []struct {
		name       string
		overcommit int
		// stops is a pressure the gate must refuse, runs one it must allow
		stops, runs int
	}{
		{"leaving connections free", -4, -4, -5},
		{"filling the free connections", 0, 0, -1},
		{"queueing beyond them", 2, 2, 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			pressure := test.stops
			adaptiveparallelmergerresource.SetPrefetch(adaptiveparallelmergerresource.PrefetchSettings{
				Concurrency: 8,
				LeadTime:    time.Minute,
				MinLead:     4,
				MaxLead:     8,
				Pressure:    func() int { return pressure },
				Overcommit:  test.overcommit,
			})
			defer adaptiveparallelmergerresource.SetPrefetch(adaptiveparallelmergerresource.PrefetchSettings{})

			resources, counters := countingResources(20, 10)

			reader, err := adaptiveparallelmergerresource.NewAdaptiveParallelMergerResource(resources).Open()
			if err != nil {
				t.Fatalf("failed opening merger: %v", err)
			}
			defer reader.Close()

			if _, err := reader.Read(make([]byte, 5)); err != nil {
				t.Fatalf("failed reading: %v", err)
			}
			for i, counter := range counters {
				if got := counter.prefetchs.Load(); got != 0 {
					t.Fatalf("resource %d was prefetched %d times at pressure %d", i, got, pressure)
				}
			}

			// The pool has room again, so the next read fills the lead
			pressure = test.runs
			if _, err := reader.Read(make([]byte, 5)); err != nil {
				t.Fatalf("failed reading: %v", err)
			}
			waitFor(t, "the lead to be warmed once the pool had room", func() bool {
				return counters[3].prefetchs.Load() == 1
			})
		})
	}
}

func TestReadersAreOpenedLazilyAndClosedBehind(t *testing.T) {
	adaptiveparallelmergerresource.SetPrefetch(adaptiveparallelmergerresource.PrefetchSettings{})

	resources, counters := countingResources(20, 10)

	reader, err := adaptiveparallelmergerresource.NewAdaptiveParallelMergerResource(resources).Open()
	if err != nil {
		t.Fatalf("failed opening merger: %v", err)
	}
	defer reader.Close()

	if got := counters[19].opens.Load(); got != 0 {
		t.Errorf("Open touched the last resource %d times, expected it to wait for a read", got)
	}

	// Read across the first three resources
	if _, err := io.ReadFull(reader, make([]byte, 25)); err != nil {
		t.Fatalf("failed reading: %v", err)
	}

	if got := counters[19].opens.Load(); got != 0 {
		t.Errorf("reading the first 25 bytes opened the last resource %d times", got)
	}
	waitFor(t, "the readers behind the read head to be closed", func() bool {
		return counters[0].closes.Load() == 1 && counters[1].closes.Load() == 1
	})
	if got := counters[2].closes.Load(); got != 0 {
		t.Errorf("the resource at the read head was closed %d times", got)
	}
}
