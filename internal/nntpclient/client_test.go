package nntpclient

import (
	"errors"
	"io"
	"net"
	"testing"
	"time"
)

var errTransient = errors.New("connection reset")

// loopback returns a connected tcp pair. The liveness probe reads the socket
// descriptor itself, so a pipe or any other in-memory stand-in answers nothing
// like a real connection does and would hide a probe that never looks.
func loopback(t testing.TB) (client, server net.Conn) {
	t.Helper()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer listener.Close()

	accepted := make(chan net.Conn, 1)
	go func() {
		cn, err := listener.Accept()
		if err != nil {
			close(accepted)
			return
		}
		accepted <- cn
	}()

	client, err = net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	server = <-accepted
	if server == nil {
		t.Fatal("accept failed")
	}
	t.Cleanup(func() { client.Close(); server.Close() })
	return client, server
}

// withFakeDial replaces dialing with a connection that is never spoken to, so
// the pool bookkeeping can be exercised without a server.
func withFakeDial(t testing.TB, client *Client) *int {
	dialed, _ := withPeerTrackingDial(t, client)
	return dialed
}

// withPeerTrackingDial is withFakeDial but it keeps both ends of each
// connection, so a test can hang up on the client the way a news server does.
func withPeerTrackingDial(t testing.TB, client *Client) (dialed *int, peers *[]net.Conn) {
	count := 0
	kept := []net.Conn{}
	client.dial = func() (*conn, error) {
		count++
		netConn, peer := loopback(t)
		kept = append(kept, peer)
		return &conn{net: netConn}, nil
	}
	return &count, &kept
}

// hangUp closes the server end and waits for the client to have the close, so a
// test asserting on what the probe sees is not racing the network stack. The
// read consumes nothing, since a close carries no bytes.
func hangUp(t testing.TB, peer net.Conn, cn *conn) {
	t.Helper()

	peer.Close()
	if err := cn.net.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatalf("deadline: %v", err)
	}
	var b [1]byte
	if _, err := cn.net.Read(b[:]); !errors.Is(err, io.EOF) {
		t.Fatalf("waiting for the peers close: %v, want EOF", err)
	}
	if err := cn.net.SetReadDeadline(time.Time{}); err != nil {
		t.Fatalf("clearing deadline: %v", err)
	}
}

// A connection the server closed while it sat idle has to be recognised before a
// command is sent on it, or every pause in reading costs a failed request.
func TestAcquireSkipsAConnectionTheServerClosed(t *testing.T) {
	client := New(Config{MaxConns: 2})
	dialed, peers := withPeerTrackingDial(t, client)

	cn, reused, err := client.acquire()
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	if reused {
		t.Error("a freshly dialed connection was reported as reused")
	}
	client.release(cn)

	hangUp(t, (*peers)[0], cn)

	cn, reused, err = client.acquire()
	if err != nil {
		t.Fatalf("second acquire: %v", err)
	}
	if reused {
		t.Error("handed out a connection the server had already closed")
	}
	if *dialed != 2 {
		t.Errorf("dialed %d times, want 2: the dead connection should have been replaced", *dialed)
	}
	client.release(cn)
}

// The probe must leave a live connection exactly as it found it: it reads the
// descriptor the next command reads, so consuming a byte or leaving a deadline
// behind would break that command instead of the previous one.
func TestAliveLeavesALiveConnectionUsable(t *testing.T) {
	client := New(Config{MaxConns: 1})
	_, peers := withPeerTrackingDial(t, client)

	cn, _, err := client.acquire()
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	client.release(cn)

	cn, reused, err := client.acquire()
	if err != nil {
		t.Fatalf("second acquire: %v", err)
	}
	if !reused {
		t.Fatal("a live connection was not reused")
	}

	go func() { _, _ = (*peers)[0].Write([]byte("hi")) }()
	buf := make([]byte, 2)
	if _, err := cn.net.Read(buf); err != nil {
		t.Errorf("read after the probe: %v, want the probes deadline to be gone", err)
	}
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
	withFakeDial(t, client)

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
	dialed := withFakeDial(t, client)

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
	withFakeDial(t, client)

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

// An IdleTimeout longer than the servers own leaves connections dying in the
// pool, so the reaper drops the ones already hung up on rather than waiting out
// a timeout that will never be reached first.
func TestReaperDropsConnectionsTheServerClosed(t *testing.T) {
	client := New(Config{MaxConns: 2, IdleTimeout: time.Hour})
	_, peers := withPeerTrackingDial(t, client)

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

	hangUp(t, (*peers)[0], first)

	// the reaper is asleep until the first connection comes due, so run one pass
	// directly rather than waiting out the hour
	client.reapPass()

	if len(client.idle) != 1 {
		t.Errorf("%d connections idle, want 1: the closed one should be gone", len(client.idle))
	}
	cn := <-client.idle
	if cn != second {
		t.Error("the reaper kept the wrong connection")
	}
}

// A burst of reads must not hold its connections open forever, and the pool has
// to keep working once they are gone.
func TestIdleConnectionsAreReaped(t *testing.T) {
	client := New(Config{MaxConns: 2, IdleTimeout: 20 * time.Millisecond})
	dialed := withFakeDial(t, client)

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
	withFakeDial(t, client)

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
// waited. Past a poolful of them the connections are fresh and it is a failure.
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

// A pause in reading idles out every pooled connection at once, so a request
// walks through as many dead ones as the pool holds before it reaches a fresh
// one. None of them is the requests failure.
func TestRetrySurvivesAWholePoolOfStaleConnections(t *testing.T) {
	client := New(Config{MaxConns: 4, Attempts: 1, Backoff: time.Hour})

	calls := 0
	err := client.retry("test", func() error {
		calls++
		if calls <= client.config.MaxConns {
			return staleIf(true, errTransient)
		}
		return nil
	})

	if err != nil {
		t.Errorf("err = %v, want nil", err)
	}
	if calls != client.config.MaxConns+1 {
		t.Errorf("tried %d times, want %d: one per stale connection plus the one that worked", calls, client.config.MaxConns+1)
	}
}
