package nntpclient

import (
	"sync/atomic"
	"testing"
	"time"
)

// timedServer answers after a delay, which is what the pool measures it by.
type timedServer struct {
	delay time.Duration
	size  int
	calls atomic.Int64
}

func (t *timedServer) GetSegment(_, _ string) ([]byte, error) {
	t.calls.Add(1)
	time.Sleep(t.delay)
	return make([]byte, t.size), nil
}

func (t *timedServer) SegmentExists(string) (bool, error) { return true, nil }
func (t *timedServer) Conns() int                         { return 10 }
func (t *timedServer) OpenConns() int                     { return 0 }

func TestPoolPrefersTheFasterServerOfAPriority(t *testing.T) {
	fast := &timedServer{delay: time.Millisecond, size: 716800}
	slow := &timedServer{delay: 30 * time.Millisecond, size: 716800}
	pool := NewPool([]ServerConfig{
		{Server: fast, Name: "fast", Priority: 1},
		{Server: slow, Name: "slow", Priority: 1},
	}, nil, BreakerConfig{})

	for range 12 {
		if _, err := pool.GetSegment("group", "id"); err != nil {
			t.Fatal(err)
		}
	}

	// The first requests spread while nothing is measured yet, so the slow one
	// is expected to have served a couple before it stops being chosen
	if slow.calls.Load() > 3 {
		t.Fatalf("sent %d of 12 to the slow server; want it dropped once measured", slow.calls.Load())
	}
	if fast.calls.Load() < 9 {
		t.Fatalf("sent %d of 12 to the fast server; want the rest", fast.calls.Load())
	}
}

func TestCostRisesWithOutstandingArticles(t *testing.T) {
	pool := &Pool{meanSize: 716800}
	idle := &poolServer{ServerConfig: ServerConfig{Server: &fakeServer{}}, load: load{fixed: 0.05, rate: 10e6, at: time.Now()}}
	busy := &poolServer{ServerConfig: ServerConfig{Server: &fakeServer{}}, load: load{fixed: 0.05, rate: 10e6, at: time.Now()}}
	// fakeServer allows 10 connections, so this is three deep into them
	busy.inflight.Store(30)

	if pool.cost(idle, load{}) >= pool.cost(busy, load{}) {
		t.Fatalf("idle %v is not cheaper than busy %v", pool.cost(idle, load{}), pool.cost(busy, load{}))
	}

	// One article of 716800 bytes at 10 MB/s after 50ms of latency
	if got, want := pool.cost(idle, load{}), 0.05+0.0716800; got < want*0.99 || got > want*1.01 {
		t.Fatalf("cost %v; want about %v", got, want)
	}
}

func TestUnmeasuredServerCostsWhatItsGroupDoes(t *testing.T) {
	pool := &Pool{meanSize: 716800}
	measured := &poolServer{ServerConfig: ServerConfig{Server: &fakeServer{}}, load: load{fixed: 0.05, rate: 10e6, at: time.Now()}}
	fresh := &poolServer{ServerConfig: ServerConfig{Server: &fakeServer{}}}
	pr := &priority{servers: []*poolServer{measured, fresh}}

	fallback := pool.groupLoad(pr)
	if got, want := pool.cost(fresh, fallback), pool.cost(measured, fallback); got != want {
		t.Fatalf("an unmeasured server costs %v against the groups %v", got, want)
	}
}

// A server priced out of its group stops being asked anything, so what priced it
// out has to expire or it never gets the request that would say it recovered.
func TestPricedOutServerIsTriedAgainOnceItsMeasurementExpires(t *testing.T) {
	pool := &Pool{meanSize: 716800}
	quick := &poolServer{ServerConfig: ServerConfig{Server: &fakeServer{}}, load: load{fixed: 0.01, rate: 10e6, at: time.Now()}}
	exiled := &poolServer{ServerConfig: ServerConfig{Server: &fakeServer{}}, load: load{fixed: 5, rate: 1e4, at: time.Now().Add(-2 * loadStale)}}
	pr := &priority{servers: []*poolServer{quick, exiled}}

	fallback := pool.groupLoad(pr)
	if got, want := pool.cost(exiled, fallback), pool.cost(quick, fallback); got != want {
		t.Fatalf("an expired measurement still costs %v against the groups %v", got, want)
	}
}
