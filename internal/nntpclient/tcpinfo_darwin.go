//go:build darwin

package nntpclient

import (
	"net"
	"time"

	"golang.org/x/sys/unix"
)

// socketStats reads TCP_CONNECTION_INFO, which is darwin's TCP_INFO under
// another name and with its own units: Srtt is the smoothed round trip in
// milliseconds, and the retransmits are packets rather than segments.
func socketStats(netConn net.Conn) (socketInfo, bool) {
	rawConn, ok := rawSocket(netConn)
	if !ok {
		return socketInfo{}, false
	}

	var info socketInfo
	read := false
	if err := rawConn.Control(func(fd uintptr) {
		tcp, err := unix.GetsockoptTCPConnectionInfo(int(fd), unix.IPPROTO_TCP, unix.TCP_CONNECTION_INFO)
		if err != nil {
			return
		}
		info = socketInfo{
			rtt:         time.Duration(tcp.Srtt) * time.Millisecond,
			retransmits: int64(tcp.Txretransmitpackets),
			packets:     int64(tcp.Txpackets),
		}
		read = true
	}); err != nil {
		return socketInfo{}, false
	}
	return info, read
}
