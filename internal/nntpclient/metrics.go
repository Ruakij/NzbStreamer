package nntpclient

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"git.ruekov.eu/ruakij/nzbStreamer/pkg/yenc"
)

// The instruments of this package. OTel hands back a working no-op instrument
// along with any error, and the only error a fixed name and unit can produce is
// a malformed one, so the error is dropped.
var (
	meter = otel.Meter("internal/nntpclient")

	fetchDuration, _ = meter.Float64Histogram("nntp.fetch.duration",
		metric.WithDescription("Time one server took to answer an article request"),
		metric.WithUnit("s"))
	fetchedArticles, _ = meter.Int64Counter("nntp.fetch.articles",
		metric.WithDescription("Article requests by the server that answered and how it did"))
	fetchedBytes, _ = meter.Int64Counter("nntp.fetch.bytes",
		metric.WithDescription("Decoded article bytes per server"),
		metric.WithUnit("By"))
	queueLatency, _ = meter.Float64Histogram("nntp.queue.latency",
		metric.WithDescription("Time a request waited before a connection was available for it"),
		metric.WithUnit("s"))
	breakerTrips, _ = meter.Int64Counter("nntp.breaker.trips",
		metric.WithDescription("Times a server was taken out of rotation"))
	serverUp, _ = meter.Int64ObservableGauge("nntp.server.up",
		metric.WithDescription("1 while a server is in rotation, 0 while something holds it out: rejected credentials, an open breaker or a spent quota. The trips count when a server dropped out, this is whether it is still out, which is what a server left disabled for an hour looks like and what a cooldown ending or a quota period rolling changes with no event to count"))
	responseLatency, _ = meter.Float64Histogram("nntp.response.latency",
		metric.WithDescription("Time the server took to answer with a status line, by what it was answering; the body transfer that follows an ARTICLE is not in it, which is what separates it from nntp.fetch.duration. On a pipelined connection a command is written long before its response is read, so what is timed is the wait once the reader reaches it"),
		metric.WithUnit("s"))
	socketConnectLatency, _ = meter.Float64Histogram("nntp.socket.connect.latency",
		metric.WithDescription("Time one phase of building the transport took; the tcp phase is a round trip and nothing else, so it separates a slow link from a slow server"),
		metric.WithUnit("s"))
	connectionsClosed, _ = meter.Int64Counter("nntp.connections.closed",
		metric.WithDescription("Connections taken out of service, by what ended them"))
	connectionsOpen, _ = meter.Int64ObservableGauge("nntp.connections.open",
		metric.WithDescription("Connections to one server that exist right now, idle ones included"))
	connectionsLimit, _ = meter.Int64ObservableGauge("nntp.connections.limit",
		metric.WithDescription("Connections one server is allowed at once, which is what the open ones sitting at it explains a request queueing for one"))
	quotaUsed, _ = meter.Int64ObservableGauge("nntp.quota.used",
		metric.WithDescription("Bytes a metered server has served in the period it is in; it drops to zero when the period rolls"),
		metric.WithUnit("By"))
	quotaLimit, _ = meter.Int64ObservableGauge("nntp.quota.limit",
		metric.WithDescription("Bytes a server may serve per period before it is skipped; 0 is unmetered"),
		metric.WithUnit("By"))
	socketRTT, _ = meter.Float64Histogram("nntp.socket.rtt",
		metric.WithDescription("Round trip the kernel measured over a connection, sampled at every scrape while it is open and once more as it closes, which unlike the tcp connect phase is measured under load"),
		metric.WithUnit("s"))
	socketRetransmits, _ = meter.Int64Counter("nntp.socket.retransmits",
		metric.WithDescription("Packets a connection had to send again, over the packets it sent, which is a lossy path and is invisible above tcp"))
	socketPackets, _ = meter.Int64Counter("nntp.socket.packets",
		metric.WithDescription("Packets a connection sent, which is what the retransmits are a fraction of"))
	errorCount, _ = meter.Int64Counter("nntp.errors",
		metric.WithDescription("Failed attempts against one server by what the error was, counted per attempt rather than per request"))
	pipelineConnections, _ = meter.Int64ObservableGauge("nntp.pipeline.connections",
		metric.WithDescription("Pipelined connections in service"))
	pipelineInflight, _ = meter.Int64ObservableGauge("nntp.pipeline.inflight",
		metric.WithDescription("Commands written across all pipelined connections that are outstanding; over the connections it is the mean load a connection carries"))
	pipelineMaxLoad, _ = meter.Int64ObservableGauge("nntp.pipeline.max_load",
		metric.WithDescription("Commands outstanding on the deepest connection; against the mean it is how evenly the load sits, which a connection draining slower than the rest pulls apart"))
	pipelineWindow, _ = meter.Int64ObservableGauge("nntp.pipeline.window",
		metric.WithDescription("Commands one connection is allowed to have outstanding, which is the ceiling the inflight over connections is read against"))
	selectionCost, _ = meter.Float64ObservableGauge("nntp.select.cost",
		metric.WithDescription("Seconds a server is expected to take over the articles already in flight there and one more, which is what a request is routed by; a server whose measurement has expired reports what its group costs"),
		metric.WithUnit("s"))
	selectionInflight, _ = meter.Int64ObservableGauge("nntp.select.inflight",
		metric.WithDescription("Article requests piled onto one server that have not been answered yet, which is the term that makes nntp.select.cost rise for a busy server rather than a slow one. Unlike nntp.pipeline.inflight it counts the ones still waiting for a connection, so it keeps climbing where that one flatlines at the window, and it is reported for a server that does not pipeline"))
	selectionDisplaced, _ = meter.Int64Counter("nntp.select.displaced",
		metric.WithDescription("Requests the rotation would have sent to a server that went to a cheaper one instead, labelled with the server that was passed over; where they went is nntp.fetch.articles, and this is the server costing enough to be worth avoiding"))
)

// recordCtx is what the record calls take. Nothing here is cancellable and no
// caller's context reaches the reaper or the pipeline goroutines, so it is one
// background context rather than one per measurement.
var recordCtx = context.Background()

// serverKey labels a measurement with the server it is about. The configured
// name is the host, which is what an operator recognises.
const serverKey = attribute.Key("server")

// outcome of one request against one server, which is what separates a descent
// past a server that missed from one past a server that failed.
const (
	outcomeOK      = "ok"
	outcomeMissing = "missing"
	outcomeError   = "error"
)

var outcomeKey = attribute.Key("outcome")

// What the server was answering. The greeting is the one it sends unasked, the
// rest are commands. The set is fixed, so the label stays a handful of series
// per server.
const (
	responseGreeting = "GREETING"
	responseAuth     = "AUTH"
	responseGroup    = "GROUP"
	responseArticle  = "ARTICLE"
	responseStat     = "STAT"
)

var responseKey = attribute.Key("response")

// The phases of building the transport, timed apart because tcp is the round
// trip and tls is the handshake on top of it.
const (
	phaseTCP = "tcp"
	phaseTLS = "tls"
)

var phaseKey = attribute.Key("phase")

// Why a connection ended. Idle is the reaper closing what demand stopped
// justifying and costs nothing but a handshake; dead is the server having hung
// up on it unannounced; error is a command that lost the response stream.
const (
	closeIdle  = "idle"
	closeDead  = "dead"
	closeError = "error"
)

var reasonKey = attribute.Key("reason")

var kindKey = attribute.Key("kind")

// recordResponse times one status line, from the command going out to it
// coming back; the greeting is timed from the socket being usable.
func (c *Client) recordResponse(response string, started time.Time) {
	responseLatency.Record(recordCtx, time.Since(started).Seconds(),
		metric.WithAttributes(serverKey.String(c.config.Name), responseKey.String(response)))
}

// recordConnect times one phase of building the transport.
func (c *Client) recordConnect(phase string, started time.Time) {
	socketConnectLatency.Record(recordCtx, time.Since(started).Seconds(),
		metric.WithAttributes(serverKey.String(c.config.Name), phaseKey.String(phase)))
}

func (c *Client) recordClose(reason string) {
	connectionsClosed.Add(recordCtx, 1,
		metric.WithAttributes(serverKey.String(c.config.Name), reasonKey.String(reason)))
}

// track takes a connection into the set a collection reads, once its handshake
// has made it one. A dial that failed never enters it, so nothing has to take it
// back out.
func (c *Client) track(netConn net.Conn) *socket {
	s := &socket{net: netConn}

	c.socketMutex.Lock()
	defer c.socketMutex.Unlock()

	c.sockets[s] = struct{}{}
	return s
}

// untrack drops a connection that is about to be closed, so a collection does
// not read a descriptor that is on its way out.
func (c *Client) untrack(s *socket) {
	c.socketMutex.Lock()
	defer c.socketMutex.Unlock()

	delete(c.sockets, s)
}

// recordSockets reads every connection this client holds open. Which path holds
// one makes no difference to the kernel, and a connection carrying a download is
// held for as long as the download lasts, so reading them where they stand is
// what the counters are worth having for at all.
func (c *Client) recordSockets(ctx context.Context) {
	c.socketMutex.Lock()
	live := make([]*socket, 0, len(c.sockets))
	for s := range c.sockets {
		live = append(live, s)
	}
	c.socketMutex.Unlock()

	for _, s := range live {
		c.recordSocket(ctx, s)
	}
}

// recordSocket takes what the kernel counted for one connection, which is what
// has arrived since the last reading of it. The last reading is the one before
// it closes, since a closed descriptor has nothing left to read.
//
// A connection that sent nothing since it was last read is skipped: its round
// trip is the same measurement again, which is a sample of nothing rather than
// of an idle link, and there is nothing for the counters to add.
func (c *Client) recordSocket(ctx context.Context, s *socket) {
	info, ok := socketStats(s.net)
	if !ok {
		return
	}
	retransmits, packets := s.since(info)
	if packets == 0 {
		return
	}

	server := metric.WithAttributes(serverKey.String(c.config.Name))
	socketRTT.Record(ctx, info.rtt.Seconds(), server)
	socketRetransmits.Add(ctx, retransmits, server)
	socketPackets.Add(ctx, packets, server)
}

// recordError counts one failed attempt. It sits on the retry, which every
// request passes through, so a failure that another attempt hid is counted as
// well as one that reached the caller.
func (c *Client) recordError(err error) {
	errorCount.Add(recordCtx, 1,
		metric.WithAttributes(serverKey.String(c.config.Name), kindKey.String(errorKind(err))))
}

// observeState reads what the client holds at collection time, the way the
// reaper and the dispatch see it, and takes the kernel's counters off the
// connections that are open. One callback per client, so the lock is taken
// once per scrape. The pipeline gauges are only observed by a client that
// pipelines, since a zero there would read as a window nothing is using rather
// than as a path that is not running.
func (c *Client) observeState() {
	server := metric.WithAttributes(serverKey.String(c.config.Name))

	instruments := []metric.Observable{connectionsOpen, connectionsLimit}
	if c.canPipe {
		instruments = append(instruments, pipelineConnections, pipelineInflight, pipelineMaxLoad, pipelineWindow)
	}

	_, err := meter.RegisterCallback(func(ctx context.Context, observer metric.Observer) error {
		observer.ObserveInt64(connectionsOpen, int64(c.OpenConns()), server)
		observer.ObserveInt64(connectionsLimit, int64(c.config.MaxConns), server)
		// The sockets are read here rather than only as a connection closes. The
		// sync instruments are written from inside the callback, which the sdk
		// runs before it reads the aggregations, so a sample reaches the scrape
		// that took it.
		c.recordSockets(ctx)

		if !c.canPipe {
			return nil
		}

		c.mu.Lock()
		inflight, deepest := 0, 0
		for p := range c.pipes {
			inflight += p.load
			deepest = max(deepest, p.load)
		}
		connections := len(c.pipes)
		c.mu.Unlock()

		observer.ObserveInt64(pipelineConnections, int64(connections), server)
		observer.ObserveInt64(pipelineInflight, int64(inflight), server)
		observer.ObserveInt64(pipelineMaxLoad, int64(deepest), server)
		observer.ObserveInt64(pipelineWindow, int64(c.config.ConnectionPipeliningSize), server)
		return nil
	}, instruments...)
	if err != nil {
		slog.Warn("Failed registering the nntp client metrics", "server", c.config.Name, "error", err)
	}
}

// observeServers reports each server's quota against its allowance, which is
// what turns a server dropping out of rotation into something visible before it
// happens, and whether it is in rotation at all. A period that has run out reads
// as zero used, the way the next fetch will roll it, and the rotation is read
// the way the descent reads it, without ending a cooldown by looking.
func (p *Pool) observeServers() {
	_, err := meter.RegisterCallback(func(_ context.Context, observer metric.Observer) error {
		p.quotaMutex.Lock()
		defer p.quotaMutex.Unlock()

		for _, pr := range p.priorities {
			for _, s := range pr.servers {
				server := metric.WithAttributes(serverKey.String(s.Name))

				used := s.used
				if s.QuotaPeriod > 0 && time.Since(s.periodStart) >= s.QuotaPeriod {
					used = 0
				}
				observer.ObserveInt64(quotaUsed, used, server)
				observer.ObserveInt64(quotaLimit, s.QuotaBytes, server)

				up := int64(1)
				if outOfRotationLocked(s) != "" {
					up = 0
				}
				observer.ObserveInt64(serverUp, up, server)
			}
		}
		return nil
	}, quotaUsed, quotaLimit, serverUp)
	if err != nil {
		slog.Warn("Failed registering the nntp server metrics", "error", err)
	}
}

// errorKind names an error by what an operator would do about it, which is what
// separates a provider refusing credentials from a link dropping bodies.
func errorKind(err error) string {
	var netErr net.Error
	switch {
	case errors.Is(err, ErrAuthFailed):
		return "auth"
	case errors.Is(err, ErrTooManyConnections):
		return "conns_exceeded"
	// a connection the server closed while it sat idle, which is expected and
	// not a failed request
	case errors.Is(err, errStaleConn):
		return "stale"
	case errors.Is(err, yenc.ErrTruncated), errors.Is(err, yenc.ErrSize), errors.Is(err, yenc.ErrCRC), errors.Is(err, yenc.ErrNoHeader):
		return "decode"
	case errors.Is(err, ErrUnexpectedResponse):
		return "protocol"
	case errors.As(err, &netErr) && netErr.Timeout():
		return "timeout"
	case errors.As(err, new(*net.OpError)):
		return "connect"
	default:
		return "other"
	}
}

// recordWait times what a request spent before it had a connection to speak on,
// which is what a pool too small for the read rate shows up as.
func (c *Client) recordWait(started time.Time) {
	queueLatency.Record(recordCtx, time.Since(started).Seconds(),
		metric.WithAttributes(serverKey.String(c.config.Name)))
}
