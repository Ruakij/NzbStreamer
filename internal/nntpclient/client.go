// Package nntpclient talks to a news server: it owns the connection pool and
// turns a segments message-id into its decoded content.
package nntpclient

import (
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"astuart.co/nntp"
	"github.com/chrisfarms/yenc"
)

// Response codes, from RFC 3977 and the AUTHINFO from RFC 4643.
const (
	serverReady       = 200
	serverReadyNoPost = 201
	groupJoined       = 211
	articleExists     = 223
	authAccepted      = 281
	passwordNeeded    = 381
	authRejected      = 481
	noArticleWithID   = 430
	connsExceeded     = 502
)

var (
	// ErrAuthFailed is the server rejecting the credentials, which no retry and
	// no waiting is going to change
	ErrAuthFailed = errors.New("auth failed")
	// ErrTooManyConnections is the account already using all the connections it
	// is allowed. Nothing here can help but using fewer, so it is a transient
	// state of this process rather than a fault of the server
	ErrTooManyConnections = errors.New("too many connections for this account")
	ErrArticleNotFound    = errors.New("article not found on server")
	ErrUnexpectedResponse = errors.New("unexpected response")
)

// errStaleConn marks a command that failed on a connection taken from the idle
// pool. News servers close idle connections without saying so, and tcp only
// reports it when the connection is next written to, so this is the expected
// outcome of a pause in reading rather than a failure of the request.
var errStaleConn = errors.New("reused connection failed")

// staleIf marks a command that failed on a connection taken from the idle pool.
func staleIf(reused bool, err error) error {
	if !reused {
		return err
	}
	return fmt.Errorf("%w: %w", errStaleConn, err)
}

type Config struct {
	Host string
	Port int
	TLS  bool
	User string
	Pass string
	// MaxConns bounds how many connections exist at once
	MaxConns int
	// Attempts a request gets before its error is reported
	Attempts int
	// Backoff waited after the first failed attempt, doubled after each further one
	Backoff time.Duration
	// Timeout for connecting and for completing a single request
	Timeout time.Duration
	// IdleTimeout after which an unused connection is closed; defaults when unset
	IdleTimeout time.Duration
}

// Client is a pool of news server connections.
//
// It keeps its own pool rather than using the one in astuart.co/nntp, which
// counts connections it hands out but never counts them back in, so a
// connection lost to a failed command is a slot lost for the rest of the
// process. Retrying makes that a matter of minutes.
type Client struct {
	config Config
	dial   func() (*conn, error)
	// idle holds connections that are ready for a command
	idle chan *conn
	// slots holds one token per connection the client is allowed to have open
	slots chan struct{}
	// waiting counts the requests blocked on a slot
	waiting atomic.Int64
}

// Waiting reports how many requests are queued for a free connection. It is a
// reading, not a reservation: it answers whether to queue more work, not what
// this request will get.
func (c *Client) Waiting() int {
	return int(c.waiting.Load())
}

// Conns reports how many connections this client may have open at once.
func (c *Client) Conns() int {
	return c.config.MaxConns
}

func New(config Config) *Client {
	if config.MaxConns < 1 {
		config.MaxConns = 1
	}
	if config.Attempts < 1 {
		config.Attempts = 1
	}
	// Holding connections indefinitely is not on offer: a news server hangs up
	// on an idle one within a few minutes whatever the client would prefer, so
	// an unbounded timeout only decides how long the closed sockets are kept.
	if config.IdleTimeout <= 0 {
		config.IdleTimeout = 2 * time.Minute
	}

	client := &Client{
		config: config,
		idle:   make(chan *conn, config.MaxConns),
		slots:  make(chan struct{}, config.MaxConns),
	}
	client.dial = client.dialServer
	for range config.MaxConns {
		client.slots <- struct{}{}
	}

	go client.reapIdle()
	return client
}

// reapIdle closes connections that have sat unused past IdleTimeout, so a burst
// of reads does not hold connections open against the accounts limit for the
// rest of the process. The next request dials again, which costs a handshake.
//
// It also closes the ones the server has already hung up on, which is what keeps
// an IdleTimeout set longer than the servers own from filling the pool with dead
// sockets that only a request would discover.
func (c *Client) reapIdle() {
	for {
		time.Sleep(c.reapPass())
	}
}

// reapPass closes what it should and reports how long until the oldest survivor
// comes due.
//
// It pops and pushes back as many times as the pool holds, which rotates it
// exactly once: every connection is looked at, the order it started in survives,
// and only one is ever out of the pool. Holding several out would let acquire
// dial replacements for connections that are coming back, and the account limit
// counts those.
func (c *Client) reapPass() time.Duration {
	// an empty pool has nothing due sooner than a connection released now
	wait := c.config.IdleTimeout

	for range len(c.idle) {
		select {
		case cn := <-c.idle:
			idle := time.Since(cn.lastUsed)
			if idle >= c.config.IdleTimeout || !cn.alive() {
				// its slot went back at release, so closing only lowers the
				// number of connections that exist
				cn.net.Close()
				continue
			}

			c.idle <- cn
			if due := c.config.IdleTimeout - idle; due < wait {
				wait = due
			}

		default:
		}
	}

	return wait
}

// GetSegment fetches an article by message-id and yenc-decodes it.
//
// Fetch and decode are one operation because a body cut short by a dropped
// connection only shows up as a decode failure, and that is exactly the case
// worth another attempt.
func (c *Client) GetSegment(group, id string) ([]byte, error) {
	var body []byte
	err := c.retry("getting segment "+id, func() error {
		var err error
		body, err = c.getSegment(group, id)
		return err
	})
	return body, err
}

// SegmentExists reports whether an article is present without transferring its
// body. STAT addressed by message-id needs no group selection (RFC 3977 6.2.4),
// so any connection can serve it.
func (c *Client) SegmentExists(id string) (bool, error) {
	var exists bool
	err := c.retry("stating segment "+id, func() error {
		var err error
		exists, err = c.segmentExists(id)
		return err
	})
	return exists, err
}

// retry runs op until it succeeds or the attempts run out, waiting a doubling
// backoff in between. A missing article is the servers final answer, and so are
// rejected credentials: both are reported as they are rather than retried.
func (c *Client) retry(what string, op func() error) error {
	var err error
	backoff := c.config.Backoff
	replacedStale := 0

	for attempt := 0; attempt < c.config.Attempts; {
		err = op()
		if err == nil || errors.Is(err, ErrArticleNotFound) || errors.Is(err, ErrAuthFailed) {
			return err
		}

		// A connection the server closed while it sat idle is not a failed
		// request. It is only ever discovered by using one, and each discovery
		// takes that connection out of the pool, so it costs neither an attempt
		// nor a wait. The whole pool idles out at once, so the allowance is the
		// size of the pool; past that the connections are fresh and the failure
		// is the requests.
		if errors.Is(err, errStaleConn) && replacedStale < c.config.MaxConns {
			replacedStale++
			continue
		}

		attempt++
		if attempt < c.config.Attempts {
			slog.Debug("Retrying nntp request", "operation", what, "attempt", attempt, "error", err)
			time.Sleep(backoff)
			backoff *= 2
		}
	}

	return fmt.Errorf("%s failed after %d attempts: %w", what, c.config.Attempts, err)
}

func (c *Client) getSegment(group, id string) ([]byte, error) {
	cn, reused, err := c.acquire()
	if err != nil {
		return nil, err
	}

	if err := cn.selectGroup(group); err != nil {
		c.drop(cn)
		return nil, staleIf(reused, err)
	}

	res, err := cn.Do("ARTICLE <%s>", id)
	if err != nil {
		c.drop(cn)
		return nil, staleIf(reused, fmt.Errorf("failed requesting article '%s': %w", id, err))
	}
	if res.Code == noArticleWithID {
		c.release(cn)
		return nil, fmt.Errorf("%w: '%s'", ErrArticleNotFound, id)
	}
	if res.Body == nil {
		c.drop(cn)
		return nil, fmt.Errorf("%w to article '%s': %d %s", ErrUnexpectedResponse, id, res.Code, res.Message)
	}

	part, err := yenc.Decode(res.Body)
	if err != nil {
		c.drop(cn)
		return nil, fmt.Errorf("failed yenc-decoding article '%s': %w", id, err)
	}

	// The decoder stops at the yenc trailer, which sits before the terminator of
	// the nntp response; a connection is only reusable once that is consumed too
	if _, err := io.Copy(io.Discard, res.Body); err != nil {
		c.drop(cn)
		return nil, fmt.Errorf("failed draining article '%s': %w", id, err)
	}

	c.release(cn)
	return part.Body, nil
}

func (c *Client) segmentExists(id string) (bool, error) {
	cn, reused, err := c.acquire()
	if err != nil {
		return false, err
	}

	res, err := cn.Do("STAT <%s>", id)
	if err != nil {
		c.drop(cn)
		return false, staleIf(reused, fmt.Errorf("failed stat for '%s': %w", id, err))
	}
	c.release(cn)

	switch res.Code {
	case articleExists:
		return true, nil
	case noArticleWithID:
		return false, nil
	default:
		return false, fmt.Errorf("%w to stat '%s': %d %s", ErrUnexpectedResponse, id, res.Code, res.Message)
	}
}

// acquire takes a connection slot and fills it with an idle connection or a new
// one, reporting which. The slot is held until release or drop, which is what
// bounds the client to MaxConns.
func (c *Client) acquire() (*conn, bool, error) {
	c.waiting.Add(1)
	<-c.slots
	c.waiting.Add(-1)

	cn, reused := c.takeIdle()
	if cn == nil {
		var err error
		cn, err = c.dial()
		if err != nil {
			c.slots <- struct{}{}
			return nil, false, err
		}
	}

	cn.deadline(c.config.Timeout)
	return cn, reused, nil
}

// takeIdle pops idle connections until it finds one the server has not closed,
// closing the dead ones on the way. They idle out as a group, so finding one
// dead usually means finding several.
func (c *Client) takeIdle() (*conn, bool) {
	for {
		select {
		case cn := <-c.idle:
			if cn.alive() {
				return cn, true
			}
			// its slot went back at release, so closing only lowers the number
			// of connections that exist
			cn.net.Close()

		default:
			return nil, false
		}
	}
}

// release returns a connection whose last response was read to its end, so the
// next command on it lines up with the next response.
func (c *Client) release(cn *conn) {
	cn.lastUsed = time.Now()
	c.idle <- cn
	c.slots <- struct{}{}
}

// drop closes a connection whose position in the response stream is unknown.
// Reusing one would read the remains of the previous response as the next one.
func (c *Client) drop(cn *conn) {
	cn.net.Close()
	c.slots <- struct{}{}
}

func (c *Client) dialServer() (*conn, error) {
	address := net.JoinHostPort(c.config.Host, strconv.Itoa(c.config.Port))

	dialer := &net.Dialer{Timeout: c.config.Timeout}

	var netConn net.Conn
	var err error
	if c.config.TLS {
		netConn, err = tls.DialWithDialer(dialer, "tcp", address, nil)
	} else {
		netConn, err = dialer.Dial("tcp", address)
	}
	if err != nil {
		return nil, fmt.Errorf("failed connecting to %s: %w", address, err)
	}

	// The handshake reads the servers welcome line, so it needs a deadline of
	// its own; acquire sets the one that covers the request itself
	cn := &conn{net: netConn}
	cn.deadline(c.config.Timeout)

	welcome, nntpConn, err := nntp.NewConn(netConn)
	if err != nil {
		netConn.Close()
		return nil, fmt.Errorf("failed nntp handshake with %s: %w", address, err)
	}
	if err := greeting(welcome); err != nil {
		netConn.Close()
		return nil, fmt.Errorf("%s refused the connection: %w", address, err)
	}
	cn.Conn = nntpConn

	if c.config.User != "" {
		if err := cn.authenticate(c.config.User, c.config.Pass); err != nil {
			netConn.Close()
			return nil, err
		}
	}
	return cn, nil
}

// greeting reads the code off the welcome line. nntp.NewConn hands it back
// without looking at it, and a server that is out of connections says so there
// rather than to the first command, which would then read the greeting as its
// own response.
func greeting(welcome string) error {
	fields := strings.Fields(welcome)
	if len(fields) == 0 {
		return fmt.Errorf("%w: empty welcome line", ErrUnexpectedResponse)
	}
	code, err := strconv.Atoi(fields[0])
	if err != nil {
		return fmt.Errorf("%w: unreadable welcome line %q", ErrUnexpectedResponse, welcome)
	}

	switch code {
	case serverReady, serverReadyNoPost:
		return nil
	case connsExceeded:
		return fmt.Errorf("%w: %s", ErrTooManyConnections, strings.TrimSpace(welcome))
	default:
		return fmt.Errorf("%w: %s", ErrUnexpectedResponse, strings.TrimSpace(welcome))
	}
}

// conn is a connection plus the group it currently has selected.
type conn struct {
	*nntp.Conn
	net      net.Conn
	group    string
	lastUsed time.Time
}

// alive reports whether the connection can still carry a command. A server
// closes an idle connection with a bare FIN, and tcp only reports that to a
// reader, so one that nobody reads looks fine until a command is sent on it.
//
// The look is a non-blocking read straight on the descriptor rather than a Read
// under a deadline in the past: the poller answers an expired deadline without
// issuing the syscall, so that never touches the socket at all. Only a definite
// close counts as dead, so anything this cannot interpret is left for the
// command to discover.
func (c *conn) alive() bool {
	raw := c.net
	if tlsConn, ok := raw.(*tls.Conn); ok {
		raw = tlsConn.NetConn()
	}
	syscallConn, ok := raw.(syscall.Conn)
	if !ok {
		return true
	}
	rawConn, err := syscallConn.SyscallConn()
	if err != nil {
		return true
	}

	dead := false
	if err := rawConn.Read(func(fd uintptr) bool {
		var b [1]byte
		n, err := syscall.Read(int(fd), b[:])
		// nothing to read is EAGAIN and is what a healthy idle connection looks
		// like; no bytes and no error is the close. Bytes would desync the next
		// response, so a connection that has any is no more usable than a closed
		// one - and under tls this read consumed one of them.
		dead = (n == 0 && err == nil) || n > 0 || errors.Is(err, syscall.ECONNRESET)
		return true
	}); err != nil {
		return true
	}
	return !dead
}

// deadline bounds one request. Without it a server that accepts a command and
// then says nothing holds its slot for as long as the process runs, which is
// worse than the connection being lost outright.
func (c *conn) deadline(timeout time.Duration) {
	if timeout <= 0 {
		return
	}
	c.net.SetDeadline(time.Now().Add(timeout))
}

// selectGroup issues GROUP only when the connection is not already on it.
func (c *conn) selectGroup(group string) error {
	if group == "" || c.group == group {
		return nil
	}

	res, err := c.Do("GROUP %s", group)
	if err != nil {
		return fmt.Errorf("failed selecting group '%s': %w", group, err)
	}
	if res.Code != groupJoined {
		return fmt.Errorf("%w to group '%s': %d %s", ErrUnexpectedResponse, group, res.Code, res.Message)
	}

	c.group = group
	return nil
}

// authenticate performs AUTHINFO. nntp.Conn.Auth reports success for a password
// the server rejected, so the response codes are checked here.
func (c *conn) authenticate(user, pass string) error {
	res, err := c.Do("AUTHINFO USER %s", user)
	if err != nil {
		return fmt.Errorf("failed sending username: %w", err)
	}

	if res.Code == passwordNeeded {
		res, err = c.Do("AUTHINFO PASS %s", pass)
		if err != nil {
			return fmt.Errorf("failed sending password: %w", err)
		}
	}

	// A rejection and a full account arrive the same way and are opposite
	// things: one will answer identically forever, the other passes on its own.
	// Anything else is a protocol answer this does not understand - 482 out of
	// sequence among them - which is worth another attempt rather than the
	// verdict that the credentials are wrong.
	switch res.Code {
	case authAccepted:
		return nil
	case authRejected:
		return fmt.Errorf("%w: %d %s", ErrAuthFailed, res.Code, res.Message)
	case connsExceeded:
		return fmt.Errorf("%w: %d %s", ErrTooManyConnections, res.Code, res.Message)
	default:
		return fmt.Errorf("%w to AUTHINFO: %d %s", ErrUnexpectedResponse, res.Code, res.Message)
	}
}
