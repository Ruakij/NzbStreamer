package nntpclient

import (
	"bufio"
	"errors"
	"fmt"
	"hash/crc32"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

const testGroup = "alt.binaries.test"

// fakeNNTP is the other end of a pipelined connection. It never answers
// anything on its own, so a test decides exactly when each response is written
// and can observe how many commands the client sent ahead of them.
type fakeNNTP struct {
	t     *testing.T
	ln    net.Listener
	conns chan *fakeNNTPConn
	// greeting written to every accepted connection; the tests that care about
	// the handshake set their own.
	greeting string
}

type fakeNNTPConn struct {
	t    *testing.T
	net  net.Conn
	cmds chan string
}

func newFakeNNTP(t *testing.T) *fakeNNTP {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })

	s := &fakeNNTP{t: t, ln: ln, conns: make(chan *fakeNNTPConn, 8), greeting: "200 server ready\r\n"}
	go s.accept()
	return s
}

func (s *fakeNNTP) accept() {
	for {
		nc, err := s.ln.Accept()
		if err != nil {
			return
		}
		fc := &fakeNNTPConn{t: s.t, net: nc, cmds: make(chan string, 64)}
		fc.write(s.greeting)
		go fc.readCommands()
		s.conns <- fc
	}
}

// conn waits for the next connection the client opens.
func (s *fakeNNTP) conn() *fakeNNTPConn {
	s.t.Helper()
	select {
	case fc := <-s.conns:
		s.t.Cleanup(func() { fc.net.Close() })
		return fc
	case <-time.After(5 * time.Second):
		s.t.Fatal("client never connected")
		return nil
	}
}

// noConn fails if the client opens another connection within a short window.
func (s *fakeNNTP) noConn(what string) {
	s.t.Helper()
	select {
	case <-s.conns:
		s.t.Fatalf("%s: client opened another connection", what)
	case <-time.After(100 * time.Millisecond):
	}
}

// readCommands moves every command line the client writes into cmds, so the
// test can assert on what was sent without ever blocking the client.
func (fc *fakeNNTPConn) readCommands() {
	br := bufio.NewReader(fc.net)
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			close(fc.cmds)
			return
		}
		fc.cmds <- strings.TrimRight(line, "\r\n")
	}
}

func (fc *fakeNNTPConn) write(v string) {
	if _, err := fc.net.Write([]byte(v)); err != nil {
		panic(fmt.Sprintf("fake server write: %v", err))
	}
}

// next returns the next command line, failing the test if none arrives. The
// long wait is a hang tripwire, not a timing assumption.
func (fc *fakeNNTPConn) next() string {
	fc.t.Helper()
	select {
	case line, ok := <-fc.cmds:
		if !ok {
			fc.t.Fatal("connection closed while a command was expected")
		}
		return line
	case <-time.After(5 * time.Second):
		fc.t.Fatal("no command sent")
		return ""
	}
}

// article returns the message-id of the next command, which has to be an
// ARTICLE.
func (fc *fakeNNTPConn) article() string {
	fc.t.Helper()
	line := fc.next()
	id, ok := strings.CutPrefix(line, "ARTICLE <")
	if !ok || !strings.HasSuffix(id, ">") {
		fc.t.Fatalf("command %q, want an ARTICLE", line)
	}
	return strings.TrimSuffix(id, ">")
}

// carrier returns whichever of two connections the client sent want on, and
// the other. Connections dialled together are usable in whatever order their
// handshakes finish, which is not the order they were accepted.
func carrier(a, b *fakeNNTPConn, want string) (first, spare *fakeNNTPConn) {
	a.t.Helper()

	var line string
	select {
	case line = <-a.cmds:
		first, spare = a, b
	case line = <-b.cmds:
		first, spare = b, a
	case <-time.After(5 * time.Second):
		a.t.Fatal("no command sent")
	}
	if line != want {
		a.t.Fatalf("command %q, want %q", line, want)
	}
	return first, spare
}

func (fc *fakeNNTPConn) expect(want string) {
	fc.t.Helper()
	if line := fc.next(); line != want {
		fc.t.Fatalf("command %q, want %q", line, want)
	}
}

// silent fails if another command arrives, which is how a window bound is
// observed: the client is deterministically blocked when this is called, so
// anything appearing is the bound not holding.
func (fc *fakeNNTPConn) silent(what string) {
	fc.t.Helper()
	select {
	case line, ok := <-fc.cmds:
		if ok {
			fc.t.Fatalf("%s: expected no further command, got %q", what, line)
		}
	case <-time.After(100 * time.Millisecond):
	}
}

func (fc *fakeNNTPConn) groupOK(group string) {
	fc.write(fmt.Sprintf("211 0 0 0 %s\r\n", group))
}

func (fc *fakeNNTPConn) respond(data []byte) {
	fc.write("220 article retrieved\r\n" + yencArticle(data))
}

// yencArticle encodes data the way a news server sends an article: the yenc
// header, dot-stuffed encoded lines, the trailer with its crc, and the
// terminator.
func yencArticle(data []byte) string {
	var b strings.Builder
	line := make([]byte, 0, 152)

	flush := func() {
		if len(line) > 0 && line[0] == '.' {
			b.WriteByte('.')
		}
		b.Write(line)
		b.WriteString("\r\n")
		line = line[:0]
	}
	for _, v := range data {
		c := v + 42
		if c == 0 || c == '\n' || c == '\r' || c == '=' {
			line = append(line, '=', c+64)
		} else {
			line = append(line, c)
		}
		if len(line) >= 76 {
			flush()
		}
	}
	if len(line) > 0 {
		flush()
	}

	return fmt.Sprintf("=ybegin part=1 line=76 size=%d name=t.bin\r\n=ypart begin=1 end=%d\r\n", len(data), len(data)) +
		b.String() +
		fmt.Sprintf("=yend size=%d part=1 pcrc32=%08x\r\n.\r\n", len(data), crc32.ChecksumIEEE(data))
}

// pipelineClient builds a client whose pipelined connections reach s.
func pipelineClient(t *testing.T, s *fakeNNTP, cfg Config) *Client {
	t.Helper()

	if cfg.Attempts == 0 {
		cfg.Attempts = 1
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = 5 * time.Second
	}
	if cfg.IdleTimeout == 0 {
		cfg.IdleTimeout = time.Hour
	}

	c := New(cfg)
	if !c.canPipe {
		t.Fatalf("config %+v does not pipeline", cfg)
	}
	addr := s.ln.Addr().String()
	c.dialNet = func() (net.Conn, error) { return net.Dial("tcp", addr) }
	return c
}

// fetches runs GetSegment calls concurrently and collects what they returned.
type fetches struct {
	t  *testing.T
	c  *Client
	mu sync.Mutex
	wg sync.WaitGroup
	// bodies and errs are keyed by message-id
	bodies map[string][]byte
	errs   map[string]error
}

func newFetches(t *testing.T, c *Client) *fetches {
	return &fetches{t: t, c: c, bodies: map[string][]byte{}, errs: map[string]error{}}
}

func (f *fetches) start(id string) {
	f.wg.Add(1)
	go func() {
		defer f.wg.Done()
		body, err := f.c.GetSegment(testGroup, id)
		f.mu.Lock()
		defer f.mu.Unlock()
		f.bodies[id], f.errs[id] = body, err
	}()
}

func (f *fetches) wait() {
	f.t.Helper()
	done := make(chan struct{})
	go func() { f.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		f.t.Fatal("fetches never returned")
	}
}

func (f *fetches) assertBody(id, want string) {
	f.t.Helper()
	if f.errs[id] != nil {
		f.t.Fatalf("fetch %s: %v", id, f.errs[id])
	}
	if string(f.bodies[id]) != want {
		f.t.Fatalf("fetch %s: got %q, want %q", id, f.bodies[id], want)
	}
}

func (f *fetches) assertErr(id string) error {
	f.t.Helper()
	if f.errs[id] == nil {
		f.t.Fatalf("fetch %s succeeded, want an error", id)
	}
	return f.errs[id]
}

// TestPipelineFillsWindowAheadOfResponses is the point of the whole thing: with
// a window of 3, three articles are asked for before a single byte of response
// has arrived, and the fourth waits for a slot.
func TestPipelineFillsWindowAheadOfResponses(t *testing.T) {
	s := newFakeNNTP(t)
	c := pipelineClient(t, s, Config{MaxConns: 1, ConnectionPipeliningSize: 3})

	f := newFetches(t, c)
	for _, id := range []string{"a", "b", "c", "d"} {
		f.start(id)
	}

	fc := s.conn()
	// The group goes out in the same write as the first article, so it costs no
	// round trip of its own, and is not repeated for the rest.
	fc.expect("GROUP " + testGroup)
	sent := []string{fc.article(), fc.article(), fc.article()}
	fc.silent("a fourth article within a window of three")

	fc.groupOK(testGroup)
	for _, id := range sent {
		fc.respond([]byte("body-" + id))
	}

	// The fourth only goes out once one of the three has been answered.
	fourth := fc.article()
	fc.respond([]byte("body-" + fourth))

	f.wait()
	if len(sent)+1 != 4 {
		t.Fatalf("sent %v then %v", sent, fourth)
	}
	for _, id := range []string{"a", "b", "c", "d"} {
		f.assertBody(id, "body-"+id)
	}
}

// TestPipelineAnswersInOrder checks the FIFO the whole design rests on: the
// responses are matched to the commands by the order they were sent, so bodies
// written back in that order reach the right callers.
func TestPipelineAnswersInOrder(t *testing.T) {
	s := newFakeNNTP(t)
	c := pipelineClient(t, s, Config{MaxConns: 1, ConnectionPipeliningSize: 4})

	f := newFetches(t, c)
	for _, id := range []string{"a", "b", "c"} {
		f.start(id)
	}

	fc := s.conn()
	fc.expect("GROUP " + testGroup)
	fc.groupOK(testGroup)

	for range 3 {
		id := fc.article()
		fc.respond([]byte("body-" + id))
	}

	f.wait()
	for _, id := range []string{"a", "b", "c"} {
		f.assertBody(id, "body-"+id)
	}
}

// A 430 is the answer to one article and leaves the response stream where it
// was, so the connection carries on serving what was already asked for.
func TestPipelineMissingArticleKeepsConnection(t *testing.T) {
	s := newFakeNNTP(t)
	c := pipelineClient(t, s, Config{MaxConns: 1, ConnectionPipeliningSize: 3})

	f := newFetches(t, c)
	f.start("missing")

	fc := s.conn()
	fc.expect("GROUP " + testGroup)
	if id := fc.article(); id != "missing" {
		t.Fatalf("first article %q, want missing", id)
	}
	fc.groupOK(testGroup)

	f.start("present")
	if id := fc.article(); id != "present" {
		t.Fatalf("second article %q, want present", id)
	}
	fc.write("430 no such article\r\n")
	fc.respond([]byte("kept"))

	f.wait()
	if err := f.assertErr("missing"); !errors.Is(err, ErrArticleNotFound) {
		t.Fatalf("missing: %v, want ErrArticleNotFound", err)
	}
	f.assertBody("present", "kept")
	s.noConn("after a missing article")
}

// A rejected GROUP is the answer to the fetch that asked for it and nothing
// else, which matters because the group is only sent for one fetch in a burst.
func TestPipelineRejectedGroupFailsOnlyItsFetch(t *testing.T) {
	s := newFakeNNTP(t)
	c := pipelineClient(t, s, Config{MaxConns: 1, ConnectionPipeliningSize: 3})

	f := newFetches(t, c)
	f.start("a")

	fc := s.conn()
	fc.expect("GROUP " + testGroup)
	first := fc.article()

	f.start("b")
	second := fc.article()

	// The article command went out with the GROUP, so the server answers it
	// whatever it made of the group; reading it is what keeps the fetch behind
	// it lined up with its own response.
	fc.write("411 no such group\r\n")
	fc.write("412 no newsgroup selected\r\n")
	fc.respond([]byte("second"))

	f.wait()
	if err := f.assertErr(first); !errors.Is(err, ErrUnexpectedResponse) {
		t.Fatalf("%s: %v, want ErrUnexpectedResponse", first, err)
	}
	f.assertBody(second, "second")
}

// A connection lost mid-burst fails everything outstanding on it, because the
// position in the response stream is gone with it.
func TestPipelineLostConnectionFailsEverythingInflight(t *testing.T) {
	s := newFakeNNTP(t)
	c := pipelineClient(t, s, Config{MaxConns: 1, ConnectionPipeliningSize: 3})

	f := newFetches(t, c)
	f.start("a")

	fc := s.conn()
	fc.expect("GROUP " + testGroup)
	ids := []string{fc.article()}
	f.start("b")
	ids = append(ids, fc.article())

	fc.groupOK(testGroup)
	// half an article, then the connection goes: the body is cut short and the
	// stream is unusable for the one behind it too
	fc.write("220 article retrieved\r\n=ybegin part=1 line=76 size=99 name=t.bin\r\n")
	s.ln.Close()
	fc.net.Close()

	f.wait()
	for _, id := range ids {
		_ = f.assertErr(id)
	}
}

// A failure on a connection that has answered before is a server hanging up on
// an idle one, which the retry loop replaces without spending an attempt.
func TestPipelineFailureOnUsedConnectionIsStale(t *testing.T) {
	s := newFakeNNTP(t)
	c := pipelineClient(t, s, Config{MaxConns: 1, ConnectionPipeliningSize: 3})

	f := newFetches(t, c)
	f.start("a")

	fc := s.conn()
	fc.expect("GROUP " + testGroup)
	fc.groupOK(testGroup)
	fc.respond([]byte("first"))
	f.wait()
	f.assertBody("a", "first")

	// The retry replaces the connection rather than failing, so the second
	// attempt lands on a fresh one and succeeds.
	f = newFetches(t, c)
	f.start("b")
	fc.next() // the command that dies with the connection
	fc.net.Close()

	fc2 := s.conn()
	fc2.expect("GROUP " + testGroup)
	fc2.article()
	fc2.groupOK(testGroup)
	fc2.respond([]byte("second"))

	f.wait()
	f.assertBody("b", "second")
}

// A fetch takes a connection of its own rather than queueing behind a segment,
// because a provider shapes one connection and parallel ones are where the
// bandwidth is.
func TestPipelineSpreadsBeforeItDeepens(t *testing.T) {
	s := newFakeNNTP(t)
	c := pipelineClient(t, s, Config{MaxConns: 4, ConnectionPipeliningSize: 4})

	f := newFetches(t, c)

	// three fetches, none of them answered: each gets its own connection even
	// though every window has room for all three
	conns := make([]*fakeNNTPConn, 0, 3)
	for _, id := range []string{"a", "b", "c"} {
		f.start(id)
		fc := s.conn()
		fc.expect("GROUP " + testGroup)
		fc.article()
		conns = append(conns, fc)
	}

	for i, fc := range conns {
		fc.groupOK(testGroup)
		fc.respond([]byte(fmt.Sprintf("body-%d", i)))
	}

	f.wait()
	for _, id := range []string{"a", "b", "c"} {
		if f.errs[id] != nil {
			t.Fatalf("fetch %s: %v", id, f.errs[id])
		}
	}
}

// MinFreeConns dials ahead of demand, so the fetch that takes the last free
// connection leaves a warm one behind it.
func TestPipelineDialsAheadOfDemand(t *testing.T) {
	s := newFakeNNTP(t)
	c := pipelineClient(t, s, Config{MaxConns: 4, ConnectionPipeliningSize: 4, MinFreeConns: 1})

	f := newFetches(t, c)
	f.start("a")

	// both come up together; the fetch lands on whichever handshake finished
	// first, and the other is the spare that nothing has been given
	first, spare := carrier(s.conn(), s.conn(), "GROUP "+testGroup)
	first.article()

	first.groupOK(testGroup)
	first.respond([]byte("body"))
	f.wait()
	f.assertBody("a", "body")
	spare.silent("dialled ahead of demand")
}

// A connection that has answered is preferred to opening another, since it is
// warm and a handshake is not free.
func TestPipelinePrefersAnIdleConnectionToANewOne(t *testing.T) {
	s := newFakeNNTP(t)
	c := pipelineClient(t, s, Config{MaxConns: 4, ConnectionPipeliningSize: 4})

	f := newFetches(t, c)
	f.start("a")
	fc := s.conn()
	fc.expect("GROUP " + testGroup)
	fc.article()
	fc.groupOK(testGroup)
	fc.respond([]byte("body-a"))
	f.wait()
	f.assertBody("a", "body-a")

	f = newFetches(t, c)
	f.start("b")
	// the group is already selected on it, so only the article goes out
	if id := fc.article(); id != "b" {
		t.Fatalf("article %q, want b on the connection that is already there", id)
	}
	s.noConn("with a connection sitting idle")
	fc.respond([]byte("body-b"))

	f.wait()
	f.assertBody("b", "body-b")
}

// With every connection the account allows already open and carrying something,
// a fetch goes to the least loaded of them.
func TestPipelineChoosesTheLeastLoadedConnection(t *testing.T) {
	s := newFakeNNTP(t)
	// two connections and a window of two, so four fetches fill both
	c := pipelineClient(t, s, Config{MaxConns: 2, ConnectionPipeliningSize: 2})

	f := newFetches(t, c)
	f.start("a")
	first := s.conn()
	first.expect("GROUP " + testGroup)
	first.article()

	f.start("b")
	second := s.conn()
	second.expect("GROUP " + testGroup)
	second.article()

	// both connections carry one; the next two even them out at two each rather
	// than piling onto either
	f.start("c")
	f.start("d")
	first.article()
	second.article()
	s.noConn("past the account's connections")

	// a fifth lands on whichever answers first, which is the one with room
	f.start("e")
	first.groupOK(testGroup)
	first.respond([]byte("body"))
	if id := first.article(); id != "e" {
		t.Fatalf("article %q, want e on the connection that freed up", id)
	}

	first.respond([]byte("body"))
	first.respond([]byte("body"))
	second.groupOK(testGroup)
	second.respond([]byte("body"))
	second.respond([]byte("body"))

	f.wait()
	for _, id := range []string{"a", "b", "c", "d", "e"} {
		f.assertBody(id, "body")
	}
}

// The pipelined path speaks the handshake itself, so it has to authenticate
// itself too.
func TestPipelineAuthenticates(t *testing.T) {
	s := newFakeNNTP(t)
	c := pipelineClient(t, s, Config{
		MaxConns: 2, ConnectionPipeliningSize: 2,
		User: "user", Pass: "secret",
	})

	f := newFetches(t, c)
	f.start("a")

	fc := s.conn()
	fc.expect("AUTHINFO USER user")
	fc.write("381 password required\r\n")
	fc.expect("AUTHINFO PASS secret")
	fc.write("281 accepted\r\n")

	fc.expect("GROUP " + testGroup)
	fc.article()
	fc.groupOK(testGroup)
	fc.respond([]byte("authed"))

	f.wait()
	f.assertBody("a", "authed")
}

func TestPipelineRejectedCredentialsAreFinal(t *testing.T) {
	s := newFakeNNTP(t)
	c := pipelineClient(t, s, Config{
		MaxConns: 2, ConnectionPipeliningSize: 2, Attempts: 3, Backoff: time.Millisecond,
		User: "user", Pass: "wrong",
	})

	f := newFetches(t, c)
	f.start("a")

	fc := s.conn()
	fc.expect("AUTHINFO USER user")
	fc.write("481 rejected\r\n")

	f.wait()
	if err := f.assertErr("a"); !errors.Is(err, ErrAuthFailed) {
		t.Fatalf("got %v, want ErrAuthFailed", err)
	}
}

// An idle pipelined connection is let go of like an idle synchronous one: the
// account's connection limit counts it either way.
func TestPipelineReapsIdleConnection(t *testing.T) {
	s := newFakeNNTP(t)
	c := pipelineClient(t, s, Config{
		MaxConns: 2, ConnectionPipeliningSize: 2, IdleTimeout: 50 * time.Millisecond,
	})

	f := newFetches(t, c)
	f.start("a")
	fc := s.conn()
	fc.expect("GROUP " + testGroup)
	fc.article()
	fc.groupOK(testGroup)
	fc.respond([]byte("body"))
	f.wait()
	f.assertBody("a", "body")

	// the command reader ends when the client closes its side
	deadline := time.After(5 * time.Second)
	for {
		if c.pipesOpen() == 0 && len(c.slots) == 2 {
			return
		}
		select {
		case <-deadline:
			t.Fatalf("idle connection not reaped: %d open, %d slots", c.pipesOpen(), len(c.slots))
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// A pipelined connection holds a slot for its whole life, so it must not also be
// counted as one of the pipes: the reported number never exceeds the limit.
func TestOpenConnsCountsPipesOnce(t *testing.T) {
	s := newFakeNNTP(t)
	c := pipelineClient(t, s, Config{MaxConns: 2, ConnectionPipeliningSize: 2})

	f := newFetches(t, c)
	f.start("a")
	fc := s.conn()
	fc.expect("GROUP " + testGroup)

	if open := c.OpenConns(); open != 1 {
		t.Fatalf("got %d open, want 1", open)
	}

	fc.article()
	fc.groupOK(testGroup)
	fc.respond([]byte("body"))
	f.wait()
	f.assertBody("a", "body")

	if open := c.OpenConns(); open > c.Conns() {
		t.Fatalf("got %d open, want at most %d", open, c.Conns())
	}
}

// Pipelining needs a window worth having, so anything less takes the plain
// path. The connection count does not come into it: one connection pipelines
// like any other.
func TestPipeliningOffWhenItCannotHelp(t *testing.T) {
	for _, cfg := range []Config{
		{MaxConns: 4, ConnectionPipeliningSize: 1},
		{MaxConns: 4, ConnectionPipeliningSize: 0},
	} {
		if New(cfg).canPipe {
			t.Errorf("%+v pipelines, want the plain path", cfg)
		}
	}
	for _, cfg := range []Config{
		{MaxConns: 2, ConnectionPipeliningSize: 2},
		{MaxConns: 1, ConnectionPipeliningSize: 8},
	} {
		if !New(cfg).canPipe {
			t.Errorf("%+v does not pipeline", cfg)
		}
	}
}

// A pipelined connection holds its slot for as long as it lives, so with every
// slot pipelined a synchronous command has nothing to acquire. One of the idle
// pipes gives way rather than the command waiting out the reaper.
func TestSyncCommandTakesTheSlotBackFromAnIdlePipe(t *testing.T) {
	s := newFakeNNTP(t)
	c := pipelineClient(t, s, Config{MaxConns: 1, ConnectionPipeliningSize: 2})

	f := newFetches(t, c)
	f.start("a")
	pipe := s.conn()
	pipe.expect("GROUP " + testGroup)
	pipe.article()
	pipe.groupOK(testGroup)
	pipe.respond([]byte("body"))
	f.wait()
	f.assertBody("a", "body")

	// the pipe carries nothing now and still holds the account's only slot
	done := make(chan error, 1)
	go func() { done <- c.Probe() }()

	probe := s.conn()
	probe.silent("a probe that took the slot back")

	if err := <-done; err != nil {
		t.Fatalf("probe: %v", err)
	}
}

// STAT rides a pipelined connection like an article does, so a health check
// costs a status line on a connection that already exists rather than a
// handshake of its own.
func TestPipelinesStat(t *testing.T) {
	s := newFakeNNTP(t)
	c := pipelineClient(t, s, Config{MaxConns: 1, ConnectionPipeliningSize: 4})

	f := newFetches(t, c)
	f.start("a")
	pipe := s.conn()
	pipe.expect("GROUP " + testGroup)
	pipe.article()

	exists := make(chan bool, 1)
	go func() {
		got, err := c.SegmentExists("here")
		if err != nil {
			t.Errorf("stat: %v", err)
		}
		exists <- got
	}()

	// no group, and out on the same connection while the article is unanswered
	pipe.expect("STAT <here>")
	s.noConn("a second connection for the stat")

	pipe.groupOK(testGroup)
	pipe.respond([]byte("body"))
	pipe.write("223 0 <here> article exists\r\n")

	f.wait()
	f.assertBody("a", "body")
	if !<-exists {
		t.Fatal("stat reported the segment missing")
	}
}

func TestPipelinedStatReportsMissing(t *testing.T) {
	s := newFakeNNTP(t)
	c := pipelineClient(t, s, Config{MaxConns: 1, ConnectionPipeliningSize: 2})

	got := make(chan bool, 1)
	go func() {
		exists, err := c.SegmentExists("gone")
		if err != nil {
			t.Errorf("stat: %v", err)
		}
		got <- exists
	}()

	pipe := s.conn()
	pipe.expect("STAT <gone>")
	pipe.write("430 no such article\r\n")

	if <-got {
		t.Fatal("stat reported a missing segment as present")
	}
}
