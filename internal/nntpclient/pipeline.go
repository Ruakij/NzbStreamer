package nntpclient

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"git.ruekov.eu/ruakij/nzbStreamer/pkg/yenc"
)

// A pipelined connection writes several commands before reading any of their
// responses, so one segment transfers while the next is being asked for.
// Responses come back in the order they were asked for (RFC 3977 3.5), which
// makes the unread ones a plain FIFO. GROUP and ARTICLE go out as one write.
//
// A writer goroutine and a reader goroutine share the connection, which is what
// lets a command go out while a response is still arriving.

// freedPoll is the fallback wakeup for a waiting fetch, for when the signal a
// completion sends went to another waiter.
const freedPoll = 50 * time.Millisecond

// readBufferSize keeps a segment to a dozen socket reads. bufio's 4 KiB default
// costs a syscall per 30 or so yenc lines.
const readBufferSize = 64 << 10

// fetch is one command waiting on a pipelined connection. cmd is what is asked
// of the server, which is also what the response is recorded as; only an
// ARTICLE is answered by a body, so the rest cost a status line and nothing
// else and are worth pipelining for exactly that reason.
type fetch struct {
	cmd       string
	group, id string
	// needsGroup tells the reader a GROUP response comes first; the writer sets
	// it and hands it over through inflight
	needsGroup bool
	result     chan fetchResult
}

type fetchResult struct {
	body []byte
	err  error
}

// pipeConn is one connection carrying a pipelined command stream.
type pipeConn struct {
	c   *Client
	net net.Conn
	br  *bufio.Reader

	// group is what the connection has selected, touched only by the writer.
	// regroup is the reader saying a recorded GROUP was rejected.
	group   string
	regroup atomic.Bool

	// in is given but not sent, inflight sent but not answered. Both are
	// bounded by load, so neither blocks whoever fills it.
	in       chan *fetch
	inflight chan *fetch
	// quit stops the writer, which lets the reader finish what is left.
	// deadErr is why, written once inside quitOnce.
	quit     chan struct{}
	quitOnce sync.Once
	deadErr  error

	// served is whether anything ever came back here. A failure on one that has
	// is a stale reuse, forgiven as on the synchronous path.
	served atomic.Bool

	// load is given and not answered, lastUsed when it last answered. Choosing
	// between connections sees all of them at once, so both are under c.mu.
	load     int
	lastUsed time.Time

	// socket is this connection in the set the collection reads
	socket *socket
}

// pipelineFetch runs one segment through a pipelined connection. Retrying is
// the caller's.
func (c *Client) pipelineFetch(group, id string) ([]byte, error) {
	return c.pipelineCmd(responseArticle, group, id)
}

// pipelineStat asks whether a segment is there over a pipelined connection. It
// needs no group: a STAT by message id is answered from anywhere.
func (c *Client) pipelineStat(id string) (bool, error) {
	_, err := c.pipelineCmd(responseStat, "", id)
	if errors.Is(err, ErrArticleNotFound) {
		return false, nil
	}
	return err == nil, err
}

// pipelineCmd runs one command through a pipelined connection. Retrying is the
// caller's.
func (c *Client) pipelineCmd(cmd, group, id string) ([]byte, error) {
	f := &fetch{cmd: cmd, group: group, id: id, result: make(chan fetchResult, 1)}
	if err := c.dispatch(f); err != nil {
		return nil, err
	}

	if c.config.Timeout <= 0 {
		res := <-f.result
		return res.body, res.err
	}
	select {
	case res := <-f.result:
		return res.body, res.err
	case <-time.After(c.config.Timeout):
		return nil, fmt.Errorf("timed out on %s '%s'", cmd, id)
	}
}

// dispatch gives a fetch to a connection, in the order of what it costs to be
// served by one:
//
//  1. a connection with nothing on it, which starts transferring at once and
//     costs no handshake,
//  2. a new connection, when the account has room for one. A provider shapes a
//     single connection, so parallel ones are how the bandwidth is there at
//     all, and a handshake is cheaper than queueing behind a segment,
//  3. the least loaded of what exists, once none of that is left.
//
// Load is the whole cost model: the connections share one link, so what a
// segment costs is common to all of them and cancels, and what is not - a big
// segment, a shaped connection - holds its fetches longer and so keeps a load
// the next choice avoids.
//
// A dial runs in the background, so the fetch takes whichever connection has
// room first: the new one, or an existing one that answered meanwhile.
func (c *Client) dispatch(f *fetch) error {
	started := time.Now()
	deadline := started.Add(c.config.Timeout)
	defer c.recordWait(started)

	c.wait(1)
	// top the headroom back up once this fetch is placed
	defer c.grow()
	defer c.wait(-1)

	for {
		if c.offer(f, true) {
			return nil
		}

		if err := c.stalled(); err != nil {
			return err
		}

		// a connection on its way beats piling onto a busy one
		if !c.grow() && c.offer(f, false) {
			return nil
		}

		// Every connection is at its window: wait for one to answer something,
		// or for a dial to finish.
		select {
		case <-c.freed:
		case <-time.After(freedPoll):
		}
		if c.config.Timeout > 0 && time.Now().After(deadline) {
			return fmt.Errorf("timed out queueing segment '%s'", f.id)
		}
	}
}

func (c *Client) wait(delta int) {
	c.mu.Lock()
	c.waiting += delta
	c.mu.Unlock()
}

// stalled takes the last dial failure while nothing is open or being dialled,
// the only state a waiting fetch cannot be served out of. Taken rather than
// read, so one failure fails one fetch.
func (c *Client) stalled() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.pipeCount > 0 {
		return nil
	}
	err := c.dialErr
	c.dialErr = nil
	return err
}

// offer hands a fetch to the least loaded connection with room for it, or only
// to an unloaded one when idleOnly. Choosing and handing over are one critical
// section, so nothing is given to a connection that has already gone.
func (c *Client) offer(f *fetch, idleOnly bool) bool {
	c.mu.Lock()
	defer c.mu.Unlock()

	var best *pipeConn
	for p := range c.pipes {
		if p.load >= c.config.ConnectionPipeliningSize {
			continue
		}
		if best == nil || p.load < best.load {
			best = p
		}
	}
	if best == nil || (idleOnly && best.load > 0) {
		return false
	}

	best.load++
	// a window's worth of inbox and a load below it, so this cannot block
	best.in <- f
	return true
}

// yieldPipe stops one pipelined connection that has nothing outstanding, so
// that its slot comes back for a command that runs synchronously. A pipe holds
// its slot for its whole life, so with every slot pipelined there is otherwise
// nothing for a Probe to acquire until the reaper comes round. It reports
// whether one was found; the slot arrives once the connection has wound down.
func (c *Client) yieldPipe() bool {
	c.mu.Lock()
	var spare *pipeConn
	for p := range c.pipes {
		if p.load == 0 {
			spare = p
			break
		}
	}
	c.mu.Unlock()

	if spare == nil {
		return false
	}
	spare.stop(nil)
	return true
}

func (c *Client) pipesOpen() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.pipeCount
}

// signalFreed wakes a fetch waiting for room. A missed signal costs a poll
// rather than a stall, so it is one slot deep and never waited on.
func (c *Client) signalFreed() {
	select {
	case c.freed <- struct{}{}:
	default:
	}
}

// grow dials until an unloaded connection is there for every waiting fetch plus
// MinFreeConns more, and reports whether a dial is in flight. Counting free
// connections rather than free window slots is what keeps a fetch spreading
// before it deepens; the reaper closes what demand stops justifying.
func (c *Client) grow() bool {
	for c.needsPipe() && c.startPipe() {
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.dialing > 0
}

func (c *Client) needsPipe() bool {
	c.mu.Lock()
	defer c.mu.Unlock()

	free := c.dialing
	for p := range c.pipes {
		if p.load == 0 {
			free++
		}
	}
	return free < c.waiting+c.config.MinFreeConns
}

// startPipe opens one more connection when the account has room. The count and
// the slot are reserved before the dial, so concurrent dispatches cannot
// overshoot the cap, and the handshake runs in the background.
func (c *Client) startPipe() bool {
	c.mu.Lock()
	if c.pipeCount >= c.config.MaxConns {
		c.mu.Unlock()
		return false
	}
	select {
	case <-c.slots:
	default:
		// every connection the account may have is in use synchronously
		c.mu.Unlock()
		return false
	}
	c.pipeCount++
	c.dialing++
	c.mu.Unlock()

	go func() {
		p, err := c.dialPipe()

		c.mu.Lock()
		c.dialing--
		c.dialErr = err
		if err != nil {
			c.pipeCount--
			c.mu.Unlock()
			c.slots <- struct{}{}
			c.signalFreed()
			return
		}
		c.pipes[p] = struct{}{}
		c.mu.Unlock()

		go p.write()
		go p.read()
		c.signalFreed()
	}()
	return true
}

// dialPipe does its own greeting and authentication, because astuart.co/nntp
// buffers into a reader it does not hand out and a second reader over the same
// socket would race it.
func (c *Client) dialPipe() (*pipeConn, error) {
	netConn, err := c.dialNet()
	if err != nil {
		return nil, err
	}

	window := c.config.ConnectionPipeliningSize
	p := &pipeConn{
		c:        c,
		net:      netConn,
		br:       bufio.NewReaderSize(netConn, readBufferSize),
		in:       make(chan *fetch, window),
		inflight: make(chan *fetch, window),
		quit:     make(chan struct{}),
		lastUsed: time.Now(),
	}

	p.deadline(c.config.Timeout)
	if err := p.handshake(); err != nil {
		netConn.Close()
		return nil, err
	}
	p.deadline(0)

	p.socket = c.track(netConn)
	return p, nil
}

func (p *pipeConn) handshake() error {
	started := time.Now()
	code, msg, err := p.status()
	p.c.recordResponse(responseGreeting, started)
	if err != nil {
		return fmt.Errorf("failed reading greeting: %w", err)
	}
	if err := greetingCode(code, msg); err != nil {
		return fmt.Errorf("%s refused the connection: %w", p.net.RemoteAddr(), err)
	}

	if p.c.config.User == "" {
		return nil
	}

	started = time.Now()
	defer func() { p.c.recordResponse(responseAuth, started) }()

	code, msg, err = p.command("AUTHINFO USER %s", p.c.config.User)
	if err != nil {
		return fmt.Errorf("failed sending username: %w", err)
	}
	if code == passwordNeeded {
		code, msg, err = p.command("AUTHINFO PASS %s", p.c.config.Pass)
		if err != nil {
			return fmt.Errorf("failed sending password: %w", err)
		}
	}
	return authCode(code, msg)
}

// command writes one command and reads its status line, for the handshake only:
// everything after it is pipelined and the two halves are split.
func (p *pipeConn) command(format string, args ...any) (int, string, error) {
	if _, err := fmt.Fprintf(p.net, format+"\r\n", args...); err != nil {
		return 0, "", err
	}
	return p.status()
}

// write sends what dispatch put in the inbox and hands each to the reader,
// never waiting for a response.
func (p *pipeConn) write() {
	for {
		select {
		case f := <-p.in:
			if err := p.send(f); err != nil {
				p.stop(err)
				p.answer(f, fetchResult{err: p.failure(err)})
				p.shutdown()
				return
			}
			// bounded by the load the fetch was accepted under, so it fits
			p.inflight <- f

		case <-p.quit:
			p.shutdown()
			return
		}
	}
}

// shutdown takes the connection out of the choice and fails whatever it was
// given but never sent, then lets the reader finish what is on the wire.
func (p *pipeConn) shutdown() {
	p.retire()

	err := p.failure(p.deadErr)
	for {
		select {
		case f := <-p.in:
			p.answer(f, fetchResult{err: err})
		default:
			close(p.inflight)
			return
		}
	}
}

// send writes the command, prefixed by a GROUP in the same write where the
// connection is not on the fetch's group already, so selecting one costs no
// round trip.
func (p *pipeConn) send(f *fetch) error {
	p.writeDeadline(p.c.config.Timeout)

	cmd := fmt.Sprintf("%s <%s>\r\n", f.cmd, f.id)
	if f.group != "" && (f.group != p.group || p.regroup.Swap(false)) {
		f.needsGroup = true
		cmd = fmt.Sprintf("GROUP %s\r\n", f.group) + cmd
	}

	if _, err := io.WriteString(p.net, cmd); err != nil {
		return fmt.Errorf("failed sending %s '%s': %w", f.cmd, f.id, err)
	}
	if f.needsGroup {
		p.group = f.group
	}
	return nil
}

// read answers the fetches the writer has sent, in the order it sent them.
func (p *pipeConn) read() {
	defer p.close()

	for f := range p.inflight {
		body, err, fatal := p.reply(f)
		if fatal {
			// The position in the response stream is lost, so everything else
			// outstanding on this connection dies with it.
			p.stop(err)
			p.answer(f, fetchResult{err: p.failure(err)})
			p.drain()
			return
		}

		p.served.Store(true)
		p.answer(f, fetchResult{body: body, err: err})
	}
}

// reply reads one fetch's responses, its GROUP first where one was sent, and
// leaves the connection at the next response. The article response is read
// whatever the group answered: both went out before either was read, so
// skipping one would leave the stream a response out of step.
//
// fatal is whether the connection is unusable afterwards. A rejected group and
// a missing article leave the stream intact; only I/O or a decode loses it.
func (p *pipeConn) reply(f *fetch) (body []byte, err error, fatal bool) {
	p.readDeadline(p.c.config.Timeout)

	var groupErr error
	if f.needsGroup {
		started := time.Now()
		code, msg, err := p.status()
		p.c.recordResponse(responseGroup, started)
		if err != nil {
			return nil, fmt.Errorf("failed reading group '%s' response: %w", f.group, err), true
		}
		if code != groupJoined {
			groupErr = fmt.Errorf("%w to group '%s': %d %s", ErrUnexpectedResponse, f.group, code, msg)
			// nothing is selected after a rejected group, whatever the writer recorded
			p.regroup.Store(true)
		}
	}

	body, err, fatal = p.respond(f)
	if groupErr != nil && !fatal {
		return nil, groupErr, false
	}
	return body, err, fatal
}

// respond reads the command's own response. Only an ARTICLE is followed by a
// body; the rest are the status line and nothing more, which is what makes them
// worth pipelining at all.
func (p *pipeConn) respond(f *fetch) (body []byte, err error, fatal bool) {
	// The command went out before this connection's earlier responses were
	// read, so this is the wait on the wire once the reader reached it rather
	// than the time the whole request took.
	started := time.Now()
	code, msg, err := p.status()
	p.c.recordResponse(f.cmd, started)
	if err != nil {
		return nil, fmt.Errorf("failed reading %s '%s' response: %w", f.cmd, f.id, err), true
	}

	switch {
	case f.cmd == responseArticle && (code == articleFollows || code == articleFollowsBody):
		// Decoding off this connection's buffer as the lines arrive costs the
		// transfer rather than following it
		decoded, err := yenc.Decode(p.br)
		if err != nil {
			// A body cut short only surfaces as a decode failure, which is worth
			// another attempt
			return nil, fmt.Errorf("failed yenc-decoding article '%s': %w", f.id, err), true
		}
		return decoded, nil, false

	case f.cmd == responseStat && code == articleExists:
		return nil, nil, false

	case code == noArticleWithID:
		return nil, fmt.Errorf("%w: '%s'", ErrArticleNotFound, f.id), false

	default:
		// A body this command was not expecting is left in the stream, and
		// everything read after it is a response out of step
		unread := code == articleFollows || code == articleFollowsBody
		return nil, fmt.Errorf("%w to %s '%s': %d %s", ErrUnexpectedResponse, f.cmd, f.id, code, msg), unread
	}
}

// drain fails everything already sent. The writer closes inflight, which ends it.
func (p *pipeConn) drain() {
	err := p.failure(p.deadErr)
	for f := range p.inflight {
		p.answer(f, fetchResult{err: err})
	}
}

// answer hands a fetch its result and gives back the load it held, which is
// what lets a waiting fetch have this connection.
func (p *pipeConn) answer(f *fetch, res fetchResult) {
	c := p.c
	c.mu.Lock()
	p.load--
	p.lastUsed = time.Now()
	c.mu.Unlock()

	f.result <- res
	c.signalFreed()
}

// status reads one "NNN <rest>" status line off the wire.
func (p *pipeConn) status() (int, string, error) {
	line, err := p.br.ReadString('\n')
	if err != nil {
		return 0, "", err
	}
	return parseStatus(line)
}

// stop takes the connection out of service, leaving the reader to finish what
// is outstanding. Retiring it here rather than once it has finished is what
// keeps the retry of a fetch it just failed from coming back to it. A failure
// closes the socket at once, to wake whoever is on it; the reaper leaves that
// to the reader.
func (p *pipeConn) stop(err error) {
	p.quitOnce.Do(func() {
		p.retire()
		p.deadErr = err
		close(p.quit)
		if err != nil {
			p.c.recordSocket(recordCtx, p.socket)
			p.c.untrack(p.socket)
			p.net.Close()
		}
	})
}

// retire takes the connection out of the choice. What it already holds is
// still answered.
func (p *pipeConn) retire() {
	c := p.c
	c.mu.Lock()
	delete(c.pipes, p)
	c.mu.Unlock()
}

// failure marks an error stale where the connection had answered before: a
// server hanging up on an idle connection, not a fault of this request.
func (p *pipeConn) failure(err error) error {
	if err == nil {
		err = fmt.Errorf("connection closed")
	}
	return staleIf(p.served.Load(), err)
}

// close is the last thing on a connection, called by the reader once nothing
// is outstanding.
func (p *pipeConn) close() {
	c := p.c
	if p.deadErr != nil {
		c.closeConn(p.socket, closeError)
	} else {
		c.closeConn(p.socket, closeIdle)
	}
	p.retire()

	c.mu.Lock()
	c.pipeCount--
	c.mu.Unlock()

	c.slots <- struct{}{}
	c.signalFreed()
}

func (p *pipeConn) deadline(timeout time.Duration) {
	if timeout <= 0 {
		_ = p.net.SetDeadline(time.Time{})
		return
	}
	_ = p.net.SetDeadline(time.Now().Add(timeout))
}

// Separate deadlines because the writer and the reader are separate goroutines;
// one SetDeadline would clobber the other's.
func (p *pipeConn) writeDeadline(timeout time.Duration) {
	if timeout <= 0 {
		return
	}
	_ = p.net.SetWriteDeadline(time.Now().Add(timeout))
}

func (p *pipeConn) readDeadline(timeout time.Duration) {
	if timeout <= 0 {
		return
	}
	_ = p.net.SetReadDeadline(time.Now().Add(timeout))
}

// reapPipes stops connections idle past IdleTimeout, which the account limit
// counts either way, and reports when the next one comes due.
func (c *Client) reapPipes() time.Duration {
	wait := c.config.IdleTimeout

	var reap []*pipeConn
	c.mu.Lock()
	for p := range c.pipes {
		if p.load > 0 {
			continue
		}
		idle := time.Since(p.lastUsed)
		if idle >= c.config.IdleTimeout {
			reap = append(reap, p)
			continue
		}
		if due := c.config.IdleTimeout - idle; due < wait {
			wait = due
		}
	}
	c.mu.Unlock()

	for _, p := range reap {
		p.stop(nil)
	}
	return wait
}
