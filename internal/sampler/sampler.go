// Package sampler draws a stratified random sample of URLs: a fixed number
// per stratum (page type), minus URLs already covered, capped by a total
// budget that is shared out round-robin so every stratum stays represented.
package sampler

import (
	"hash/fnv"
	"math/rand/v2"
	"slices"
	"sort"
)

// Item is one sampled URL and its stratum.
type Item struct {
	Type string
	URL  string
}

// Result is a drawn sample.
type Result struct {
	// Order lists the sample in pick order, interleaved across strata (one
	// from each type per round). Inspecting in this order keeps every stratum
	// represented even if the run is interrupted.
	Order []Item
	// ByType is the same sample grouped by stratum, in pick order.
	ByType map[string][]string
	// Eligible is the number of candidate URLs per stratum after removing
	// duplicates and excluded URLs.
	Eligible map[string]int
}

// Total is the sample size.
func (r Result) Total() int { return len(r.Order) }

// Sample draws up to perType URLs from each group of groups (stratum name ->
// URLs), skipping URLs in exclude, and at most budget URLs overall (budget < 0
// means no overall limit).
//
// When budget is smaller than the sum of the per-stratum targets, URLs are
// allocated round-robin (every stratum gets one before any gets two), and the
// stratum visited first in each round is itself chosen by seed so a tiny
// budget does not always favour the same stratum.
//
// The result depends only on the set of URLs in each group (not their input
// order), perType, exclude, budget and seed. A stratum's picks do not depend
// on which other strata exist.
func Sample(groups map[string][]string, perType int, exclude map[string]bool, budget int, seed int64) Result {
	res := Result{ByType: map[string][]string{}, Eligible: map[string]int{}}

	names := make([]string, 0, len(groups))
	pools := make(map[string][]string, len(groups))
	for name, urls := range groups {
		pool := eligible(urls, exclude)
		res.Eligible[name] = len(pool)
		pools[name] = pool
		shuffle(pool, seed, name)
		names = append(names, name)
	}
	sort.Strings(names)
	shuffle(names, seed, "\x00strata")

	if perType <= 0 || budget == 0 {
		return res
	}

	taken := make(map[string]int, len(names))
	for progressed := true; progressed; {
		progressed = false
		for _, name := range names {
			if budget >= 0 && len(res.Order) >= budget {
				return res
			}
			i := taken[name]
			if i >= perType || i >= len(pools[name]) {
				continue
			}
			u := pools[name][i]
			taken[name] = i + 1
			res.Order = append(res.Order, Item{Type: name, URL: u})
			res.ByType[name] = append(res.ByType[name], u)
			progressed = true
		}
	}
	return res
}

// eligible returns the distinct, non-excluded URLs sorted lexicographically,
// so the later shuffle is independent of input order.
func eligible(urls []string, exclude map[string]bool) []string {
	out := make([]string, 0, len(urls))
	for _, u := range urls {
		if !exclude[u] {
			out = append(out, u)
		}
	}
	sort.Strings(out)
	return slices.Compact(out)
}

// shuffle permutes s with a generator derived from (seed, salt), so each
// stratum has its own stream.
func shuffle[T any](s []T, seed int64, salt string) {
	h := fnv.New64a()
	_, _ = h.Write([]byte(salt))
	r := rand.New(rand.NewPCG(uint64(seed), h.Sum64()))
	r.Shuffle(len(s), func(i, j int) { s[i], s[j] = s[j], s[i] })
}
