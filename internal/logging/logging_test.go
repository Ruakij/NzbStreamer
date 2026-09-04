package logging_test

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	"git.ruekov.eu/ruakij/nzbStreamer/internal/logging"
)

// The module is the package of whoever logged, however it got to slog: the
// top-level functions, and a logger carrying attributes of its own.
func TestALineSaysWhichPackageItCameFrom(t *testing.T) {
	var out bytes.Buffer
	logger := slog.New(logging.New(&out, slog.LevelDebug))

	logger.Info("from the test")
	logger.With("nzb", "Some.Release").Warn("from a logger with attributes")

	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("logged %q", out.String())
	}
	// The date and time the log package writes are in front of all of it
	if !strings.HasSuffix(lines[0], "INFO [logging_test] from the test") {
		t.Errorf("the module goes in front of the message, got %q", lines[0])
	}
	if !strings.HasSuffix(lines[1], "WARN [logging_test] from a logger with attributes nzb=Some.Release") {
		t.Errorf("a loggers own attributes stay behind the message, got %q", lines[1])
	}
}
