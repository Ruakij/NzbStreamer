package nntpclient

import (
	"errors"
	"fmt"
	"slices"

	"astuart.co/nntp"
)

var ErrAuthFailed = errors.New("auth failed")

func SetupNntpClient(usenetHost string, usenetPort int, tls bool, user, pass string, maxConns int) (*nntp.Client, error) {
	client := nntp.NewClient(usenetHost, usenetPort)
	client.TLS = tls
	client.User = user
	client.Pass = pass
	client.SetMaxConns(maxConns)

	caps, err := client.Capabilities()
	if err != nil {
		err = fmt.Errorf("getting capabilities failed: %w", err)
		return client, err
	}
	if slices.Contains(caps, "AUTHINFO USER PASS") {
		return client, ErrAuthFailed
	}

	return client, err
}
