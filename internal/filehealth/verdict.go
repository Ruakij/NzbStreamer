package filehealth

import "math"

// verdict is what a sample says about the file it was taken from.
type verdict int

const (
	// verdictAccept: nothing missing in the sample. It does not prove the file is
	// clean, only that there is no evidence against it.
	verdictAccept verdict = iota
	// verdictDamaged: missing segments, but confidently within what par2 repairs.
	verdictDamaged
	// verdictDiscard: confidently worse than repairable.
	verdictDiscard
	// verdictUndecided: the interval straddles the limit, so the sample cannot
	// say which side the file is on.
	verdictUndecided
)

// wilson returns the score interval for the true missing fraction, given
// missing out of samples observed at the given confidence.
//
// The textbook normal interval degenerates exactly where the sampling lands
// most often - no misses at all, or every probe missing - which is why this one
// is used instead.
func wilson(missing, samples int, confidence float64) (low, high float64) {
	if samples <= 0 {
		return 0, 1
	}

	z := zScore(confidence)
	n, m := float64(samples), float64(missing)

	denominator := n + z*z
	center := (m + z*z/2) / denominator
	halfWidth := z / denominator * math.Sqrt(m*(n-m)/n+z*z/4)

	return math.Max(center-halfWidth, 0), math.Min(center+halfWidth, 1)
}

// decide judges a sample against the limit, the missing fraction still
// considered recoverable.
func decide(missing, samples int, limit, confidence float64) verdict {
	if missing == 0 {
		return verdictAccept
	}

	low, high := wilson(missing, samples, confidence)
	switch {
	case low > limit:
		return verdictDiscard
	case high < limit:
		return verdictDamaged
	}
	return verdictUndecided
}

// requiredSamples is how many probes it takes to resolve the distance between
// the observed missing fraction and the limit, from the normal-approximation
// sample size z^2*p*(1-p)/d^2. An estimate sitting on the limit, or one with no
// spread left to shrink, cannot be resolved by more samples at all; both ask for
// everything the caller is willing to spend, and the cap decides.
func requiredSamples(missing, samples int, limit, confidence float64) int {
	p := float64(missing) / float64(samples)
	d := math.Abs(p - limit)
	spread := p * (1 - p)
	if d <= 0 || spread <= 0 {
		return math.MaxInt32
	}

	z := zScore(confidence)
	return int(math.Ceil(z * z * spread / (d * d)))
}

// zScore is the two-sided normal quantile for a confidence level.
func zScore(confidence float64) float64 {
	if confidence <= 0 || confidence >= 1 {
		return 1.959963985 // 0.95, the default
	}
	return math.Sqrt2 * math.Erfinv(confidence)
}
