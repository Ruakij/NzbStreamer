package nntpClient

import (
	"errors"
	"fmt"
	"slices"

	"astuart.co/nntp"
)

func SetupNntpClient(usenetHost string, usenetPort int, tls bool, user, pass string, maxConns int) (client *nntp.Client, err error) {
	client = nntp.NewClient(usenetHost, usenetPort)
	client.TLS = tls
	client.User = user
	client.Pass = pass
	client.SetMaxConns(maxConns)

	caps, err := client.Capabilities()
	if err != nil {
		err = fmt.Errorf("getting capabilities failed: %w", err)
		return
	}
	if slices.Contains(caps, "AUTHINFO USER PASS") {
		err = errors.New("auth failed")
		return
	}

	return
}
