package crux

// Ratings of a metric's 75th percentile against Google's Core Web Vitals
// thresholds (https://web.dev/articles/vitals).
const (
	Good             = "good"
	NeedsImprovement = "needs_improvement"
	Poor             = "poor"
)

// Assessments of a set of metrics against the Core Web Vitals pass criterion.
const (
	AssessPass         = "pass"
	AssessFail         = "fail"
	AssessInsufficient = "insufficient_data"
)

// Metric describes one CrUX metric gscx reports.
type Metric struct {
	// Short is the name used in gscx output (lcp, inp, cls, fcp, ttfb).
	Short string
	// API is the CrUX API metric name.
	API string
	// Unit is "ms" or "" (CLS is unitless).
	Unit string
	// Good and Poor are the thresholds: p75 <= Good is good, p75 > Poor is
	// poor, anything between needs improvement.
	Good, Poor float64
	// Core marks the Core Web Vitals (LCP, INP, CLS).
	Core bool
}

// Metrics are the metrics requested by default, in report order.
var Metrics = []Metric{
	{Short: "lcp", API: "largest_contentful_paint", Unit: "ms", Good: 2500, Poor: 4000, Core: true},
	{Short: "inp", API: "interaction_to_next_paint", Unit: "ms", Good: 200, Poor: 500, Core: true},
	{Short: "cls", API: "cumulative_layout_shift", Unit: "", Good: 0.1, Poor: 0.25, Core: true},
	{Short: "fcp", API: "first_contentful_paint", Unit: "ms", Good: 1800, Poor: 3000},
	{Short: "ttfb", API: "experimental_time_to_first_byte", Unit: "ms", Good: 800, Poor: 1800},
}

// APINames lists the CrUX metric names of Metrics.
func APINames() []string {
	out := make([]string, len(Metrics))
	for i, m := range Metrics {
		out[i] = m.API
	}
	return out
}

// ByAPI returns the metric with the given CrUX name.
func ByAPI(api string) (Metric, bool) {
	for _, m := range Metrics {
		if m.API == api {
			return m, true
		}
	}
	return Metric{}, false
}

// Rate classifies a 75th-percentile value.
func (m Metric) Rate(p75 float64) string {
	switch {
	case p75 <= m.Good:
		return Good
	case p75 > m.Poor:
		return Poor
	default:
		return NeedsImprovement
	}
}

// Assess applies the Core Web Vitals criterion to ratings keyed by Metric.Short:
// pass when LCP, INP and CLS are all good; fail as soon as one of them is not;
// otherwise (a core metric has no data and the others are good) there is not
// enough data to say.
func Assess(ratings map[string]string) string {
	complete := true
	for _, m := range Metrics {
		if !m.Core {
			continue
		}
		switch r, ok := ratings[m.Short]; {
		case !ok || r == "":
			complete = false
		case r != Good:
			return AssessFail
		}
	}
	if complete {
		return AssessPass
	}
	return AssessInsufficient
}
