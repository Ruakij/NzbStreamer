package readclient

import (
	"testing"
	"time"
)

func TestPercentileDur(t *testing.T) {
	xs := []time.Duration{
		10 * time.Millisecond, 20 * time.Millisecond, 30 * time.Millisecond,
		40 * time.Millisecond, 50 * time.Millisecond,
	}
	if got := PercentileDur(xs, 0); got != 10*time.Millisecond {
		t.Fatalf("p0 = %v, want 10ms", got)
	}
	if got := PercentileDur(xs, 50); got != 30*time.Millisecond {
		t.Fatalf("p50 = %v, want 30ms", got)
	}
	if got := PercentileDur(xs, 100); got != 50*time.Millisecond {
		t.Fatalf("p100 = %v, want 50ms", got)
	}
	// Interpolated between the two neighbouring samples.
	if got := PercentileDur(xs, 95); got != 48*time.Millisecond {
		t.Fatalf("p95 = %v, want 48ms", got)
	}
	if got := PercentileDur(xs[:1], 50); got != 10*time.Millisecond {
		t.Fatalf("single-element p50 = %v, want 10ms", got)
	}
	if got := PercentileDur(nil, 50); got != 0 {
		t.Fatalf("empty p50 = %v, want 0", got)
	}
}
