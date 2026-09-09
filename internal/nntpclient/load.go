package nntpclient

import (
	"context"
	"log/slog"
	"time"

	"go.opentelemetry.io/otel/metric"
)

// Load-based selection within a priority group: a request goes to the server
// whose outstanding work plus this article is expected to finish first.
//
// The estimate is per-server, of the form fixed + size/rate. Both terms are
// measured rather than configured, since a providers latency and throughput
// depend on the route to it and change with the time of day.
//
// The size is what the pool has been serving lately, pool-wide: it describes the
// workload rather than any one server, and it is the term that decides whether a
// low-latency server or a high-throughput one is the better answer. Segment
// sizes are uniform within a release and differ across them, so it follows the
// release being read.

const (
	// loadAlpha weights the newest sample in the averages. Low enough that one
	// slow article does not move a server, high enough to follow one going bad
	// within a handful of requests.
	loadAlpha = 0.2
	// loadDeadband is how much cheaper another server has to look before the
	// rotation is broken for it. Articles vary in how long they take by more
	// than two comparable servers differ, and letting that decide would send
	// everything to whichever one answered its last one fastest. A quarter is
	// past what that noise reaches and well under what a server worth avoiding
	// costs.
	loadDeadband = 0.25
)

// loadStale is how long a measurement stands for. Being priced out of a group
// stops a server being asked anything, which would otherwise leave the estimate
// that priced it out as the last word forever - a server that has recovered
// never gets the request that would say so. Past this it counts as unmeasured
// again and comes back on its turn in the rotation, at the cost of one article
// finding out.
const loadStale = time.Minute

// load is what a server has been costing lately. A rate of 0, or one older than
// loadStale, is a server nothing is currently known about.
type load struct {
	// fixed is the seconds a request costs before any bytes move
	fixed float64
	// rate is the bytes per second once they do
	rate float64
	// at is when the last sample landed
	at time.Time
}

// current reports whether a measurement is one to still go by.
func (l load) current() bool {
	return l.rate > 0 && time.Since(l.at) < loadStale
}

// recordLoad folds one answered request into a servers averages.
//
// An article that was missing transferred nothing, so its whole duration is the
// fixed cost - the one sample that separates the two terms. Failures are left
// out: a timeout says how long this pool waits, not what the server costs.
func (p *Pool) recordLoad(s *poolServer, seconds float64, bytes int64) {
	if seconds <= 0 {
		return
	}

	p.loadMutex.Lock()
	defer p.loadMutex.Unlock()

	if bytes <= 0 {
		s.load.fixed = ewma(s.load.fixed, seconds)
		return
	}
	s.load.at = time.Now()

	// A request quicker than the fixed cost says that cost is currently too high
	// rather than that these bytes moved instantly
	transfer := seconds - s.load.fixed
	if transfer <= 0 {
		transfer = seconds
	}

	s.load.rate = ewma(s.load.rate, float64(bytes)/transfer)
	p.meanSize = ewma(p.meanSize, float64(bytes))
}

func ewma(old, sample float64) float64 {
	if old <= 0 {
		return sample
	}
	return old + loadAlpha*(sample-old)
}

// pick chooses which server of a group to try first. The caller holds no lock.
//
// It starts from the round robin position and moves only for a server that is
// meaningfully cheaper, so a pool whose servers perform alike spreads across
// them as it always did, and a cold pool - where nothing is measured and every
// cost is zero - is round robin exactly.
func (p *Pool) pick(pr *priority) int {
	if len(pr.servers) == 1 {
		return 0
	}

	start := pr.start()

	p.loadMutex.Lock()
	fallback := p.groupLoad(pr)
	best, bestCost := start, p.cost(pr.servers[start], fallback)
	for i := 1; i < len(pr.servers); i++ {
		index := (start + i) % len(pr.servers)
		if cost := p.cost(pr.servers[index], fallback); cost < bestCost*(1-loadDeadband) {
			best, bestCost = index, cost
		}
	}
	p.loadMutex.Unlock()

	if best != start {
		selectionDisplaced.Add(recordCtx, 1, metric.WithAttributes(serverKey.String(pr.servers[start].Name)))
	}

	return best
}

// observeLoad reports what each server is expected to cost right now, which is
// the number every routing decision is made on. It is a gauge rather than
// something derived from the latency histograms because the in-flight articles
// are in it, and those are gone by the time a request has been measured.
func (p *Pool) observeLoad() {
	_, err := meter.RegisterCallback(func(_ context.Context, observer metric.Observer) error {
		p.loadMutex.Lock()
		defer p.loadMutex.Unlock()

		for _, pr := range p.priorities {
			fallback := p.groupLoad(pr)
			for _, s := range pr.servers {
				server := metric.WithAttributes(serverKey.String(s.Name))
				observer.ObserveFloat64(selectionCost, p.cost(s, fallback), server)
				observer.ObserveInt64(selectionInflight, s.inflight.Load(), server)
			}
		}
		return nil
	}, selectionCost, selectionInflight)
	if err != nil {
		slog.Warn("Failed registering the nntp selection metrics", "error", err)
	}
}

// cost is the seconds a request is expected to take on a server: what is queued
// ahead of it and itself, each an article of the size the pool has been serving.
//
// Counting what is in flight is what makes this load-based rather than a
// ranking: a server answering slowly right now accumulates requests, and its
// cost rises without anything having to model why it slowed down. They are
// divided by the connections the account allows, because that many are in
// progress at once - charging a request for all of them would make the largest
// account look like the worst one.
//
// The caller holds loadMutex.
func (p *Pool) cost(s *poolServer, fallback load) float64 {
	l := s.load
	if !l.current() {
		// A server with nothing measured is charged what its group costs, so it
		// is tried on its turn rather than singled out for being unknown
		l = fallback
	}
	if l.rate <= 0 {
		return 0
	}

	// Below the connection count nothing is waiting, so this is under 1 and the
	// cost is one articles worth
	queued := float64(s.inflight.Load()) / float64(max(s.Server.Conns(), 1))

	return (queued + 1) * (l.fixed + p.meanSize/l.rate)
}

// groupLoad averages the servers of a group that have been measured, which is
// what an unmeasured one stands in for. The caller holds loadMutex.
func (p *Pool) groupLoad(pr *priority) load {
	var mean load
	var measured float64

	for _, s := range pr.servers {
		if !s.load.current() {
			continue
		}
		mean.fixed += s.load.fixed
		mean.rate += s.load.rate
		measured++
	}

	if measured == 0 {
		return load{}
	}
	mean.fixed /= measured
	mean.rate /= measured

	return mean
}
