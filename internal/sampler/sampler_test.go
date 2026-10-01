package sampler

import (
	"fmt"
	"reflect"
	"slices"
	"sort"
	"testing"
)

func urls(prefix string, n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf("https://x.test/%s/%03d", prefix, i)
	}
	return out
}

func groups() map[string][]string {
	return map[string][]string{
		"product":  urls("p", 50),
		"category": urls("c", 30),
		"brand":    urls("b", 20),
	}
}

func TestPerTypeTargetsAndNoDuplicates(t *testing.T) {
	r := Sample(groups(), 5, nil, -1, 1)
	for _, typ := range []string{"product", "category", "brand"} {
		if got := len(r.ByType[typ]); got != 5 {
			t.Errorf("%s: %d picks, want 5", typ, got)
		}
	}
	if r.Total() != 15 {
		t.Errorf("Total = %d, want 15", r.Total())
	}
	seen := map[string]bool{}
	for _, it := range r.Order {
		if seen[it.URL] {
			t.Errorf("duplicate pick %s", it.URL)
		}
		seen[it.URL] = true
	}
}

func TestTargetLargerThanStratum(t *testing.T) {
	g := map[string][]string{"small": urls("s", 3), "big": urls("b", 40)}
	r := Sample(g, 10, nil, -1, 1)
	if len(r.ByType["small"]) != 3 || len(r.ByType["big"]) != 10 {
		t.Errorf("by type = %d/%d, want 3/10", len(r.ByType["small"]), len(r.ByType["big"]))
	}
}

func TestDuplicatesWithinGroupAreCollapsed(t *testing.T) {
	g := map[string][]string{"a": {"u1", "u1", "u2", "u2", "u2"}}
	r := Sample(g, 10, nil, -1, 1)
	if r.Total() != 2 || r.Eligible["a"] != 2 {
		t.Errorf("total=%d eligible=%d, want 2/2", r.Total(), r.Eligible["a"])
	}
}

func TestExclusion(t *testing.T) {
	g := map[string][]string{"a": urls("a", 10)}
	excl := map[string]bool{}
	for _, u := range g["a"][:7] {
		excl[u] = true
	}
	r := Sample(g, 10, excl, -1, 1)
	if r.Total() != 3 || r.Eligible["a"] != 3 {
		t.Fatalf("total=%d eligible=%d, want 3/3", r.Total(), r.Eligible["a"])
	}
	for _, it := range r.Order {
		if excl[it.URL] {
			t.Errorf("picked excluded URL %s", it.URL)
		}
	}
	// A fully excluded stratum yields nothing but is still reported.
	all := map[string]bool{}
	for _, u := range g["a"] {
		all[u] = true
	}
	r = Sample(g, 10, all, -1, 1)
	if r.Total() != 0 || r.Eligible["a"] != 0 {
		t.Errorf("fully excluded: total=%d eligible=%d", r.Total(), r.Eligible["a"])
	}
}

func TestDeterministicForSeed(t *testing.T) {
	a := Sample(groups(), 7, nil, -1, 42)
	b := Sample(groups(), 7, nil, -1, 42)
	if !reflect.DeepEqual(a, b) {
		t.Error("same seed produced different samples")
	}
	c := Sample(groups(), 7, nil, -1, 43)
	if reflect.DeepEqual(a.Order, c.Order) {
		t.Error("different seeds produced identical samples")
	}
}

func TestIndependentOfInputOrder(t *testing.T) {
	g1 := groups()
	g2 := map[string][]string{}
	for k, v := range g1 {
		rev := slices.Clone(v)
		slices.Reverse(rev)
		g2[k] = rev
	}
	a := Sample(g1, 6, nil, -1, 9)
	b := Sample(g2, 6, nil, -1, 9)
	if !reflect.DeepEqual(a.Order, b.Order) {
		t.Error("sample depends on input URL order")
	}
}

func TestStratumPicksIndependentOfOtherStrata(t *testing.T) {
	full := Sample(groups(), 6, nil, -1, 5)
	g := groups()
	delete(g, "category")
	partial := Sample(g, 6, nil, -1, 5)
	for _, typ := range []string{"product", "brand"} {
		if !reflect.DeepEqual(full.ByType[typ], partial.ByType[typ]) {
			t.Errorf("%s picks changed when another stratum was removed", typ)
		}
	}
}

func TestBudgetRoundRobin(t *testing.T) {
	cases := []struct {
		name   string
		budget int
		want   map[int]int // count-per-type value -> number of types having it
	}{
		{"budget below #types: one each for some", 2, map[int]int{1: 2, 0: 1}},
		{"budget == #types: one each", 3, map[int]int{1: 3}},
		{"budget #types+1: one type gets two", 4, map[int]int{2: 1, 1: 2}},
		{"budget 7 of 15", 7, map[int]int{3: 1, 2: 2}},
		{"budget above targets is not exceeded", 100, map[int]int{5: 3}},
		{"zero budget", 0, map[int]int{0: 3}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := Sample(groups(), 5, nil, tc.budget, 11)
			got := map[int]int{}
			for _, typ := range []string{"product", "category", "brand"} {
				got[len(r.ByType[typ])]++
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("distribution = %v, want %v", got, tc.want)
			}
			wantTotal := min(tc.budget, 15)
			if r.Total() != wantTotal {
				t.Errorf("Total = %d, want %d", r.Total(), wantTotal)
			}
		})
	}
}

func TestBudgetPrefixIsRoundRobinOrdered(t *testing.T) {
	full := Sample(groups(), 5, nil, -1, 3)
	cut := Sample(groups(), 5, nil, 6, 3)
	if !reflect.DeepEqual(cut.Order, full.Order[:6]) {
		t.Error("budget-limited sample is not a prefix of the unlimited pick order")
	}
	// The first round visits every stratum once.
	first := []string{full.Order[0].Type, full.Order[1].Type, full.Order[2].Type}
	sort.Strings(first)
	if !reflect.DeepEqual(first, []string{"brand", "category", "product"}) {
		t.Errorf("first round = %v", first)
	}
}

func TestSmallBudgetDoesNotAlwaysFavourSameStratum(t *testing.T) {
	first := map[string]bool{}
	for seed := int64(0); seed < 40; seed++ {
		r := Sample(groups(), 5, nil, 1, seed)
		first[r.Order[0].Type] = true
	}
	if len(first) < 2 {
		t.Errorf("budget=1 always picked %v", first)
	}
}

func TestZeroPerTypeAndEmptyInput(t *testing.T) {
	if r := Sample(groups(), 0, nil, -1, 1); r.Total() != 0 {
		t.Errorf("perType 0: total = %d", r.Total())
	}
	if r := Sample(nil, 5, nil, -1, 1); r.Total() != 0 || r.ByType == nil {
		t.Errorf("nil groups: %+v", r)
	}
}
