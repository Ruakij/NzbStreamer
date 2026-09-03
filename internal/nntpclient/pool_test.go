package nntpclient

import (
	"errors"
	"testing"
	"time"
)

// fakeServer answers every request the same way and counts what it was sent.
type fakeServer struct {
	body  []byte
	err   error
	calls int
}

func (f *fakeServer) GetSegment(_, _ string) ([]byte, error) {
	f.calls++
	return f.body, f.err
}

func (f *fakeServer) SegmentExists(_ string) (bool, error) {
	f.calls++
	return f.err == nil, f.err
}

func (f *fakeServer) Waiting() int { return 1 }
func (f *fakeServer) Conns() int   { return 10 }

func TestPoolDescendsOnNotFound(t *testing.T) {
	primary := &fakeServer{err: ErrArticleNotFound}
	backup := &fakeServer{body: []byte("article")}
	pool := NewPool([]ServerConfig{
		{Server: primary, Name: "primary", Priority: 1},
		{Server: backup, Name: "backup", Priority: 2},
	}, nil, BreakerConfig{})

	body, err := pool.GetSegment("group", "id")
	if err != nil || string(body) != "article" {
		t.Fatalf("got %q, %v; want the backups article", body, err)
	}
	if primary.calls != 1 || backup.calls != 1 {
		t.Fatalf("tried primary %d times and backup %d; want 1 each", primary.calls, backup.calls)
	}

	exists, err := pool.SegmentExists("id")
	if err != nil || !exists {
		t.Fatalf("got %v, %v; want the backups yes", exists, err)
	}
}

func TestPoolReportsFailureRatherThanNotFound(t *testing.T) {
	pool := NewPool([]ServerConfig{
		{Server: &fakeServer{err: ErrArticleNotFound}, Name: "primary", Priority: 1},
		{Server: &fakeServer{err: errors.New("connection reset")}, Name: "backup", Priority: 2},
	}, nil, BreakerConfig{})

	if _, err := pool.GetSegment("group", "id"); err == nil || errors.Is(err, ErrArticleNotFound) {
		t.Fatalf("got %v; want the failure, since the article is undecided", err)
	}
}

func TestPoolStopsOnAuthFailure(t *testing.T) {
	block := &fakeServer{body: []byte("article")}
	pool := NewPool([]ServerConfig{
		{Server: &fakeServer{err: ErrAuthFailed}, Name: "primary", Priority: 1},
		{Server: block, Name: "block", Priority: 2},
	}, nil, BreakerConfig{})

	if _, err := pool.GetSegment("group", "id"); !errors.Is(err, ErrAuthFailed) {
		t.Fatalf("got %v; want the auth failure", err)
	}
	if block.calls != 0 {
		t.Fatalf("spent the block account on a credentials problem")
	}
}

func TestPoolRoundRobinsWithinAPriority(t *testing.T) {
	first := &fakeServer{body: []byte("a")}
	second := &fakeServer{body: []byte("b")}
	pool := NewPool([]ServerConfig{
		{Server: first, Name: "first", Priority: 1},
		{Server: second, Name: "second", Priority: 1},
	}, nil, BreakerConfig{})

	for range 4 {
		if _, err := pool.GetSegment("group", "id"); err != nil {
			t.Fatal(err)
		}
	}
	if first.calls != 2 || second.calls != 2 {
		t.Fatalf("spread %d/%d; want 2/2", first.calls, second.calls)
	}
	if got := pool.Conns(); got != 20 {
		t.Fatalf("Conns() = %d; want both servers of the group", got)
	}
}

// fakeQuotaStore is the persistence a quota needs to survive a restart.
type fakeQuotaStore struct {
	used  int64
	start time.Time
}

func (f *fakeQuotaStore) ServerUsage(string) (int64, time.Time, error) {
	return f.used, f.start, nil
}

func (f *fakeQuotaStore) RecordServerUsage(_ string, used int64, start time.Time) {
	f.used, f.start = used, start
}

func TestPoolSkipsAnExhaustedServer(t *testing.T) {
	metered := &fakeServer{body: []byte("0123456789")}
	backup := &fakeServer{body: []byte("backup")}
	store := &fakeQuotaStore{}
	pool := NewPool([]ServerConfig{
		{Server: metered, Name: "metered", Priority: 1, QuotaBytes: 8, QuotaPeriod: time.Hour},
		{Server: backup, Name: "backup", Priority: 2},
	}, store, BreakerConfig{})

	if _, err := pool.GetSegment("group", "id"); err != nil {
		t.Fatal(err)
	}
	if store.used != 10 {
		t.Fatalf("counted %d bytes; want the 10 it served", store.used)
	}

	body, err := pool.GetSegment("group", "id")
	if err != nil || string(body) != "backup" {
		t.Fatalf("got %q, %v; want the backup once the quota is spent", body, err)
	}
	if got := pool.Conns(); got != 10 {
		t.Fatalf("Conns() = %d; want only the group still in play", got)
	}
}

func TestPoolRestoresAndResetsQuota(t *testing.T) {
	metered := &fakeServer{body: []byte("article")}
	store := &fakeQuotaStore{used: 100, start: time.Now()}
	config := ServerConfig{Server: metered, Name: "metered", Priority: 1, QuotaBytes: 50, QuotaPeriod: time.Hour}

	if _, err := NewPool([]ServerConfig{config}, store, BreakerConfig{}).GetSegment("group", "id"); !errors.Is(err, ErrNoServer) {
		t.Fatalf("got %v; want a spent quota to leave no server", err)
	}

	store.start = time.Now().Add(-2 * time.Hour)
	if _, err := NewPool([]ServerConfig{config}, store, BreakerConfig{}).GetSegment("group", "id"); err != nil {
		t.Fatalf("got %v; want the period past its length to reset the count", err)
	}
}

func TestPoolDisablesAServerThatKeepsFailing(t *testing.T) {
	broken := &fakeServer{err: errors.New("connection refused")}
	backup := &fakeServer{body: []byte("backup")}
	pool := NewPool([]ServerConfig{
		{Server: broken, Name: "broken", Priority: 1},
		{Server: backup, Name: "backup", Priority: 2},
	}, nil, BreakerConfig{Failures: 2, Cooldown: time.Hour})

	for range 4 {
		if _, err := pool.GetSegment("group", "id"); err != nil {
			t.Fatal(err)
		}
	}
	if broken.calls != 2 {
		t.Fatalf("tried the broken server %d times; want it disabled after 2 failures", broken.calls)
	}
	if got := pool.Conns(); got != 10 {
		t.Fatalf("Conns() = %d; want only the group still in play", got)
	}
}

func TestPoolDisablesRejectedCredentialsAtOnce(t *testing.T) {
	broken := &fakeServer{err: ErrAuthFailed}
	backup := &fakeServer{body: []byte("backup")}
	pool := NewPool([]ServerConfig{
		{Server: broken, Name: "broken", Priority: 1},
		{Server: backup, Name: "backup", Priority: 2},
	}, nil, BreakerConfig{Failures: 3, Cooldown: time.Hour})

	if _, err := pool.GetSegment("group", "id"); !errors.Is(err, ErrAuthFailed) {
		t.Fatalf("got %v; want the auth failure reported", err)
	}
	body, err := pool.GetSegment("group", "id")
	if err != nil || string(body) != "backup" {
		t.Fatalf("got %q, %v; want the backup while the credentials are disabled", body, err)
	}
	if broken.calls != 1 {
		t.Fatalf("tried the server with bad credentials %d times", broken.calls)
	}
}

func TestPoolKeepsRejectedCredentialsDisabled(t *testing.T) {
	broken := &fakeServer{err: ErrAuthFailed}
	pool := NewPool([]ServerConfig{{Server: broken, Name: "broken", Priority: 1}}, nil,
		BreakerConfig{Failures: 3, Cooldown: time.Millisecond})

	if _, err := pool.GetSegment("group", "id"); !errors.Is(err, ErrAuthFailed) {
		t.Fatal(err)
	}

	time.Sleep(5 * time.Millisecond)
	_, err := pool.GetSegment("group", "id")
	if !errors.Is(err, ErrNoServer) || !errors.Is(err, ErrAuthFailed) {
		t.Fatalf("got %v; want no server, and what it was disabled for", err)
	}
	if broken.calls != 1 {
		t.Fatalf("tried it %d times; want credentials to stay disabled past any cooldown", broken.calls)
	}
}

func TestPoolReenablesAfterCooldown(t *testing.T) {
	broken := &fakeServer{err: errors.New("connection refused")}
	pool := NewPool([]ServerConfig{{Server: broken, Name: "broken", Priority: 1}}, nil,
		BreakerConfig{Failures: 1, Cooldown: 20 * time.Millisecond})

	if _, err := pool.GetSegment("group", "id"); err == nil {
		t.Fatal("want the failure reported")
	}
	if _, err := pool.GetSegment("group", "id"); !errors.Is(err, ErrNoServer) {
		t.Fatalf("got %v; want the server disabled for its cooldown", err)
	}

	time.Sleep(30 * time.Millisecond)
	broken.err = nil
	broken.body = []byte("article")
	if _, err := pool.GetSegment("group", "id"); err != nil {
		t.Fatalf("got %v; want the cooldown to let a request through", err)
	}
}

// The accounts connection limit is this process using more than it may, so it is
// not the servers fault and taking the server out would only make it worse.
func TestPoolKeepsAServerThatIsOutOfConnections(t *testing.T) {
	busy := &fakeServer{err: ErrTooManyConnections}
	pool := NewPool([]ServerConfig{{Server: busy, Name: "busy", Priority: 1}}, nil,
		BreakerConfig{Failures: 1, Cooldown: time.Hour})

	for range 3 {
		if _, err := pool.GetSegment("group", "id"); !errors.Is(err, ErrTooManyConnections) {
			t.Fatalf("got %v; want the failure reported every time", err)
		}
	}
	if busy.calls != 3 {
		t.Fatalf("tried it %d times; want it kept in rotation", busy.calls)
	}
}
