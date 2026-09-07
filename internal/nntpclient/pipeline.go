package nntpclient

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"git.ruekov.eu/ruakij/nzbStreamer/pkg/yenc"
)

// A pipelined connection writes several commands before reading any of their
// responses, so the transfer of one segment overlaps the round trip of the
// next. Responses come back strictly in the order the commands were sent
// (RFC 3977 3.5), which is what makes the queue of unread responses a plain
// FIFO and the whole thing possible at all.
//
// It pipelines in both directions of that: GROUP and ARTICLE go out as one
// write, and up to ConnectionPipeliningSize articles are outstanding at once.
//
// One writer goroutine and one reader goroutine share a connection. The writer
// takes what dispatch put in the connection's inbox and hands each to the
// reader through inflight. Two goroutines rather than one is what lets a
// command go out while a response is still being read, which is the point of
// the exercise.

// freedPoll is how long a fetch waiting for a connection to free up sleeps
// before looking again. Completions signal it awake, so this only ever runs out
// when a signal went to another waiter.
const freedPoll = 50 * time.Millisecond

// fetch is one segment waiting on a pipelined connection.
type fetch struct {
	group, id string
	// needsGroup records that a GROUP was written ahead of the ARTICLE, so the
	// reader knows to read its response first. The writer sets it and the
	// reader reads it, handed over through inflight.
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

	// group is the group the connection has selected. Only the writer touches
	// it; regroup is how the reader tells it that a GROUP it recorded was
	// rejected, so nothing is selected after all.
	group   string
	regroup atomic.Bool

	// in holds what dispatch has given this connection and the writer has not
	// sent yet, inflight what it has sent and the reader has not answered yet.
	// Both are bounded by load, so neither ever blocks whoever fills it.
	in       chan *fetch
	inflight chan *fetch
	// quit stops the writer, which then hands the reader whatever is left and
	// lets it finish. deadErr is why, written once inside quitOnce.
	quit     chan struct{}
	quitOnce sync.Once
	deadErr  error

	// served reports whether a response has ever come back on this connection.
	// A failure on one that has is a reuse the server may simply have hung up
	// on, which is the same forgiveness the synchronous path gets.
	served atomic.Bool

	// load is how many fetches this connection has been given and not answered,
	// lastUsed when it last answered one. Both belong to the client rather than
	// to the connection, because choosing between connections has to see all of
	// them at once: they are guarded by c.mu.
	load     int
	lastUsed time.Time
}

// pipelineFetch runs one segment through a pipelined connection. Retrying and
// the classification of what comes back is the caller's retry wrapper.
func (c *Client) pipelineFetch(group, id string) ([]byte, error) {
	f := &fetch{group: group, id: id, result: make(chan fetchResult, 1)}
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
		return nil, fmt.Errorf("timed out getting segment '%s'", id)
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
// Load is the whole cost model, because it already carries what a timing would
// measure: the connections share one link, so what a segment costs is common to
// all of them and cancels out of the comparison, and what does not - a large
// segment, a connection a provider is shaping - shows up as that connection
// holding its fetches longer and so keeping a load the next choice avoids. A
// measured per-connection service time would say the same thing a step later.
//
// A dial runs in the background rather than in front of the fetch that asked
// for it, so the fetch takes whichever connection has room first - the new one,
// or an existing one that answered in the meantime.
func (c *Client) dispatch(f *fetch) error {
	deadline := time.Now().Add(c.config.Timeout)

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
// the only state a waiting fetch cannot be served out of. It is taken rather
// than read, so one failure is not reported to every later fetch too.
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
// to an unloaded one when idleOnly, and reports whether one took it.
//
// The choice and the hand-off are one critical section: a connection can only
// be given a fetch while it is still in c.pipes, and a connection that stops
// takes itself out of c.pipes under the same lock, so nothing is ever handed to
// a connection that has already gone.
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
	// the inbox holds a window's worth and the load is below it, so this cannot
	// block, which is what makes doing it under the lock safe
	best.in <- f
	return true
}

// pipeCap is how many pipelined connections may exist at once: all but one of
// the account's connections. The one held back stays free for the commands that
// are still synchronous (STAT, Probe), so a health check can never queue behind
// a full set of busy ones.
func (c *Client) pipeCap() int {
	if n := c.config.MaxConns - 1; n >= 1 {
		return n
	}
	return 1
}

func (c *Client) pipesOpen() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.pipeCount
}

// freed wakes a fetch waiting for a connection to have room. Missing a signal
// costs a poll rather than a stall, so it is one slot deep and never waited on.
func (c *Client) signalFreed() {
	select {
	case c.freed <- struct{}{}:
	default:
	}
}

// grow dials until an unloaded connection is there for every waiting fetch plus
// MinFreeConns more, or the account has no room left, and reports whether a
// dial is in flight afterwards. Counting free connections rather than free
// window slots is what keeps a fetch spreading before it deepens; the headroom
// is what a measured arrival rate would say a step later, since capacity only
// looks short once demand has taken it and a handshake costs a round trip a
// warm connection does not. The reaper closes what demand stops justifying.
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

// startPipe opens one more pipelined connection when the account has room for
// it, and reports whether it started. The count and the slot are reserved
// before the dial, so concurrent dispatches cannot overshoot the cap; the
// handshake runs in the background, so nobody waits on a connection they may
// not end up using.
func (c *Client) startPipe() bool {
	c.mu.Lock()
	if c.pipeCount >= c.pipeCap() {
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

// dialPipe opens a connection and takes it through greeting and authentication
// itself, because the pipelined path owns its reader from the first byte on:
// astuart.co/nntp buffers into a reader it does not hand out, and a second
// reader over the same socket would race it for the bytes.
func (c *Client) dialPipe() (*pipeConn, error) {
	netConn, err := c.dialNet()
	if err != nil {
		return nil, err
	}

	window := c.config.ConnectionPipeliningSize
	p := &pipeConn{
		c:        c,
		net:      netConn,
		br:       bufio.NewReader(netConn),
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

	return p, nil
}

func (p *pipeConn) handshake() error {
	code, msg, err := p.status()
	if err != nil {
		return fmt.Errorf("failed reading greeting: %w", err)
	}
	if err := greetingCode(code, msg); err != nil {
		return fmt.Errorf("%s refused the connection: %w", p.net.RemoteAddr(), err)
	}

	if p.c.config.User == "" {
		return nil
	}

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

// command writes one command and reads its status line, which is only for the
// handshake: everything after it is pipelined and the two halves are split.
func (p *pipeConn) command(format string, args ...any) (int, string, error) {
	if _, err := fmt.Fprintf(p.net, format+"\r\n", args...); err != nil {
		return 0, "", err
	}
	return p.status()
}

// write sends the commands for what dispatch put in the inbox, handing each to
// the reader. It never waits for a response, so the next command goes out while
// the previous one is still arriving.
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

// send writes a fetch's command, prefixing GROUP in the same write when the
// connection is not already on the fetch's group. The two go out together so
// the group selection costs no round trip of its own.
func (p *pipeConn) send(f *fetch) error {
	p.writeDeadline(p.c.config.Timeout)

	cmd := fmt.Sprintf("ARTICLE <%s>\r\n", f.id)
	if f.group != "" && (f.group != p.group || p.regroup.Swap(false)) {
		f.needsGroup = true
		cmd = fmt.Sprintf("GROUP %s\r\n", f.group) + cmd
	}

	if _, err := io.WriteString(p.net, cmd); err != nil {
		return fmt.Errorf("failed requesting article '%s': %w", f.id, err)
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

// reply reads one fetch's responses - its GROUP response first, where one was
// sent - and yenc-decodes the body through its terminator, leaving the
// connection at the next response.
//
// The article response follows whatever the group answered, because both
// commands were written before either was read, so it is read out either way
// and a rejected group is reported instead of it rather than in place of
// reading it.
//
// fatal reports whether the connection is unusable afterwards. A rejected group
// and a missing article both leave the stream intact and are the answer to that
// one fetch; only an I/O or decode failure loses the position.
func (p *pipeConn) reply(f *fetch) (body []byte, err error, fatal bool) {
	p.readDeadline(p.c.config.Timeout)

	var groupErr error
	if f.needsGroup {
		code, msg, err := p.status()
		if err != nil {
			return nil, fmt.Errorf("failed reading group '%s' response: %w", f.group, err), true
		}
		if code != groupJoined {
			groupErr = fmt.Errorf("%w to group '%s': %d %s", ErrUnexpectedResponse, f.group, code, msg)
			// no group is selected after a rejected one, whatever the writer
			// recorded when it sent it
			p.regroup.Store(true)
		}
	}

	body, err, fatal = p.article(f)
	if groupErr != nil && !fatal {
		return nil, groupErr, false
	}
	return body, err, fatal
}

func (p *pipeConn) article(f *fetch) (body []byte, err error, fatal bool) {
	code, msg, err := p.status()
	if err != nil {
		return nil, fmt.Errorf("failed reading article '%s' response: %w", f.id, err), true
	}

	switch code {
	case articleFollows, articleFollowsBody:
		// The decode reads the dot-stuffed lines straight off this connection's
		// buffer as they arrive, so it costs the transfer rather than following
		// it.
		decoded, err := yenc.Decode(p.br)
		if err != nil {
			// A body cut short only shows up as a decode failure, and fetch and
			// decode being one operation is what makes it worth another attempt.
			return nil, fmt.Errorf("failed yenc-decoding article '%s': %w", f.id, err), true
		}
		return decoded, nil, false

	case noArticleWithID:
		return nil, fmt.Errorf("%w: '%s'", ErrArticleNotFound, f.id), false

	default:
		return nil, fmt.Errorf("%w to article '%s': %d %s", ErrUnexpectedResponse, f.id, code, msg), false
	}
}

// drain fails everything the writer had already sent. The writer notices the
// stop and closes inflight, which is what ends this.
func (p *pipeConn) drain() {
	err := p.failure(p.deadErr)
	for f := range p.inflight {
		p.answer(f, fetchResult{err: err})
	}
}

// answer hands a fetch its result and gives back the load it was holding, which
// is what lets a fetch waiting for a connection have this one.
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

// stop takes the connection out of service: nothing more is given to it, the
// writer stops accepting fetches and the reader finishes what is outstanding.
// Retiring it here rather than once it has finished is what keeps the retry of
// a fetch it just failed from being handed straight back to it. A failure
// closes the socket straight away, since it is broken anyway and closing wakes
// whoever is on it; a graceful stop (err nil, the reaper) leaves that to the
// reader.
func (p *pipeConn) stop(err error) {
	p.quitOnce.Do(func() {
		p.retire()
		p.deadErr = err
		close(p.quit)
		if err != nil {
			p.net.Close()
		}
	})
}

// retire takes the connection out of the choice, so nothing more is given to
// it. Everything it already holds is still answered.
func (p *pipeConn) retire() {
	c := p.c
	c.mu.Lock()
	delete(c.pipes, p)
	c.mu.Unlock()
}

// failure marks an error as a stale reuse where the connection had already
// answered something, which is a server hanging up on an idle connection rather
// than a fault of the request that discovered it.
func (p *pipeConn) failure(err error) error {
	if err == nil {
		err = fmt.Errorf("connection closed")
	}
	return staleIf(p.served.Load(), err)
}

// close is the last thing to happen on a connection: the reader calls it once
// nothing is outstanding any more.
func (p *pipeConn) close() {
	p.net.Close()
	p.retire()

	c := p.c
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

// writeDeadline and readDeadline are separate because the writer and the reader
// are separate goroutines; one SetDeadline would clobber the other's.
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

// reapPipes stops pipelined connections that have sat idle past IdleTimeout, so
// this path also lets go of the connections the account limit counts, and
// reports how long until the next one comes due.
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
