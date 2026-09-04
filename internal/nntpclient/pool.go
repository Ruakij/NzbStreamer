package nntpclient

import (
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

var ErrNoServer = errors.New("no server available")

// Server is what a pool needs from a Client. It is an interface so the descent
// can be tested without a news server.
type Server interface {
	GetSegment(group, id string) ([]byte, error)
	SegmentExists(id string) (bool, error)
	Waiting() int
	Conns() int
}

// QuotaStore persists what a metered server has spent. A count that does not
// survive a restart is not a quota.
type QuotaStore interface {
	ServerUsage(name string) (used int64, periodStart time.Time, err error)
	// RecordServerUsage is called once per fetched article, so it buffers
	// rather than writing
	RecordServerUsage(name string, used int64, periodStart time.Time)
}

// ServerConfig places one server in a pool. Priority orders the servers, lower
// first; servers sharing a priority are one group a request spreads across round
// robin. QuotaBytes of 0 is no quota.
type ServerConfig struct {
	Server      Server
	Name        string
	Priority    int
	QuotaBytes  int64
	QuotaPeriod time.Duration
}

type poolServer struct {
	ServerConfig
	used        int64
	periodStart time.Time

	// consecutive failures, and until when the server is disabled for them.
	// permanent is a failure no waiting will fix, so nothing re-enables it
	failures      int
	disabledUntil time.Time
	disabledErr   error
	permanent     bool
}

type priority struct {
	servers []*poolServer
	next    atomic.Uint64
}

// BreakerConfig takes a server that keeps failing out of rotation. Failures
// counts the consecutive ones it takes, and 0 never disables a server for them;
// Cooldown is how long a disabled one is skipped for. Rejected credentials are
// not counted and not on a cooldown - see failed.
type BreakerConfig struct {
	Failures int
	Cooldown time.Duration
}

// Pool tries several servers for an article, in priority order. It offers the two
// methods a Client does, so nothing above the call site knows how many accounts
// there are.
type Pool struct {
	priorities []*priority
	store      QuotaStore
	breaker    BreakerConfig

	// ponytail: one lock for every servers quota counter and breaker state;
	// per-server locks if a pool ever grows past a handful
	quotaMutex sync.Mutex
}

// NewPool groups servers by priority and restores their quota counters.
func NewPool(servers []ServerConfig, store QuotaStore, breaker BreakerConfig) *Pool {
	pool := &Pool{store: store, breaker: breaker}

	byPriority := make(map[int]*priority)
	for _, config := range servers {
		server := &poolServer{ServerConfig: config}
		if store != nil && config.QuotaBytes > 0 {
			used, start, err := store.ServerUsage(config.Name)
			if err != nil {
				slog.Error("Failed reading server quota usage, starting it at zero", "server", config.Name, "error", err)
			}
			server.used, server.periodStart = used, start
		}
		if server.periodStart.IsZero() {
			server.periodStart = time.Now()
		}

		pr, ok := byPriority[config.Priority]
		if !ok {
			pr = &priority{}
			byPriority[config.Priority] = pr
			pool.priorities = append(pool.priorities, pr)
		}
		pr.servers = append(pr.servers, server)
	}

	sort.SliceStable(pool.priorities, func(i, j int) bool {
		return pool.priorities[i].servers[0].Priority < pool.priorities[j].servers[0].Priority
	})
	return pool
}

// GetSegment tries each server in turn until one has the article.
//
// A not-found is the servers final answer, so the descent carries on. So does a
// failure: Client has already retried it against that server, and by the time it
// reports one the server is down or refusing. Wrong credentials are not the
// articles fault and stop the descent, since the alternative is quietly spending
// a metered account on a typo.
func (p *Pool) GetSegment(group, id string) ([]byte, error) {
	var lastErr error
	missed := false

	for _, pr := range p.priorities {
		start := pr.start()
		for i := range pr.servers {
			server := pr.servers[(start+i)%len(pr.servers)]
			if !p.usable(server) {
				continue
			}

			body, err := server.Server.GetSegment(group, id)
			switch {
			case err == nil:
				p.succeeded(server)
				p.count(server, int64(len(body)))
				return body, nil
			case errors.Is(err, ErrArticleNotFound):
				p.succeeded(server)
				missed = true
			case errors.Is(err, ErrAuthFailed):
				p.failed(server, err)
				return nil, fmt.Errorf("%s: %w", server.Name, err)
			default:
				p.failed(server, err)
				lastErr = fmt.Errorf("%s: %w", server.Name, err)
			}
		}
	}

	// a server that failed leaves the article undecided, so its error is the
	// answer rather than the not-found another server gave
	switch {
	case lastErr != nil:
		return nil, lastErr
	case missed:
		return nil, fmt.Errorf("%w: '%s'", ErrArticleNotFound, id)
	default:
		return nil, p.noServer()
	}
}

// SegmentExists reports missing only once every server has missed, which is what
// keeps a release that is gone from the primary but whole on a secondary from
// failing its health check.
func (p *Pool) SegmentExists(id string) (bool, error) {
	var lastErr error
	answered := false

	for _, pr := range p.priorities {
		start := pr.start()
		for i := range pr.servers {
			server := pr.servers[(start+i)%len(pr.servers)]
			if !p.usable(server) {
				continue
			}

			exists, err := server.Server.SegmentExists(id)
			switch {
			case err != nil && errors.Is(err, ErrAuthFailed):
				p.failed(server, err)
				return false, fmt.Errorf("%s: %w", server.Name, err)
			case err != nil:
				p.failed(server, err)
				lastErr = fmt.Errorf("%s: %w", server.Name, err)
			case exists:
				p.succeeded(server)
				return true, nil
			default:
				p.succeeded(server)
				answered = true
			}
		}
	}

	switch {
	case answered:
		return false, nil
	case lastErr != nil:
		return false, lastErr
	default:
		return false, p.noServer()
	}
}

// Waiting reports the requests queued for a connection on the servers a fetch
// would actually use, which is the first priority still holding an unexhausted
// server. Counting the ones behind it would raise the number while the group
// doing the work is the saturated one.
func (p *Pool) Waiting() int {
	return p.sumActive(Server.Waiting)
}

// Conns reports the connections in play in that same group.
func (p *Pool) Conns() int {
	return p.sumActive(Server.Conns)
}

func (p *Pool) sumActive(of func(Server) int) int {
	for _, pr := range p.priorities {
		sum, any := 0, false
		for _, server := range pr.servers {
			if !p.usable(server) {
				continue
			}
			sum += of(server.Server)
			any = true
		}
		if any {
			return sum
		}
	}
	return 0
}

// ServerHealth is one configured server and why it is out of rotation, if it is.
type ServerHealth struct {
	Name     string
	Priority int
	Conns    int
	Up       bool
	// Reason is why it is not, empty while it is up
	Reason string
}

// Health reports every configured server, in priority order.
func (p *Pool) Health() []ServerHealth {
	health := make([]ServerHealth, 0, len(p.priorities))
	for _, pr := range p.priorities {
		for _, server := range pr.servers {
			reason := p.outOfRotation(server)
			health = append(health, ServerHealth{
				Name:     server.Name,
				Priority: server.Priority,
				Conns:    server.Server.Conns(),
				Up:       reason == "",
				Reason:   reason,
			})
		}
	}

	return health
}

// outOfRotation names what takes a server out of it, in the order it matters:
// rejected credentials never heal, a cooldown does, and a spent quota heals when
// its period rolls over. It asks without usable(), which would end a cooldown and
// roll a period as a side effect of being looked at.
func (p *Pool) outOfRotation(s *poolServer) string {
	p.quotaMutex.Lock()
	defer p.quotaMutex.Unlock()

	quotaLive := s.QuotaPeriod <= 0 || time.Since(s.periodStart) < s.QuotaPeriod

	switch {
	case s.permanent:
		return "auth rejected"
	case !s.disabledUntil.IsZero() && time.Now().Before(s.disabledUntil):
		return "breaker open"
	case s.QuotaBytes > 0 && s.used >= s.QuotaBytes && quotaLive:
		return "quota spent"
	default:
		return ""
	}
}

// start picks where in a group this request begins, so servers of equal priority
// spread the load across each other.
func (p *priority) start() int {
	return int(p.next.Add(1)-1) % len(p.servers)
}

func (p *Pool) usable(s *poolServer) bool {
	p.quotaMutex.Lock()
	defer p.quotaMutex.Unlock()

	if p.disabled(s) {
		return false
	}
	if s.QuotaBytes <= 0 {
		return true
	}

	p.rollPeriod(s)
	return s.used < s.QuotaBytes
}

// disabled reports whether a server is still serving out its cooldown, and lets
// one request through when it is over. That request decides the next cooldown on
// its own, which is why re-enabling leaves the failure count one short of the
// threshold rather than at zero.
func (p *Pool) disabled(s *poolServer) bool {
	if s.permanent {
		return true
	}
	if s.disabledUntil.IsZero() {
		return false
	}
	if time.Now().Before(s.disabledUntil) {
		return true
	}

	slog.Info("Trying a disabled news server again", "server", s.Name)
	s.disabledUntil = time.Time{}
	s.failures = p.breaker.Failures - 1
	return false
}

// failed counts a request the server could not answer. Client has already
// retried it there, so by the time one is reported the server is down, refusing
// or handing out truncated articles - all of which are worth taking it out of
// rotation rather than descending past it on every request.
//
// Not every failure is the servers though:
//
//   - rejected credentials will answer the same way for as long as the process
//     runs, so the server is disabled outright rather than retried on a cooldown,
//     and no failure count applies.
//   - the accounts connection limit is this process using more than it may
//     have. Disabling the server would be exactly the wrong answer, so it is
//     logged and otherwise ignored; using fewer connections is the only fix and
//     that is a matter of config.
func (p *Pool) failed(s *poolServer, err error) {
	if errors.Is(err, ErrTooManyConnections) {
		slog.Warn("News server is out of connections for this account; lower its MAX_CONN", "server", s.Name, "error", err)
		return
	}

	permanent := errors.Is(err, ErrAuthFailed)
	if !permanent && p.breaker.Failures <= 0 {
		return
	}

	p.quotaMutex.Lock()
	defer p.quotaMutex.Unlock()

	if s.permanent || !s.disabledUntil.IsZero() {
		return
	}

	s.failures++
	if !permanent && s.failures < p.breaker.Failures {
		return
	}

	s.disabledErr = err
	if permanent {
		s.permanent = true
		slog.Error("Disabling a news server that rejected its credentials; nothing here will fix that", "server", s.Name, "error", err)
		return
	}

	s.disabledUntil = time.Now().Add(p.breaker.Cooldown)
	// ponytail: one flat cooldown; back it off per disable if a server flaps
	slog.Warn("Disabling a news server that keeps failing", "server", s.Name, "failures", s.failures, "cooldown", p.breaker.Cooldown, "error", err)
}

func (p *Pool) succeeded(s *poolServer) {
	if p.breaker.Failures <= 0 {
		return
	}

	p.quotaMutex.Lock()
	defer p.quotaMutex.Unlock()

	s.failures = 0
	s.disabledErr = nil
}

// noServer reports that every server is disabled or out of quota, and carries
// what one of them was disabled for. A pool of one whose credentials are wrong
// would otherwise say only that it has nothing left to try.
func (p *Pool) noServer() error {
	p.quotaMutex.Lock()
	defer p.quotaMutex.Unlock()

	for _, pr := range p.priorities {
		for _, s := range pr.servers {
			if s.disabledErr != nil {
				return fmt.Errorf("%w, %s is disabled: %w", ErrNoServer, s.Name, s.disabledErr)
			}
		}
	}
	return ErrNoServer
}

// count charges a fetch to the server that served it. Overshooting the allowance
// by one article is fine; knowing the size beforehand would cost a STAT.
func (p *Pool) count(s *poolServer, bytes int64) {
	if s.QuotaBytes <= 0 {
		return
	}

	p.quotaMutex.Lock()
	p.rollPeriod(s)
	s.used += bytes
	used, start := s.used, s.periodStart
	p.quotaMutex.Unlock()

	if p.store != nil {
		p.store.RecordServerUsage(s.Name, used, start)
	}
}

// rollPeriod starts a new allowance once the old one has run its length. Nothing
// calendar-aware: a providers month boundary is not knowable from here.
func (p *Pool) rollPeriod(s *poolServer) {
	if s.QuotaPeriod <= 0 || time.Since(s.periodStart) < s.QuotaPeriod {
		return
	}
	s.used = 0
	s.periodStart = time.Now()
}
