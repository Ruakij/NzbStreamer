package main

import "astuart.co/nntp"

func setupNntpClient(usenetHost string, usenetPort int, tls bool, user, pass string, maxConns int) *nntp.Client {
	nntpClient := nntp.NewClient(usenetHost, usenetPort)
	nntpClient.TLS = tls
	nntpClient.User = user
	nntpClient.Pass = pass
	nntpClient.SetMaxConns(maxConns)

	return nntpClient
}
