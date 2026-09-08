package main

import (
	"context"
	"testing"

	"github.com/sethvargo/go-envconfig"
)

// envconfig has to reach UnmarshalText for any of this to matter, defaults
// included.
func TestBytesThroughEnvconfig(t *testing.T) {
	var config ReadaheadConfig
	lookup := envconfig.MapLookuper(map[string]string{"READAHEAD_MAX_SIZE": "12M"})
	if err := envconfig.ProcessWith(context.Background(), &envconfig.Config{
		Target: &config, Lookuper: lookup,
	}); err != nil {
		t.Fatal(err)
	}

	if config.MaxSize != 12*1024*1024 {
		t.Errorf("max size = %d, want 12M", config.MaxSize)
	}
	if config.Chunk != 1024*1024 {
		t.Errorf("chunk = %d, want the 1M default", config.Chunk)
	}
}
