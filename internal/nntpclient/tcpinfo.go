package nntpclient

import (
	"crypto/tls"
	"net"
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
