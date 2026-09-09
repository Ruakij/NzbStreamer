package nntpclient

import (
	"crypto/tls"
	"net"
	"sync"
	"syscall"
	"time"
)

// socketInfo is what the kernel counted for one connection: the round trip time
// it measures while the connection is loaded, and how much of what it sent it
// had to send again. Retransmits are the one explanation for a link that went
// slow which nothing above tcp can see.
type socketInfo struct {
	rtt         time.Duration
	retransmits int64
	packets     int64
}

// socket is one live connection and what has already been read off it. Every
// connection past its handshake is one of these, whichever path holds it, which
// is what lets a collection read all of them from one place.
type socket struct {
	net net.Conn

	mu          sync.Mutex
	retransmits int64
	packets     int64
}

// since reports what the kernel counted beyond the last reading. The counters it
// keeps are per-socket totals, so a connection can be read as often as a scrape
// asks and each packet is still counted once.
func (s *socket) since(info socketInfo) (retransmits, packets int64) {
	s.mu.Lock()
	defer s.mu.Unlock()

	retransmits, packets = info.retransmits-s.retransmits, info.packets-s.packets
	s.retransmits, s.packets = info.retransmits, info.packets
	return retransmits, packets
}

// rawSocket reaches the descriptor under a connection, through tls where there
// is one.
func rawSocket(netConn net.Conn) (syscall.RawConn, bool) {
	raw := netConn
	if tlsConn, ok := raw.(*tls.Conn); ok {
		raw = tlsConn.NetConn()
	}

	syscallConn, ok := raw.(syscall.Conn)
	if !ok {
		return nil, false
	}
	rawConn, err := syscallConn.SyscallConn()
	if err != nil {
		return nil, false
	}
	return rawConn, true
}
