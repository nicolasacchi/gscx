package commands

import (
	"fmt"
	"math"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/nicolasacchi/clicore/cierrors"
	"github.com/nicolasacchi/gscx/internal/output"
	"github.com/nicolasacchi/gscx/internal/pagetype"
	"github.com/nicolasacchi/gscx/internal/sitemap"
	"github.com/nicolasacchi/gscx/internal/stats"
	"github.com/nicolasacchi/gscx/internal/store"
	"github.com/spf13/cobra"
	"github.com/tidwall/gjson"
)

const coverageNote = "Sample-based estimate for the URLs we know (the sitemap sampling frame), " +
	"not the Page indexing report's count of every URL Google knows about: that report has no API. " +
	"indexed = inspection verdict PASS (\"URL is on Google\"); ci95 is the Wilson interval of the sampled " +
	"proportion; each URL counts once, with its latest successful inspection in the window; page types " +
	"are re-derived from the current page_types rules. Totals pool strata that were sampled unevenly, so " +
	"prefer the per-type rows (or weighted_indexed_pct with --frame) over the overall rate."

var (
	flagCovSince    string
	flagCovVS       string
	flagCovFrame    bool
	flagCovSitemaps []string

	flagCovURLsType    string
	flagCovURLsState   string
	flagCovURLsVerdict string
	flagCovURLsSince   string
)

var coverageCmd = &cobra.Command{
	Use:   "coverage",
	Short: "Estimate index coverage per page type from stored URL inspections",
	Long: `The Page indexing report has no API, so gscx estimates coverage from sampled
URL inspections (see 'gscx inspect batch'): for each page type, the share of
sampled URLs whose verdict is PASS ("URL is on Google"), with a 95% Wilson
confidence interval, the coverage states of the rest, canonical mismatches,
robots blocks and how long ago Google last crawled the sample.

Reads the local cache only (no API calls) unless --frame is given, which walks
the sitemaps (network) to scale each type's rate to its number of URLs.
Each URL counts once, with its latest successful inspection in the window.

--since is a duration (30d, 12h, 2w) or a date (2026-09-01); the window ends
now. --vs DATE compares with the same-length window ending at DATE (00:00 UTC)
and adds delta_pct_points.`,
	Args: cobra.NoArgs,
	RunE: runCoverage,
}

var coverageURLsCmd = &cobra.Command{
	Use:   "urls",
	Short: "List the stored URLs behind a coverage number",
	Long: `Drill-down for 'gscx coverage': lists each URL's latest successful inspection
in the window, filtered by page type, coverage state and/or verdict, e.g.

  gscx coverage urls --type product --state "Crawled - currently not indexed"`,
	Args: cobra.NoArgs,
	RunE: runCoverageURLs,
}

func init() {
	f := coverageCmd.Flags()
	f.StringVar(&flagCovSince, "since", "30d", "window start: a duration (30d, 12h, 2w) or a date (2026-09-01)")
	f.StringVar(&flagCovVS, "vs", "", "compare with the same-length window ending at this date (YYYY-MM-DD, 00:00 UTC)")
	f.BoolVar(&flagCovFrame, "frame", false, "walk the sitemaps (network) and scale rates to the number of URLs per type")
	f.StringArrayVar(&flagCovSitemaps, "sitemap", nil, "sitemap URL for --frame (repeatable; default: config sitemaps)")

	u := coverageURLsCmd.Flags()
	u.StringVar(&flagCovURLsType, "type", "", "page type")
	u.StringVar(&flagCovURLsState, "state", "", `coverage state, e.g. "Crawled - currently not indexed"`)
	u.StringVar(&flagCovURLsVerdict, "verdict", "", "verdict: PASS, NEUTRAL, FAIL, …")
	u.StringVar(&flagCovURLsSince, "since", "30d", "window start: a duration (30d) or a date")

	coverageCmd.AddCommand(coverageURLsCmd)
	rootCmd.AddCommand(coverageCmd)
}

// --- time windows -------------------------------------------------------------

var sinceRe = regexp.MustCompile(`^(\d+)([dwh])$`)

// parseSince resolves a --since value relative to now: "30d", "2w", "12h", a Go
// duration ("720h30m"), a date (UTC midnight) or an RFC3339 time.
func parseSince(s string, now time.Time) (time.Time, error) {
	s = strings.TrimSpace(s)
	if m := sinceRe.FindStringSubmatch(s); m != nil {
		n, _ := strconv.Atoi(m[1])
		unit := map[string]time.Duration{"d": 24 * time.Hour, "w": 7 * 24 * time.Hour, "h": time.Hour}[m[2]]
		return now.Add(-time.Duration(n) * unit), nil
	}
	if d, err := time.ParseDuration(s); err == nil && d > 0 {
		return now.Add(-d), nil
	}
	if t, err := parseWhen(s); err == nil {
		return t, nil
	}
	return time.Time{}, &cierrors.APIError{
		Kind:   "validation",
		Detail: fmt.Sprintf("cannot parse %q as a duration or date", s),
		Hint:   "use e.g. 30d, 2w, 12h, 2026-09-01",
	}
}

// parseWhen parses YYYY-MM-DD (UTC midnight) or RFC3339.
func parseWhen(s string) (time.Time, error) {
	if t, err := time.Parse("2006-01-02", s); err == nil {
		return t, nil
	}
	return time.Parse(time.RFC3339, s)
}

type timeWindow struct{ From, To time.Time }

type windowOut struct {
	From string  `json:"from"`
	To   string  `json:"to"`
	Days float64 `json:"days"`
}

func (w timeWindow) out() windowOut {
	return windowOut{
		From: w.From.UTC().Format(time.RFC3339),
		To:   w.To.UTC().Format(time.RFC3339),
		Days: round(w.To.Sub(w.From).Hours()/24, 1),
	}
}

func round(x float64, places int) float64 {
	p := math.Pow10(places)
	return math.Round(x*p) / p
}

// --- coverage computation ------------------------------------------------------

type ci95 struct {
	Lo float64 `json:"lo"`
	Hi float64 `json:"hi"`
}

type ageStats struct {
	Median float64 `json:"median"`
	P90    float64 `json:"p90"`
	Max    float64 `json:"max"`
}

type estimate struct {
	Point int `json:"point"`
	Lo    int `json:"lo"`
	Hi    int `json:"hi"`
}

type coverageRow struct {
	N                 int            `json:"n"`
	Indexed           int            `json:"indexed"`
	IndexedPct        *float64       `json:"indexed_pct,omitempty"`
	CI95              *ci95          `json:"ci95,omitempty"`
	CoverageStates    map[string]int `json:"coverage_states"`
	CanonicalMismatch int            `json:"canonical_mismatch"`
	RobotsBlocked     int            `json:"robots_blocked"`
	LastCrawlAgeDays  *ageStats      `json:"last_crawl_age_days,omitempty"`
	NeverCrawled      int            `json:"never_crawled"`

	// --frame
	FrameURLs          *int      `json:"frame_urls,omitempty"`
	EstimatedIndexed   *estimate `json:"estimated_indexed,omitempty"`
	WeightedIndexedPct *float64  `json:"weighted_indexed_pct,omitempty"`
	UnsampledFrameURLs *int      `json:"unsampled_frame_urls,omitempty"`

	// --vs
	VSN            *int     `json:"vs_n,omitempty"`
	VSIndexedPct   *float64 `json:"vs_indexed_pct,omitempty"`
	DeltaPctPoints *float64 `json:"delta_pct_points,omitempty"`
}

type coverageAcc struct {
	n, indexed, mismatch, robots, never int
	states                              map[string]int
	ages                                []float64
}

func newCoverageAcc() *coverageAcc { return &coverageAcc{states: map[string]int{}} }

// crawlTime parses the API's lastCrawlTime. ok is false when Google never
// fetched the page (absent, unparsable, or an epoch placeholder).
func crawlTime(s string) (time.Time, bool) {
	if s == "" {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil || t.Year() < 2000 {
		return time.Time{}, false
	}
	return t, true
}

func (a *coverageAcc) add(r store.Inspection) {
	a.n++
	if r.Verdict == "PASS" {
		a.indexed++
	}
	a.states[orNone(r.CoverageState)]++
	if canonicalMismatch(r.GoogleCanonical, r.UserCanonical) {
		a.mismatch++
	}
	if r.RobotsTxtState == "DISALLOWED" {
		a.robots++
	}
	if t, ok := crawlTime(r.LastCrawlTime); ok {
		// Age at inspection time, so rows from different days stay comparable.
		a.ages = append(a.ages, math.Max(r.InspectedAt.Sub(t).Hours()/24, 0))
	} else {
		a.never++
	}
}

func (a *coverageAcc) row() *coverageRow {
	row := &coverageRow{
		N: a.n, Indexed: a.indexed, CoverageStates: a.states,
		CanonicalMismatch: a.mismatch, RobotsBlocked: a.robots, NeverCrawled: a.never,
	}
	if a.n > 0 {
		pct := round(100*float64(a.indexed)/float64(a.n), 2)
		lo, hi := stats.Wilson(a.indexed, a.n, stats.Z95)
		row.IndexedPct = &pct
		row.CI95 = &ci95{Lo: round(100*lo, 2), Hi: round(100*hi, 2)}
	}
	if len(a.ages) > 0 {
		row.LastCrawlAgeDays = &ageStats{
			Median: round(stats.Median(a.ages), 1),
			P90:    round(stats.Percentile(a.ages, 90), 1),
			Max:    round(stats.Max(a.ages), 1),
		}
	}
	return row
}

// computeCoverage aggregates inspection rows (one per URL) per page type and
// pooled. Types are re-derived from cls so the strata match the sampling frame
// even if the page_types rules changed since the inspection was stored.
func computeCoverage(rows []store.Inspection, cls *pagetype.Classifier) (*coverageRow, map[string]*coverageRow) {
	all := newCoverageAcc()
	per := map[string]*coverageAcc{}
	for _, r := range rows {
		t := cls.Classify(r.URL)
		if per[t] == nil {
			per[t] = newCoverageAcc()
		}
		per[t].add(r)
		all.add(r)
	}
	by := make(map[string]*coverageRow, len(per))
	for t, a := range per {
		by[t] = a.row()
	}
	return all.row(), by
}

// applyFrame scales each type's sampled rate to its number of URLs in the
// sampling frame: estimated_indexed = rate x frame size, with the Wilson
// bounds scaled the same way. The overall row sums the per-type estimates (its
// bounds are sums of per-type bounds, hence conservative) and reports the
// frame-weighted rate over the types that were sampled.
func applyFrame(overall *coverageRow, by map[string]*coverageRow, frameByType map[string]int) {
	var total, tLo, tHi, frameTotal, sampledFrame, unsampled int
	for t, n := range frameByType {
		frameTotal += n
		row := by[t]
		if row == nil {
			row = newCoverageAcc().row()
			by[t] = row
		}
		fn := n
		row.FrameURLs = &fn
		if row.N == 0 {
			unsampled += n
			continue
		}
		lo, hi := stats.Wilson(row.Indexed, row.N, stats.Z95)
		frac := float64(row.Indexed) / float64(row.N)
		e := &estimate{
			Point: int(math.Round(frac * float64(n))),
			Lo:    int(math.Round(lo * float64(n))),
			Hi:    int(math.Round(hi * float64(n))),
		}
		row.EstimatedIndexed = e
		total, tLo, tHi, sampledFrame = total+e.Point, tLo+e.Lo, tHi+e.Hi, sampledFrame+n
	}
	ft := frameTotal
	overall.FrameURLs = &ft
	overall.EstimatedIndexed = &estimate{Point: total, Lo: tLo, Hi: tHi}
	if sampledFrame > 0 {
		w := round(100*float64(total)/float64(sampledFrame), 2)
		overall.WeightedIndexedPct = &w
	}
	overall.UnsampledFrameURLs = &unsampled
}

// applyVS annotates cur rows with the previous window's n and rate and the
// change in percentage points (only where both windows have data).
func applyVS(cur, prev *coverageRow) {
	if prev == nil {
		return
	}
	n := prev.N
	cur.VSN = &n
	if prev.IndexedPct == nil {
		return
	}
	p := *prev.IndexedPct
	cur.VSIndexedPct = &p
	if cur.IndexedPct != nil {
		d := round(*cur.IndexedPct-p, 2)
		cur.DeltaPctPoints = &d
	}
}

type coverageOut struct {
	Site     string                  `json:"site"`
	Note     string                  `json:"note"`
	Hint     string                  `json:"hint,omitempty"`
	Window   windowOut               `json:"window"`
	VSWindow *windowOut              `json:"vs_window,omitempty"`
	Frame    *frameOut               `json:"frame,omitempty"`
	Overall  *coverageRow            `json:"overall"`
	ByType   map[string]*coverageRow `json:"by_type"`
}

func runCoverage(cmd *cobra.Command, _ []string) error {
	ctx := cmd.Context()
	site := siteURL()
	now := time.Now()

	from, err := parseSince(flagCovSince, now)
	if err != nil {
		return err
	}
	if !from.Before(now) {
		return &cierrors.APIError{Kind: "validation", Detail: "--since must be in the past"}
	}
	cur := timeWindow{From: from, To: now}

	cls, err := classifier()
	if err != nil {
		return err
	}
	st, err := openStore(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()

	rows, err := st.LatestPerURL(ctx, site, cur.From, cur.To)
	if err != nil {
		return err
	}
	overall, by := computeCoverage(rows, cls)
	out := coverageOut{Site: site, Note: coverageNote, Window: cur.out(), Overall: overall, ByType: by}
	if overall.N == 0 {
		out.Hint = "no successful inspections stored in this window; run 'gscx inspect batch' first"
	}

	if flagCovVS != "" {
		end, err := parseWhen(flagCovVS)
		if err != nil {
			return &cierrors.APIError{Kind: "validation", Detail: fmt.Sprintf("--vs %q: want YYYY-MM-DD", flagCovVS)}
		}
		prev := timeWindow{From: end.Add(-now.Sub(from)), To: end}
		prevRows, err := st.LatestPerURL(ctx, site, prev.From, prev.To)
		if err != nil {
			return err
		}
		pOverall, pBy := computeCoverage(prevRows, cls)
		applyVS(overall, pOverall)
		for t, row := range by {
			applyVS(row, pBy[t])
		}
		w := prev.out()
		out.VSWindow = &w
	}

	if flagCovFrame {
		sitemaps := flagCovSitemaps
		if len(sitemaps) == 0 {
			sitemaps = cfg.Sitemaps
		}
		fr, err := loadFrame(ctx, site, cls, sitemap.NewClient(userAgent()), sitemaps, "", nil)
		if err != nil {
			return err
		}
		fo := fr.out()
		out.Frame = &fo
		applyFrame(overall, by, fo.ByType)
	}
	return emit(cmd, out)
}

// --- coverage urls --------------------------------------------------------------

type coverageURLRow struct {
	URL               string `json:"url"`
	PageType          string `json:"page_type"`
	Verdict           string `json:"verdict"`
	CoverageState     string `json:"coverage_state"`
	IndexingState     string `json:"indexing_state"`
	RobotsTxtState    string `json:"robots_txt_state"`
	GoogleCanonical   string `json:"google_canonical"`
	UserCanonical     string `json:"user_canonical"`
	CanonicalMismatch bool   `json:"canonical_mismatch"`
	LastCrawlTime     string `json:"last_crawl_time"`
	InspectedAt       string `json:"inspected_at"`
	InspectionLink    string `json:"inspection_link,omitempty"`
}

type coverageURLsOut struct {
	Site    string             `json:"site"`
	Window  windowOut          `json:"window"`
	Filters map[string]string  `json:"filters,omitempty"`
	Total   int                `json:"total"`
	URLs    []coverageURLRow   `json:"urls"`
	Trunc   *output.Truncation `json:"truncated,omitempty"`
}

// filterInspections keeps rows matching the page type, coverage state and
// verdict filters (empty = no filter; state and verdict compare
// case-insensitively). Page types are re-derived from cls.
func filterInspections(rows []store.Inspection, cls *pagetype.Classifier, typ, state, verdict string) []store.Inspection {
	var out []store.Inspection
	for _, r := range rows {
		if typ != "" && cls.Classify(r.URL) != typ {
			continue
		}
		if state != "" && !strings.EqualFold(r.CoverageState, state) {
			continue
		}
		if verdict != "" && !strings.EqualFold(r.Verdict, verdict) {
			continue
		}
		out = append(out, r)
	}
	return out
}

func runCoverageURLs(cmd *cobra.Command, _ []string) error {
	ctx := cmd.Context()
	site := siteURL()
	now := time.Now()

	from, err := parseSince(flagCovURLsSince, now)
	if err != nil {
		return err
	}
	cls, err := classifier()
	if err != nil {
		return err
	}
	if flagCovURLsType != "" && !slices.Contains(cls.Names(), flagCovURLsType) {
		return &cierrors.APIError{Kind: "validation",
			Detail: fmt.Sprintf("unknown page type %q", flagCovURLsType),
			Hint:   "known types: " + strings.Join(cls.Names(), ", ")}
	}
	st, err := openStore(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()

	all, err := st.LatestPerURL(ctx, site, from, now)
	if err != nil {
		return err
	}
	matched := filterInspections(all, cls, flagCovURLsType, flagCovURLsState, flagCovURLsVerdict)
	capped, trunc := output.Cap(matched, flagLimit)

	rows := make([]coverageURLRow, 0, len(capped))
	for _, r := range capped {
		raw, err := st.RawJSON(ctx, r.ID)
		if err != nil {
			return err
		}
		rows = append(rows, coverageURLRow{
			URL: r.URL, PageType: cls.Classify(r.URL),
			Verdict: r.Verdict, CoverageState: r.CoverageState, IndexingState: r.IndexingState,
			RobotsTxtState: r.RobotsTxtState, GoogleCanonical: r.GoogleCanonical, UserCanonical: r.UserCanonical,
			CanonicalMismatch: canonicalMismatch(r.GoogleCanonical, r.UserCanonical),
			LastCrawlTime:     r.LastCrawlTime,
			InspectedAt:       r.InspectedAt.UTC().Format(time.RFC3339),
			InspectionLink:    gjson.Get(raw, "inspectionResultLink").String(),
		})
	}

	filters := map[string]string{}
	for k, v := range map[string]string{"type": flagCovURLsType, "state": flagCovURLsState, "verdict": flagCovURLsVerdict} {
		if v != "" {
			filters[k] = v
		}
	}
	return emit(cmd, coverageURLsOut{
		Site: site, Window: timeWindow{From: from, To: now}.out(),
		Filters: filters, Total: len(matched), URLs: rows, Trunc: trunc,
	})
}
