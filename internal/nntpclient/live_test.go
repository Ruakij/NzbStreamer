//go:build live

// These talk to a real news server and are excluded from the normal build. Run
// them with the servers credentials in the environment and an nzb to take
// message-ids from:
//
//	set -a; . .compose-test/.env; set +a
//	NNTP_LIVE_NZB=.compose-test/watch/some.nzb go test -tags live -v -timeout 20m ./internal/nntpclient/
package nntpclient

import (
	"errors"
	"io"
	"os"
	"strconv"
	"testing"
	"time"

	"git.ruekov.eu/ruakij/nzbStreamer/pkg/nzbparser"
)

func liveConfig(t *testing.T) Config {
	t.Helper()

	host := os.Getenv("USENET_HOST")
	if host == "" {
		t.Skip("USENET_HOST unset")
	}
	port, err := strconv.Atoi(os.Getenv("USENET_PORT"))
	if err != nil {
		t.Fatalf("USENET_PORT: %v", err)
	}
	useTLS, _ := strconv.ParseBool(os.Getenv("USENET_TLS"))

	return Config{
		Host:     host,
		Port:     port,
		TLS:      useTLS,
		User:     os.Getenv("USENET_USER"),
		Pass:     os.Getenv("USENET_PASS"),
		MaxConns: 1,
		Attempts: 1,
		Timeout:  30 * time.Second,
	}
}

// liveSegments returns the group and the first few message-ids of the largest
// file in the nzb, which is the sequential read the pool is built for. Files of
// one nzb are often posted to different groups, so they cannot be mixed.
func liveSegments(t *testing.T) (string, []string) {
	t.Helper()

	path := os.Getenv("NNTP_LIVE_NZB")
	if path == "" {
		t.Skip("NNTP_LIVE_NZB unset")
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open nzb: %v", err)
	}
	defer f.Close()

	nzb, err := nzbparser.ParseNzb(f, path)
	if err != nil {
		t.Fatalf("parse nzb: %v", err)
	}

	largest := -1
	for i, file := range nzb.Files {
		if len(file.Groups) == 0 {
			continue
		}
		if largest < 0 || len(file.Segments) > len(nzb.Files[largest].Segments) {
			largest = i
		}
	}
	if largest < 0 {
		t.Fatalf("nzb %s has no file with a group", path)
	}

	file := nzb.Files[largest]
	if len(file.Segments) < 2 {
		t.Fatalf("largest file in %s has %d segments, want at least 2", path, len(file.Segments))
	}
	ids := make([]string, 0, 6)
	for _, segment := range file.Segments[:min(6, len(file.Segments))] {
		ids = append(ids, segment.ID)
	}
	return file.Groups[0], ids
}

// The whole pool design rests on a connection serving more than one command, so
// this is the assumption itself: one connection, several articles, back to back.
func TestLiveOneConnectionServesManySegments(t *testing.T) {
	group, ids := liveSegments(t)

	client := New(liveConfig(t))
	dials := 0
	dial := client.dial
	client.dial = func() (*conn, error) {
		dials++
		return dial()
	}

	for i, id := range ids {
		start := time.Now()
		body, err := client.GetSegment(group, id)
		if err != nil {
			t.Fatalf("segment %d (%s): %v", i, id, err)
		}
		t.Logf("segment %d: %d bytes in %v, dials so far %d", i, len(body), time.Since(start).Round(time.Millisecond), dials)
	}

	if dials != 1 {
		t.Errorf("dialed %d times for %d segments, want 1: the connection is not being reused", dials, len(ids))
	}
}

// The second command on a connection already on the group skips GROUP, so this
// checks that the shortcut does not desynchronise the response stream.
func TestLiveGroupIsSelectedOnce(t *testing.T) {
	group, ids := liveSegments(t)

	client := New(liveConfig(t))
	cn, _, err := client.acquire()
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}

	for i, id := range ids[:2] {
		cn.deadline(client.config.Timeout)
		if err := cn.selectGroup(group); err != nil {
			t.Fatalf("selectGroup before segment %d: %v", i, err)
		}
		res, err := cn.Do("STAT <%s>", id)
		if err != nil {
			t.Fatalf("stat segment %d: %v", i, err)
		}
		t.Logf("segment %d: %d %s", i, res.Code, res.Message)
	}
	client.release(cn)
}

// A pause longer than the servers idle timeout must cost a dial, not a failed
// request. One attempt and no backoff, so the fetch can only succeed if the
// closed connection is recognised before a command is sent on it.
func TestLiveReuseSurvivesAPauseLongerThanTheServerAllows(t *testing.T) {
	group, ids := liveSegments(t)
	config := liveConfig(t)
	config.Attempts = 1
	client := New(config)

	dials := 0
	dial := client.dial
	client.dial = func() (*conn, error) {
		dials++
		return dial()
	}

	if _, err := client.GetSegment(group, ids[0]); err != nil {
		t.Fatalf("first segment: %v", err)
	}

	pause := 3*time.Minute + 30*time.Second
	t.Logf("idling %v, past the measured 3m the server allows", pause)
	time.Sleep(pause)

	// the dial count alone cannot tell the probe working from the stale retry
	// covering for it, since both end up dialing once more; acquire can
	cn, reused, err := client.acquire()
	if err != nil {
		t.Fatalf("acquire after the pause: %v", err)
	}
	if reused {
		t.Error("acquire handed out a connection the server had already closed")
	}
	client.release(cn)

	start := time.Now()
	body, err := client.GetSegment(group, ids[1])
	if err != nil {
		t.Fatalf("segment after %v idle: %v", pause, err)
	}
	t.Logf("segment after the pause: %d bytes in %v, %d dials total", len(body), time.Since(start).Round(time.Millisecond), dials)

	if dials != 2 {
		t.Errorf("dialed %d times, want 2: one connection, then its replacement", dials)
	}
}

// What the server does to a connection it is done waiting on, and when. A
// blocking read reports the close at the moment it happens and says which kind
// it was: a line then io.EOF is a graceful close, a reset is not. Nothing in the
// pool has a reader parked like this, which is why the close is only ever
// noticed by the next command.
func TestLiveIdleUntilServerCloses(t *testing.T) {
	_, ids := liveSegments(t)
	config := liveConfig(t)
	client := New(config)

	cn, err := client.dial()
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer cn.net.Close()

	cn.deadline(config.Timeout)
	if _, err := cn.Do("STAT <%s>", ids[0]); err != nil {
		t.Fatalf("priming stat: %v", err)
	}

	cn.net.SetDeadline(time.Now().Add(30 * time.Minute))
	idleSince := time.Now()
	buf := make([]byte, 256)
	for {
		n, err := cn.net.Read(buf)
		idle := time.Since(idleSince).Round(time.Second)
		if n > 0 {
			t.Logf("idle %v: server sent %q", idle, buf[:n])
		}
		if err != nil {
			t.Logf("idle %v: read ended with %v (io.EOF means the server sent FIN)", idle, err)
			if errors.Is(err, io.EOF) {
				t.Logf("graceful close after %v idle", idle)
			}
			return
		}
	}
}

// How long the server leaves an unused connection open, which is what
// IdleTimeout has to stay under. One connection per step, all opened at the
// start, each probed after its own idle time, so the whole table costs the
// longest step rather than their sum.
func TestLiveIdleTimeout(t *testing.T) {
	_, ids := liveSegments(t)
	config := liveConfig(t)
	config.MaxConns = 8
	client := New(config)

	steps := []time.Duration{30 * time.Second, time.Minute, 2 * time.Minute, 3 * time.Minute, 4 * time.Minute, 5 * time.Minute}

	conns := make([]*conn, 0, len(steps))
	for range steps {
		cn, err := client.dial()
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		// a connection the server has never seen a command on may be treated
		// differently from one that has been used, so use it
		cn.deadline(config.Timeout)
		if _, err := cn.Do("STAT <%s>", ids[0]); err != nil {
			t.Fatalf("priming stat: %v", err)
		}
		conns = append(conns, cn)
	}
	t.Logf("opened %d connections", len(conns))

	opened := time.Now()
	survived := time.Duration(0)
	for i, step := range steps {
		time.Sleep(time.Until(opened.Add(step)))

		conns[i].deadline(config.Timeout)
		res, err := conns[i].Do("STAT <%s>", ids[0])
		if err != nil {
			t.Logf("idle %v: dead (%v)", step, err)
		} else {
			t.Logf("idle %v: alive (%d %s)", step, res.Code, res.Message)
			survived = step
		}
		conns[i].net.Close()
	}

	t.Logf("longest idle time a connection survived: %v", survived)
	if survived == 0 {
		t.Errorf("no connection survived even %v idle, so pooling them is pointless", steps[0])
	}
}
