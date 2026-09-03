package filehealth

import (
	"math"
	"testing"
)

func TestZScore(t *testing.T) {
	if z := zScore(0.95); math.Abs(z-1.96) > 0.001 {
		t.Errorf("zScore(0.95) = %v, want ~1.96", z)
	}
}

func TestWilsonHandlesTheEnds(t *testing.T) {
	low, high := wilson(0, 8, 0.95)
	if low > 1e-9 || high <= 0 || high >= 1 {
		t.Errorf("wilson(0, 8) = [%v, %v], want [0, something well under 1]", low, high)
	}

	low, high = wilson(8, 8, 0.95)
	if high < 1-1e-9 || low <= 0 || low >= 1 {
		t.Errorf("wilson(8, 8) = [%v, %v], want [something well over 0, 1]", low, high)
	}
}

func TestDecide(t *testing.T) {
	cases := []struct {
		missing, samples int
		limit            float64
		want             verdict
	}{
		{0, 2, 0, verdictAccept},
		// The takedown case against an nzb with no par2: any miss is fatal
		{1, 2, 0, verdictDiscard},
		// Both of two missing is already confidently past a 10% limit
		{2, 2, 0.10, verdictDiscard},
		// A single miss in a wide sample, against a limit par2 could carry
		{1, 400, 0.10, verdictDamaged},
		// One in eight: could be 3%, could be 50%
		{1, 8, 0.10, verdictUndecided},
	}

	for _, c := range cases {
		if got := decide(c.missing, c.samples, c.limit, 0.95); got != c.want {
			t.Errorf("decide(%d, %d, %v) = %v, want %v", c.missing, c.samples, c.limit, got, c.want)
		}
	}
}

func TestRequiredSamplesGrowsNearTheLimit(t *testing.T) {
	near := requiredSamples(1, 8, 0.10, 0.95)
	far := requiredSamples(1, 8, 0.50, 0.95)
	if near <= far {
		t.Errorf("resolving a distance of 0.025 took %d samples, 0.375 took %d; want more for the closer one", near, far)
	}
	if got := requiredSamples(1, 8, 0.125, 0.95); got != math.MaxInt32 {
		t.Errorf("an estimate sitting on the limit asked for %d samples, want the cap", got)
	}
}
