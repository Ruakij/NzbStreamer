package main

import (
	"context"
	"testing"

	"github.com/sethvargo/go-envconfig"
)

func TestBytesUnmarshal(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want Bytes
	}{
		{"0", 0},
		{"", 0},
		{"1024", 1024},
		{"12M", 12 * 1024 * 1024},
		{"12MB", 12 * 1024 * 1024},
		{"12MiB", 12 * 1024 * 1024},
		{" 8k ", 8 * 1024},
		{"12m", 12 * 1024 * 1024},
		{"12mib", 12 * 1024 * 1024},
		{"1G", 1024 * 1024 * 1024},
		{"1g", 1024 * 1024 * 1024},
		{"2T", 2 * 1024 * 1024 * 1024 * 1024},
	} {
		var got Bytes
		if err := got.UnmarshalText([]byte(tc.in)); err != nil {
			t.Errorf("%q: %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("%q = %d, want %d", tc.in, got, tc.want)
		}
	}

	for _, in := range []string{"M", "12X", "twelve", "12 M B"} {
		var got Bytes
		if err := got.UnmarshalText([]byte(in)); err == nil {
			t.Errorf("%q = %d, want an error", in, got)
		}
	}
}

// envconfig has to reach UnmarshalText for any of this to matter, defaults
// included.
func TestBytesThroughEnvconfig(t *testing.T) {
	var config ReadaheadConfig
	lookup := envconfig.MapLookuper(map[string]string{"READAHEAD_SIZE": "12M"})
	if err := envconfig.ProcessWith(context.Background(), &envconfig.Config{
		Target: &config, Lookuper: lookup,
	}); err != nil {
		t.Fatal(err)
	}

	if config.Size != 12*1024*1024 {
		t.Errorf("size = %d, want 12M", config.Size)
	}
	if config.Chunk != 1024*1024 {
		t.Errorf("chunk = %d, want the 1M default", config.Chunk)
	}
}
