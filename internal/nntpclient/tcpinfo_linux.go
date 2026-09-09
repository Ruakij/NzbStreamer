//go:build linux

package nntpclient

import (
	"net"
	"time"

	"golang.org/x/sys/unix"
)

// socketStats reads TCP_INFO. Rtt is the kernel's smoothed round trip in
// microseconds and Total_retrans the segments this connection sent twice.
func socketStats(netConn net.Conn) (socketInfo, bool) {
	rawConn, ok := rawSocket(netConn)
	if !ok {
		return socketInfo{}, false
	}

	var info socketInfo
	read := false
	if err := rawConn.Control(func(fd uintptr) {
		tcp, err := unix.GetsockoptTCPInfo(int(fd), unix.IPPROTO_TCP, unix.TCP_INFO)
		if err != nil {
			return
		}
		info = socketInfo{
			rtt:         time.Duration(tcp.Rtt) * time.Microsecond,
			retransmits: int64(tcp.Total_retrans),
			packets:     int64(tcp.Segs_out),
		}
		read = true
	}); err != nil {
		return socketInfo{}, false
	}
	return info, read
}
