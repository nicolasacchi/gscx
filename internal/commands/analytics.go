package commands

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/nicolasacchi/gscx/internal/gsc"
	"github.com/nicolasacchi/gscx/internal/gscdate"
	"github.com/nicolasacchi/gscx/internal/output"
	"github.com/spf13/cobra"
	searchconsole "google.golang.org/api/searchconsole/v1"
)

var analyticsCmd = &cobra.Command{
	Use:   "analytics",
	Short: "Search Analytics (clicks, impressions, CTR, position)",
	Long: `Search Analytics queries. Dates are Pacific-time calendar days and the newest
~3 days are incomplete, so the default end date is Pacific today minus 3 days.`,
}

// analyticsQueryOpts holds the raw flag values of 'analytics query'.
type analyticsQueryOpts struct {
	StartDate, EndDate string
	Dimensions         []string
	Filters            []string
	Type               string
	DataState          string
	AggregationType    string
	RowLimit           int
	StartRow           int
	All                bool
	Classify           bool
}

var aqOpts analyticsQueryOpts

var analyticsQueryCmd = &cobra.Command{
	Use:   "query",
	Short: "Run a Search Analytics query",
	Long: `Runs a Search Analytics query and prints rows keyed by dimension name plus
clicks, impressions, ctr (fraction) and position.

Filters are DIM:OP:EXPR, repeatable and ANDed. DIM is query, page, country,
device or searchAppearance; OP is equals, notEquals, contains, notContains,
includingRegex or excludingRegex. EXPR may contain colons.

--all pages through every row (25,000 per call) and ignores --row-limit; --limit
only trims the printed rows (row_count is always the full count).`,
	Example: `  gscx analytics query --dimensions page --row-limit 10 --classify
  gscx analytics query --dimensions query --filter 'page:includingRegex:/blog/'
  gscx analytics query --dimensions date --start-date 2026-09-01 --end-date 2026-09-27`,
	Args: cobra.NoArgs,
	RunE: runAnalyticsQuery,
}

// analyticsRequestEcho is the effective request, echoed in the output.
type analyticsRequestEcho struct {
	StartDate       string   `json:"start_date"`
	EndDate         string   `json:"end_date"`
	Dimensions      []string `json:"dimensions"`
	Filters         []string `json:"filters"`
	Type            string   `json:"type,omitempty"`
	DataState       string   `json:"data_state,omitempty"`
	AggregationType string   `json:"aggregation_type,omitempty"`
	RowLimit        int64    `json:"row_limit"`
	StartRow        int64    `json:"start_row"`
	All             bool     `json:"all,omitempty"`
}

type analyticsQueryResult struct {
	Site                    string               `json:"site"`
	Request                 analyticsRequestEcho `json:"request"`
	RowCount                int                  `json:"row_count"`
	Rows                    []map[string]any     `json:"rows"`
	ResponseAggregationType string               `json:"response_aggregation_type,omitempty"`
	Truncated               *output.Truncation   `json:"truncated,omitempty"`
}

func runAnalyticsQuery(cmd *cobra.Command, _ []string) error {
	req, echo, err := buildAnalyticsRequest(aqOpts, gscdate.DefaultEnd())
	if err != nil {
		return err
	}
	if aqOpts.Classify && !hasDimension(echo.Dimensions, "page") {
		return gsc.NewValidationError("--classify needs the page dimension", "add --dimensions page")
	}
	var classify func(string) string
	if aqOpts.Classify {
		c, err := classifier()
		if err != nil {
			return err
		}
		classify = c.Classify
	}

	ctx := cmd.Context()
	svc, err := newGSC(ctx)
	if err != nil {
		return err
	}
	site := siteURL()
	rows, aggType, err := gsc.QueryAll(ctx, svc, site, req, aqOpts.All, func(n int) {
		logf("analytics query: %d rows fetched", n)
	})
	if err != nil {
		return err
	}

	shown, trunc := output.Cap(rows, flagLimit)
	out := make([]map[string]any, 0, len(shown))
	for _, r := range shown {
		out = append(out, gsc.RowToMap(echo.Dimensions, r))
	}
	if classify != nil {
		addPageTypes(out, classify)
	}
	return emit(cmd, analyticsQueryResult{
		Site:                    site,
		Request:                 echo,
		RowCount:                len(rows),
		Rows:                    out,
		ResponseAggregationType: aggType,
		Truncated:               trunc,
	})
}

// addPageTypes adds page_type to every row that has a page key.
func addPageTypes(rows []map[string]any, classify func(string) string) {
	for _, r := range rows {
		if p, ok := r["page"].(string); ok {
			r["page_type"] = classify(p)
		}
	}
}

// buildAnalyticsRequest validates the flags and builds the API request plus
// the echo block. defaultEnd is the end date used when --end-date is absent.
func buildAnalyticsRequest(o analyticsQueryOpts, defaultEnd time.Time) (*searchconsole.SearchAnalyticsQueryRequest, analyticsRequestEcho, error) {
	var echo analyticsRequestEcho
	start, end, err := analyticsDateRange(o.StartDate, o.EndDate, 7, defaultEnd)
	if err != nil {
		return nil, echo, err
	}
	dims, err := gsc.ParseDimensions(o.Dimensions)
	if err != nil {
		return nil, echo, err
	}
	filters, err := gsc.ParseFilters(o.Filters)
	if err != nil {
		return nil, echo, err
	}
	req := &searchconsole.SearchAnalyticsQueryRequest{
		StartDate:             start,
		EndDate:               end,
		Dimensions:            dims,
		DimensionFilterGroups: gsc.FilterGroups(filters),
		RowLimit:              int64(o.RowLimit),
		StartRow:              int64(o.StartRow),
	}
	if req.Type, err = analyticsEnumFlag("--type", o.Type, gsc.SearchTypes); err != nil {
		return nil, echo, err
	}
	if req.DataState, err = analyticsEnumFlag("--data-state", o.DataState, gsc.DataStates); err != nil {
		return nil, echo, err
	}
	if req.AggregationType, err = analyticsEnumFlag("--aggregation-type", o.AggregationType, gsc.AggregationTypes); err != nil {
		return nil, echo, err
	}
	if o.RowLimit < 1 || o.RowLimit > gsc.MaxRowLimit {
		return nil, echo, gsc.NewValidationError(
			fmt.Sprintf("--row-limit %d out of range", o.RowLimit),
			fmt.Sprintf("must be 1..%d; use --all to fetch everything", gsc.MaxRowLimit))
	}
	if o.StartRow < 0 {
		return nil, echo, gsc.NewValidationError("--start-row must be >= 0", "")
	}
	if err := gsc.ValidateRequest(req); err != nil {
		return nil, echo, err
	}

	echo = analyticsRequestEcho{
		StartDate:       start,
		EndDate:         end,
		Dimensions:      dimsOrEmpty(dims),
		Filters:         formatFilters(filters),
		Type:            req.Type,
		DataState:       req.DataState,
		AggregationType: req.AggregationType,
		RowLimit:        req.RowLimit,
		StartRow:        req.StartRow,
		All:             o.All,
	}
	if o.All {
		echo.RowLimit = gsc.MaxRowLimit
	}
	return req, echo, nil
}

// analyticsDateRange fills in missing dates: end defaults to defaultEnd and
// start to spanDays-1 days before end.
func analyticsDateRange(startFlag, endFlag string, spanDays int, defaultEnd time.Time) (string, string, error) {
	end := defaultEnd
	if endFlag != "" {
		e, err := gscdate.Parse(endFlag)
		if err != nil {
			return "", "", gsc.NewValidationError("--end-date: "+err.Error(), "")
		}
		end = e
	}
	start := gscdate.AddDays(end, -(spanDays - 1))
	if startFlag != "" {
		s, err := gscdate.Parse(startFlag)
		if err != nil {
			return "", "", gsc.NewValidationError("--start-date: "+err.Error(), "")
		}
		start = s
	}
	if start.After(end) {
		return "", "", gsc.NewValidationError(
			fmt.Sprintf("start date %s is after end date %s", gscdate.Format(start), gscdate.Format(end)), "")
	}
	return gscdate.Format(start), gscdate.Format(end), nil
}

// choice canonicalizes an optional enum flag; empty stays empty (omitted).
func analyticsEnumFlag(flag, v string, choices []string) (string, error) {
	if v == "" {
		return "", nil
	}
	c, ok := gsc.Canonical(v, choices)
	if !ok {
		return "", gsc.NewValidationError(
			fmt.Sprintf("%s: unknown value %q", flag, v), "allowed: "+strings.Join(choices, ", "))
	}
	return c, nil
}

func formatFilters(fs []*searchconsole.ApiDimensionFilter) []string {
	out := make([]string, 0, len(fs))
	for _, f := range fs {
		out = append(out, gsc.FormatFilter(f))
	}
	return out
}

func dimsOrEmpty(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

func hasDimension(list []string, v string) bool {
	for _, s := range list {
		if s == v {
			return true
		}
	}
	return false
}

// --- quick-wins -----------------------------------------------------------

// quickWinThresholds are the selection criteria of 'analytics quick-wins'.
type quickWinThresholds struct {
	MinPosition    float64 `json:"min_position"`
	MaxPosition    float64 `json:"max_position"`
	MaxCTR         float64 `json:"max_ctr"`
	MinImpressions float64 `json:"min_impressions"`
}

type quickWinsOpts struct {
	StartDate, EndDate string
	Filters            []string
	ByPage             bool
	T                  quickWinThresholds
}

var qwOpts = quickWinsOpts{}

var analyticsQuickWinsCmd = &cobra.Command{
	Use:   "quick-wins",
	Short: "Queries ranking just off page one with a low CTR",
	Long: `Finds queries that already rank around positions 4-10 but get a low click
through rate: the cheapest organic gains (better title/meta, internal links).
Fetches every row for the period, filters client-side and sorts by impressions.

By default each row is a query (position averaged over all its pages); with
--by-page rows are query+page pairs, which shows which page to improve.`,
	Example: `  gscx analytics quick-wins --limit 20
  gscx analytics quick-wins --by-page --filter 'page:includingRegex:\.html$' --min-impressions 200`,
	Args: cobra.NoArgs,
	RunE: runQuickWins,
}

type quickWinsResult struct {
	Site       string               `json:"site"`
	Request    analyticsRequestEcho `json:"request"`
	Thresholds quickWinThresholds   `json:"thresholds"`
	Scanned    int                  `json:"scanned_rows"`
	Matches    int                  `json:"matches"`
	Rows       []map[string]any     `json:"rows"`
	Truncated  *output.Truncation   `json:"truncated,omitempty"`
}

func runQuickWins(cmd *cobra.Command, _ []string) error {
	start, end, err := analyticsDateRange(qwOpts.StartDate, qwOpts.EndDate, 28, gscdate.DefaultEnd())
	if err != nil {
		return err
	}
	if err := validateQuickWinThresholds(qwOpts.T); err != nil {
		return err
	}
	dims := []string{"query"}
	if qwOpts.ByPage {
		dims = []string{"query", "page"}
	}
	filters, err := gsc.ParseFilters(qwOpts.Filters)
	if err != nil {
		return err
	}
	req := &searchconsole.SearchAnalyticsQueryRequest{
		StartDate:             start,
		EndDate:               end,
		Dimensions:            dims,
		DimensionFilterGroups: gsc.FilterGroups(filters),
	}

	ctx := cmd.Context()
	svc, err := newGSC(ctx)
	if err != nil {
		return err
	}
	site := siteURL()
	rows, _, err := gsc.QueryAll(ctx, svc, site, req, true, func(n int) {
		logf("quick-wins: %d rows fetched", n)
	})
	if err != nil {
		return err
	}

	matches := filterQuickWins(rows, qwOpts.T)
	shown, trunc := output.Cap(matches, flagLimit)
	out := make([]map[string]any, 0, len(shown))
	for _, r := range shown {
		out = append(out, gsc.RowToMap(dims, r))
	}
	return emit(cmd, quickWinsResult{
		Site: site,
		Request: analyticsRequestEcho{
			StartDate:  start,
			EndDate:    end,
			Dimensions: dims,
			Filters:    formatFilters(filters),
			RowLimit:   gsc.MaxRowLimit,
			All:        true,
		},
		Thresholds: qwOpts.T,
		Scanned:    len(rows),
		Matches:    len(matches),
		Rows:       out,
		Truncated:  trunc,
	})
}

func validateQuickWinThresholds(t quickWinThresholds) error {
	switch {
	case t.MinPosition > t.MaxPosition:
		return gsc.NewValidationError(
			fmt.Sprintf("--min-position %g is above --max-position %g", t.MinPosition, t.MaxPosition), "")
	case t.MaxCTR <= 0 || t.MaxCTR > 1:
		return gsc.NewValidationError(
			fmt.Sprintf("--max-ctr %g out of range", t.MaxCTR), "ctr is a fraction: 0.02 means 2%")
	case t.MinImpressions < 0:
		return gsc.NewValidationError("--min-impressions must be >= 0", "")
	}
	return nil
}

// filterQuickWins keeps rows with MinPosition <= position <= MaxPosition,
// ctr <= MaxCTR and impressions >= MinImpressions, sorted by impressions
// descending (ties: clicks descending, then keys ascending, for stable output).
func filterQuickWins(rows []*searchconsole.ApiDataRow, t quickWinThresholds) []*searchconsole.ApiDataRow {
	var out []*searchconsole.ApiDataRow
	for _, r := range rows {
		if r.Position < t.MinPosition || r.Position > t.MaxPosition {
			continue
		}
		if r.Ctr > t.MaxCTR || r.Impressions < t.MinImpressions {
			continue
		}
		out = append(out, r)
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Impressions != b.Impressions {
			return a.Impressions > b.Impressions
		}
		if a.Clicks != b.Clicks {
			return a.Clicks > b.Clicks
		}
		return strings.Join(a.Keys, "\x00") < strings.Join(b.Keys, "\x00")
	})
	return out
}

func init() {
	f := analyticsQueryCmd.Flags()
	f.StringVar(&aqOpts.StartDate, "start-date", "", "first day, YYYY-MM-DD Pacific (default: end date - 6 days)")
	f.StringVar(&aqOpts.EndDate, "end-date", "", "last day, YYYY-MM-DD Pacific (default: Pacific today - 3 days)")
	f.StringSliceVar(&aqOpts.Dimensions, "dimensions", nil, "group by: query,page,device,country,date,searchAppearance,hour (comma list or repeat)")
	f.StringArrayVar(&aqOpts.Filters, "filter", nil, "DIM:OP:EXPR, repeatable, ANDed (e.g. page:includingRegex:/blog/)")
	f.StringVar(&aqOpts.Type, "type", "", "search type: web|image|video|news|discover|googleNews (default web)")
	f.StringVar(&aqOpts.DataState, "data-state", "", "final|all|hourly_all (all includes fresh partial data; hour needs hourly_all)")
	f.StringVar(&aqOpts.AggregationType, "aggregation-type", "", "auto|byPage|byProperty|byNewsShowcasePanel")
	f.IntVar(&aqOpts.RowLimit, "row-limit", 1000, "rows per request, 1-25000 (ignored with --all)")
	f.IntVar(&aqOpts.StartRow, "start-row", 0, "zero-based first row")
	f.BoolVar(&aqOpts.All, "all", false, "paginate through every row (25,000 per call)")
	f.BoolVar(&aqOpts.Classify, "classify", false, "add page_type to each row (requires the page dimension)")

	q := analyticsQuickWinsCmd.Flags()
	q.StringVar(&qwOpts.StartDate, "start-date", "", "first day, YYYY-MM-DD Pacific (default: end date - 27 days)")
	q.StringVar(&qwOpts.EndDate, "end-date", "", "last day, YYYY-MM-DD Pacific (default: Pacific today - 3 days)")
	q.StringArrayVar(&qwOpts.Filters, "filter", nil, "DIM:OP:EXPR, repeatable, ANDed")
	q.BoolVar(&qwOpts.ByPage, "by-page", false, "rows are query+page pairs instead of queries")
	q.Float64Var(&qwOpts.T.MinPosition, "min-position", 4, "minimum average position")
	q.Float64Var(&qwOpts.T.MaxPosition, "max-position", 10, "maximum average position")
	q.Float64Var(&qwOpts.T.MaxCTR, "max-ctr", 0.02, "maximum CTR as a fraction (0.02 = 2%)")
	q.Float64Var(&qwOpts.T.MinImpressions, "min-impressions", 50, "minimum impressions in the period")

	analyticsCmd.AddCommand(analyticsQueryCmd, analyticsQuickWinsCmd)
	rootCmd.AddCommand(analyticsCmd)
}
