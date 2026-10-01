package stats

import (
	"math"
	"testing"
)

func near(a, b, eps float64) bool { return math.Abs(a-b) <= eps }

func TestWilson(t *testing.T) {
	cases := []struct {
		name         string
		s, n         int
		z            float64
		wantLo, want float64
	}{
		{"40/40", 40, 40, Z95, 0.9124, 1},
		{"30/30", 30, 30, Z95, 0.8865, 1},
		{"100/100", 100, 100, Z95, 0.9630, 1},
		{"0/40 mirrors 40/40", 0, 40, Z95, 0, 0.0876},
		{"5/10", 5, 10, Z95, 0.2366, 0.7634},
		{"0/0 is NaN-safe", 0, 0, Z95, 0, 0},
		{"negative n is NaN-safe", 3, -1, Z95, 0, 0},
		{"successes clamped to n", 12, 10, Z95, 0.7225, 1},
		{"z=0 collapses to the point estimate", 3, 10, 0, 0.3, 0.3},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			lo, hi := Wilson(tc.s, tc.n, tc.z)
			if math.IsNaN(lo) || math.IsNaN(hi) {
				t.Fatalf("NaN: %v %v", lo, hi)
			}
			if !near(lo, tc.wantLo, 0.0006) || !near(hi, tc.want, 0.0006) {
				t.Errorf("Wilson(%d,%d) = (%.4f, %.4f), want (%.4f, %.4f)", tc.s, tc.n, lo, hi, tc.wantLo, tc.want)
			}
			if lo < 0 || hi > 1 || lo > hi {
				t.Errorf("interval out of range: (%v, %v)", lo, hi)
			}
		})
	}
}

func TestPercentile(t *testing.T) {
	cases := []struct {
		name string
		in   []float64
		p    float64
		want float64
	}{
		{"empty", nil, 50, 0},
		{"single", []float64{7}, 90, 7},
		{"median even", []float64{4, 1, 3, 2}, 50, 2.5},
		{"median odd", []float64{5, 1, 3}, 50, 3},
		{"p90 interpolates", []float64{1, 2, 3, 4}, 90, 3.7},
		{"p0 is min", []float64{9, 2, 5}, 0, 2},
		{"p100 is max", []float64{9, 2, 5}, 100, 9},
		{"p clamped", []float64{9, 2, 5}, 250, 9},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			orig := append([]float64(nil), tc.in...)
			if got := Percentile(tc.in, tc.p); !near(got, tc.want, 1e-9) {
				t.Errorf("Percentile = %v, want %v", got, tc.want)
			}
			for i := range orig {
				if tc.in[i] != orig[i] {
					t.Fatalf("input was mutated: %v -> %v", orig, tc.in)
				}
			}
		})
	}
	if got := Median([]float64{3, 1, 2}); got != 2 {
		t.Errorf("Median = %v", got)
	}
	if got := Max([]float64{3, 9, 2}); got != 9 {
		t.Errorf("Max = %v", got)
	}
	if got := Max(nil); got != 0 {
		t.Errorf("Max(nil) = %v", got)
	}
}
