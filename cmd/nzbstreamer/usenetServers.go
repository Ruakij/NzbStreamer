package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strconv"

	"github.com/sethvargo/go-envconfig"
)

// indexedHost matches the variable that declares a further server. A server
// exists because it has a host; everything else about it has a default.
var indexedHost = regexp.MustCompile(`^USENET_([0-9]+)_HOST=`)

// usenetServers reads USENET_* as the first server and USENET_<n>_* as the rest,
// in index order. go-envconfig has no list-of-structs form that fits this, so the
// indices are discovered by scanning the environment and each one is processed
// under its own prefix.
func usenetServers(ctx context.Context) ([]UsenetServerConfig, error) {
	var indices []int
	for _, entry := range os.Environ() {
		if match := indexedHost.FindStringSubmatch(entry); match != nil {
			index, _ := strconv.Atoi(match[1])
			indices = append(indices, index)
		}
	}
	sort.Ints(indices)

	var servers []UsenetServerConfig
	if _, unindexed := os.LookupEnv("USENET_HOST"); unindexed {
		first, err := usenetServer(ctx, "USENET_")
		if err != nil {
			return nil, err
		}
		if first.Priority == 0 {
			first.Priority = 1
		}
		servers = append(servers, first)
	}

	for _, index := range indices {
		prefix := fmt.Sprintf("USENET_%d_", index)
		server, err := usenetServer(ctx, prefix)
		if err != nil {
			return nil, err
		}
		// A backup server is a server on a priority of its own, which is what
		// the index already says
		if server.Priority == 0 {
			server.Priority = index
		}
		servers = append(servers, server)
	}

	if len(servers) == 0 {
		return nil, errors.New("no news server configured: set USENET_1_HOST, or USENET_HOST")
	}

	return servers, nil
}

func usenetServer(ctx context.Context, prefix string) (UsenetServerConfig, error) {
	var server UsenetServerConfig
	err := envconfig.ProcessWith(ctx, &envconfig.Config{
		Target:   &server,
		Lookuper: envconfig.PrefixLookuper(prefix, envconfig.OsLookuper()),
	})
	if err != nil {
		return server, fmt.Errorf("failed reading config of server %s: %w", prefix, err)
	}

	return server, nil
}
