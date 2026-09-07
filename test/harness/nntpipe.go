package harness

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"
)

// DefaultBaselinePipelineSize is how many ARTICLE requests a baseline socket keeps
// outstanding. Fixture articles are ~768 KiB, so 32 bounds per-socket buffering
// to ~24 MiB while keeping the socket busy across one link RTT. It is far past
// what the app itself runs with (NNTP_PIPELINE_SIZE defaults to 4) on purpose:
// this measures what the server and the link can deliver, not what the app asks
// for. Run the baseline at the app's window to see what the extra depth buys.
const DefaultBaselinePipelineSize = 32

// articleTimeout bounds the wait for one pipelined response, so a missing or
// wedged article cannot hang the baseline behind a socket that will never
// answer.
const articleTimeout = 30 * time.Second

// nntConn is one pipelined NNTP socket for the news baseline. NNTP answers
// commands in order on a single socket (RFC 3977 3.5), so requests can be
// written ahead of the reader and the socket never idles on RTT: latency is
// hidden rather than added to each article.
type nntConn struct {
	conn   net.Conn
	reader *bufio.Reader
}

// dialConn opens a socket and consumes the server greeting, which every NNTP
// server sends unprompted: left in the stream it would be read as the response
// to the first command and put every reply behind it one out of step.
func dialConn(host string, port int) (*nntConn, error) {
	c, err := net.Dial("tcp", net.JoinHostPort(host, strconv.Itoa(port)))
	if err != nil {
		return nil, err
	}
	n := &nntConn{conn: c, reader: bufio.NewReaderSize(c, 64*1024)}
	code, err := n.readStatus()
	if err != nil {
		c.Close()
		return nil, fmt.Errorf("greeting: %w", err)
	}
	if code != 200 && code != 201 {
		c.Close()
		return nil, fmt.Errorf("greeting: code %d", code)
	}
	return n, nil
}

func (n *nntConn) close() { n.conn.Close() }

func (n *nntConn) send(cmd string) error {
	_, err := io.WriteString(n.conn, cmd+"\r\n")
	return err
}

// readStatus reads one status line "NNN <rest>\r\n" and returns its code.
func (n *nntConn) readStatus() (int, error) {
	line, err := n.reader.ReadString('\n')
	if err != nil {
		return 0, err
	}
	line = strings.TrimRight(line, "\r\n")
	if len(line) < 3 {
		return 0, fmt.Errorf("short status line %q", line)
	}
	code, err := strconv.Atoi(line[:3])
	if err != nil {
		return 0, fmt.Errorf("bad status code in %q", line)
	}
	return code, nil
}

// readBody reads up to the ".\r\n" terminator and returns the body byte count
// (article lines), excluding the status line and the terminator itself. It is
// only called for successful ARTICLE replies, which always carry a body; error
// replies are single-line and must not be drained here (nothing follows).
func (n *nntConn) readBody() (int64, error) {
	var total int64
	for {
		line, err := n.reader.ReadString('\n')
		if err != nil {
			return total, err
		}
		if line == ".\r\n" || line == ".\n" {
			return total, nil
		}
		total += int64(len(line))
	}
}

// auth sends AUTHINFO USER/PASS, one command per round trip, before the timed
// region so authentication never poisons the throughput measurement.
func (n *nntConn) auth(user, pass string) error {
	for _, cmd := range []string{"AUTHINFO USER " + user, "AUTHINFO PASS " + pass} {
		if err := n.send(cmd); err != nil {
			return err
		}
		code, err := n.readStatus()
		if err != nil {
			return err
		}
		if code >= 500 {
			return fmt.Errorf("%s: code %d", strings.SplitN(cmd, " ", 3)[1], code)
		}
	}
	return nil
}

// warm pipelines depth articles and discards them, so the timer starts on a
// socket that is past slow start at the full window depth and on a server that
// has the fixture in its page cache. The ids cycle, so a short fixture warms as
// deep as a long one.
func (n *nntConn) warm(ids []string, depth int) error {
	for i := range depth {
		if err := n.send("ARTICLE <" + ids[i%len(ids)] + ">"); err != nil {
			return err
		}
	}
	for i := range depth {
		if _, ok := n.readResp(); !ok {
			return fmt.Errorf("ARTICLE <%s> failed", ids[i%len(ids)])
		}
	}
	return nil
}

// readResp reads one pipelined ARTICLE response and reports the body byte count
// and whether the article was retrieved. Success replies (220/222) carry a
// ".\r\n"-terminated body which is drained; error replies are single-line, so
// the next pipelined response follows directly with nothing to skip. A failed
// read leaves the socket at an unknown position, so its caller stops using it.
func (n *nntConn) readResp() (int64, bool) {
	_ = n.conn.SetReadDeadline(time.Now().Add(articleTimeout))
	defer func() { _ = n.conn.SetReadDeadline(time.Time{}) }()
	code, err := n.readStatus()
	if err != nil {
		return 0, false
	}
	if code != 220 && code != 222 {
		return 0, false
	}
	body, err := n.readBody()
	if err != nil {
		return 0, false
	}
	return body, true
}

// pumpState is one socket's share of a pump run: the ids handed to it, whether
// it is still usable, and what it retrieved.
type pumpState struct {
	inbox    chan string
	dead     chan struct{} // closed when this socket stops answering
	articles int
	bytes    int64
}

// pumpAll fetches every id over the sockets and returns what they retrieved
// together. Slots are dealt rather than pulled: the free list starts with one
// slot per socket, then a second per socket, and so on, so every socket has its
// first request before any has its second. A pulled queue lets the socket that
// happens to start first claim a whole window - 32 of the 44 articles of a
// fixture - while the others sit idle, which on a high-latency link measures
// two sockets and reports a ceiling below what the app itself achieves.
//
// Each socket is a writer goroutine and a reader goroutine over the one socket,
// as the app's pipelined path is: the writer sends while the reader is still
// reading, so the window stays full and latency is hidden rather than paid per
// article. A socket that fails stops taking work; its outstanding ids are lost
// and counted by the caller as the shortfall.
func pumpAll(ctx context.Context, conns []*nntConn, ids []string, window int) (articles int, bytes int64, elapsed time.Duration) {
	free := make(chan int, len(conns)*window)
	for range window {
		for i := range conns {
			free <- i
		}
	}

	st := make([]*pumpState, len(conns))
	var wg sync.WaitGroup
	start := time.Now()
	for i, c := range conns {
		s := &pumpState{inbox: make(chan string, window), dead: make(chan struct{})}
		st[i] = s
		// One token per outstanding request, handed from the writer to the
		// reader: the responses come back in the order the commands went out
		// (RFC 3977 3.5), so nothing more than the count has to be passed.
		inflight := make(chan struct{}, window)
		wg.Add(2)
		go func() {
			defer wg.Done()
			defer close(inflight)
			for id := range s.inbox {
				if err := c.send("ARTICLE <" + id + ">"); err != nil {
					return
				}
				inflight <- struct{}{}
			}
		}()
		go func() {
			defer wg.Done()
			defer close(s.dead)
			for range inflight {
				body, ok := c.readResp()
				if !ok {
					return
				}
				s.articles++
				s.bytes += body
				free <- i
			}
		}()
	}

dispatch:
	for _, id := range ids {
		for {
			i, ok := pickSocket(ctx, free, st)
			if !ok {
				break dispatch
			}
			select {
			case st[i].inbox <- id:
			case <-st[i].dead:
				continue // it died between the slot and the hand-over
			}
			break
		}
	}
	for _, s := range st {
		close(s.inbox)
	}
	wg.Wait()
	elapsed = time.Since(start)
	for _, s := range st {
		articles += s.articles
		bytes += s.bytes
	}
	return articles, bytes, elapsed
}

// pickSocket takes the next free slot belonging to a socket that is still
// alive. A dead socket's slots never come back, so the wait is bounded by a
// re-check: with every socket gone there is nothing left to dispatch to.
func pickSocket(ctx context.Context, free chan int, st []*pumpState) (int, bool) {
	for {
		select {
		case i := <-free:
			select {
			case <-st[i].dead:
				continue
			default:
				return i, true
			}
		case <-ctx.Done():
			return 0, false
		case <-time.After(50 * time.Millisecond):
			if allDead(st) {
				return 0, false
			}
		}
	}
}

func allDead(st []*pumpState) bool {
	for _, s := range st {
		select {
		case <-s.dead:
		default:
			return false
		}
	}
	return true
}
