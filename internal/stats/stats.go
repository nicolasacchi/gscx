// Package stats holds the small statistics gscx needs to report sampled index
// coverage honestly: a binomial confidence interval and order statistics.
package stats

import (
	"math"
	"sort"
)

// Z95 is the normal quantile for a two-sided 95% interval.
const Z95 = 1.96

// Wilson returns the Wilson score interval for a binomial proportion
// successes/n at normal quantile z (use Z95 for 95%). Unlike the Wald
// interval it stays informative at 0% and 100%: 40/40 gives a lower bound of
// about 91%, not "exactly 100%". n <= 0 yields (0, 0).
func Wilson(successes, n int, z float64) (lo, hi float64) {
	if n <= 0 {
		return 0, 0
	}
	if successes < 0 {
		successes = 0
	}
	if successes > n {
		successes = n
	}
	nf := float64(n)
	p := float64(successes) / nf
	z2 := z * z
	denom := 1 + z2/nf
	centre := p + z2/(2*nf)
	margin := z * math.Sqrt(p*(1-p)/nf+z2/(4*nf*nf))
	lo = (centre - margin) / denom
	hi = (centre + margin) / denom
	return math.Max(0, lo), math.Min(1, hi)
}

// Percentile returns the p-th percentile (0..100) of vals using linear
// interpolation between closest ranks. vals is not modified. An empty slice
// yields 0.
func Percentile(vals []float64, p float64) float64 {
	if len(vals) == 0 {
		return 0
	}
	s := append([]float64(nil), vals...)
	sort.Float64s(s)
	if p <= 0 {
		return s[0]
	}
	if p >= 100 {
		return s[len(s)-1]
	}
	rank := p / 100 * float64(len(s)-1)
	i := int(math.Floor(rank))
	frac := rank - float64(i)
	if i+1 >= len(s) {
		return s[i]
	}
	return s[i] + frac*(s[i+1]-s[i])
}

// Median is the 50th percentile.
func Median(vals []float64) float64 { return Percentile(vals, 50) }

// Max returns the largest value (0 for an empty slice).
func Max(vals []float64) float64 {
	if len(vals) == 0 {
		return 0
	}
	m := vals[0]
	for _, v := range vals[1:] {
		m = math.Max(m, v)
	}
	return m
}
