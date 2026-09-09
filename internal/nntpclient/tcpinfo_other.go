//go:build !linux && !darwin

package nntpclient

import "net"

// socketStats has nothing to read where the kernel does not offer TCP_INFO, and
// the socket metrics are simply absent.
func socketStats(net.Conn) (socketInfo, bool) {
	return socketInfo{}, false
}
