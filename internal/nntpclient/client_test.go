package nntpclient

import (
	"errors"
	"net"
	"testing"
	"time"
)

var errTransient = errors.New("connection reset")

// withFakeDial replaces dialing with a connection that is never spoken to, so
// the pool bookkeeping can be exercised without a server.
func withFakeDial(client *Client) *int {
	dialed := 0
	client.dial = func() (*conn, error) {
		dialed++
		_, netConn := net.Pipe()
		return &conn{net: netConn}, nil
	}
	return &dialed
}

// finishes reports whether fn returns within a second. Every failure mode of
// slot accounting is a permanent block, so a timeout is what a broken pool
// looks like.
func finishes(fn func()) bool {
	done := make(chan struct{})
	go func() {
		fn()
		close(done)
	}()

	select {
	case <-done:
		return true
	case <-time.After(time.Second):
		return false
	}
}

// A connection lost to a failed command has to give its slot back, or a client
// that hits MaxConns failures can never open a connection again.
func TestDropReturnsTheSlot(t *testing.T) {
	client := New(Config{MaxConns: 1})
	withFakeDial(client)

	ok := finishes(func() {
		for range 3 {
			cn, _, err := client.acquire()
			if err != nil {
				t.Errorf("acquire: %v", err)
				return
			}
			client.drop(cn)
		}
	})
	if !ok {
		t.Fatal("acquire blocked after a dropped connection, its slot was not returned")
	}
}

func TestReleasedConnectionIsReused(t *testing.T) {
	client := New(Config{MaxConns: 2})
	dialed := withFakeDial(client)

	for range 3 {
		cn, _, err := client.acquire()
		if err != nil {
			t.Fatalf("acquire: %v", err)
		}
		client.release(cn)
	}

	if *dialed != 1 {
		t.Errorf("dialed %d times, want 1", *dialed)
	}
}

// The first connection acquire hands out is a fresh one, every later one it
// takes from the idle pool is a reuse. Only the latter can have been closed by
// the server behind our back.
func TestAcquireReportsReuse(t *testing.T) {
	client := New(Config{MaxConns: 1})
	withFakeDial(client)

	cn, reused, err := client.acquire()
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if reused {
		t.Error("a freshly dialed connection was reported as reused")
	}
	client.release(cn)

	if _, reused, _ = client.acquire(); !reused {
		t.Error("a connection taken from the idle pool was reported as fresh")
	}
}

// A burst of reads must not hold its connections open forever, and the pool has
// to keep working once they are gone.
func TestIdleConnectionsAreReaped(t *testing.T) {
	client := New(Config{MaxConns: 2, IdleTimeout: 20 * time.Millisecond})
	dialed := withFakeDial(client)

	// both at once, so the second is a fresh dial rather than a reuse
	first, _, err := client.acquire()
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	second, _, err := client.acquire()
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	client.release(first)
	client.release(second)

	time.Sleep(200 * time.Millisecond)
	if len(client.idle) != 0 {
		t.Errorf("%d connections still idle, want 0", len(client.idle))
	}

	if _, reused, _ := client.acquire(); reused {
		t.Error("handed out a reaped connection")
	}
	if *dialed != 3 {
		t.Errorf("dialed %d times, want 3", *dialed)
	}
}

func TestAcquireBlocksAtMaxConns(t *testing.T) {
	client := New(Config{MaxConns: 1})
	withFakeDial(client)

	held, _, err := client.acquire()
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}

	if finishes(func() { client.acquire() }) { //nolint:errcheck // blocking is the assertion
		t.Fatal("acquired a second connection while MaxConns is 1")
	}
	client.release(held)
}

// A failed dial must not keep the slot it took, otherwise an unreachable server
// leaves the client unable to recover once it comes back.
func TestFailedDialReturnsTheSlot(t *testing.T) {
	client := New(Config{MaxConns: 1})
	client.dial = func() (*conn, error) { return nil, errTransient }

	ok := finishes(func() {
		for range 3 {
			if _, _, err := client.acquire(); !errors.Is(err, errTransient) {
				t.Errorf("acquire = %v, want errTransient", err)
				return
			}
		}
	})
	if !ok {
		t.Fatal("acquire blocked after a failed dial, its slot was not returned")
	}
}

func TestRetryGivesUpAfterAttempts(t *testing.T) {
	client := New(Config{Attempts: 3, Backoff: time.Millisecond})

	calls := 0
	err := client.retry("test", func() error {
		calls++
		return errTransient
	})

	if calls != 3 {
		t.Errorf("tried %d times, want 3", calls)
	}
	if !errors.Is(err, errTransient) {
		t.Errorf("err = %v, want it to wrap errTransient", err)
	}
}

func TestRetryStopsOnSuccess(t *testing.T) {
	client := New(Config{Attempts: 3, Backoff: time.Millisecond})

	calls := 0
	err := client.retry("test", func() error {
		calls++
		if calls < 2 {
			return errTransient
		}
		return nil
	})

	if err != nil {
		t.Errorf("err = %v, want nil", err)
	}
	if calls != 2 {
		t.Errorf("tried %d times, want 2", calls)
	}
}

// A missing article is the servers final answer, so spending attempts on it
// only delays reporting it.
func TestRetrySkipsMissingArticle(t *testing.T) {
	client := New(Config{Attempts: 3, Backoff: time.Second})

	calls := 0
	err := client.retry("test", func() error {
		calls++
		return ErrArticleNotFound
	})

	if calls != 1 {
		t.Errorf("tried %d times, want 1", calls)
	}
	if !errors.Is(err, ErrArticleNotFound) {
		t.Errorf("err = %v, want ErrArticleNotFound", err)
	}
}

// Reusing a connection the server had already closed is the normal cost of a
// pause in reading, so the attempt it wastes is given back and no backoff is
// waited. The second one is not, because by then the failure is the requests.
func TestRetryReplacesAStaleConnectionForFree(t *testing.T) {
	// A single attempt, so a backoff long enough to hang the test is only ever
	// waited if the free replacement wrongly counts as one.
	client := New(Config{Attempts: 1, Backoff: time.Hour})

	calls := 0
	err := client.retry("test", func() error {
		calls++
		return staleIf(true, errTransient)
	})

	if calls != 2 {
		t.Errorf("tried %d times, want 2: the replacement plus the one attempt", calls)
	}
	if !errors.Is(err, errTransient) {
		t.Errorf("err = %v, want it to wrap errTransient", err)
	}
}
