package main

import (
	"context"
	"testing"
	"time"
)

func TestUsenetServersReadsIndexedServers(t *testing.T) {
	t.Setenv("USENET_HOST", "primary.example")
	t.Setenv("USENET_USER", "user")
	t.Setenv("USENET_PASS", "pass")
	t.Setenv("USENET_2_HOST", "block.example")
	t.Setenv("USENET_2_USER", "user")
	t.Setenv("USENET_2_PASS", "pass")
	t.Setenv("USENET_2_MAX_CONN", "5")
	t.Setenv("USENET_2_QUOTA_BYTES", "1000")
	t.Setenv("USENET_2_QUOTA_PERIOD", "24h")

	servers, err := usenetServers(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(servers) != 2 {
		t.Fatalf("found %d servers; want the unindexed one and the indexed one", len(servers))
	}

	if servers[0].Host != "primary.example" || servers[0].Priority != 1 || servers[0].MaxConn != 20 {
		t.Fatalf("first server is %+v; want the unindexed variables on priority 1 with the default connections", servers[0])
	}
	want := UsenetServerConfig{
		Host: "block.example", Port: 563, TLS: true, User: "user", Password: "pass",
		MaxConn: 5, Priority: 2, QuotaBytes: 1000, QuotaPeriod: 24 * time.Hour,
	}
	if servers[1] != want {
		t.Fatalf("second server is %+v; want %+v", servers[1], want)
	}
}

func TestUsenetServersWithoutUnindexedServer(t *testing.T) {
	t.Setenv("USENET_1_HOST", "primary.example")
	t.Setenv("USENET_1_USER", "user")
	t.Setenv("USENET_1_PASS", "pass")

	servers, err := usenetServers(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(servers) != 1 || servers[0].Host != "primary.example" || servers[0].Priority != 1 {
		t.Fatalf("found %+v; want the indexed server alone on priority 1", servers)
	}
}

func TestUsenetServersWithoutAnyServer(t *testing.T) {
	if _, err := usenetServers(context.Background()); err == nil {
		t.Fatal("no error for an environment configuring no server")
	}
}
