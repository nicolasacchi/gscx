package commands

import (
	"context"
	"fmt"
	"math"
	"sync"
	"time"

	"github.com/nicolasacchi/gscx/internal/gsc"
	"github.com/nicolasacchi/gscx/internal/gscdate"
	"github.com/nicolasacchi/gscx/internal/output"
	"github.com/spf13/cobra"
	searchconsole "google.golang.org/api/searchconsole/v1"
)

// maxTrendWeeks bounds a trend run: Search Console keeps ~16 months of data,
// so older weeks would read as a false collapse to zero.
const maxTrendWeeks = 70

var pagesCmd = &cobra.Command{
	Use:   "pages",
	Short: "Pages with search impressions, over time",
}

type pagesTrendOpts struct {
	Weeks       int
	Start, End  string
	AlertPct    float64
	Filters     []string
	Concurrency int
}

var ptOpts pagesTrendOpts

var pagesTrendCmd = &cobra.Command{
	Use:   "trend",
	Short: "Weekly count of distinct pages with impressions, by page type",
	Long: `Counts, per complete Monday-Sunday week, the distinct pages that received at
least one search impression, split by page type. It is the best available proxy
for "pages Google keeps in the index and shows": the Page indexing report has no
API. A sustained drop (or a week-over-week change above --alert-pct) is the
signal to investigate with 'gscx inspect' / 'gscx coverage'.

--weeks N takes the N latest complete weeks ending on or before Pacific today
minus 3 days. --start/--end instead snap inward to whole weeks (start forward to
a Monday, end back to a Sunday). Each week is one paginated page-dimension query
(~18k pages per week today, a single 25,000-row call).`,
	Example: `  gscx pages trend --weeks 16
  gscx pages trend --start 2026-06-01 --end 2026-09-27 --jq summary`,
	Args: cobra.NoArgs,
	RunE: runPagesTrend,
}

// trendTypeStats is one page type's slice of a week.
type trendTypeStats struct {
	Pages       int   `json:"pages"`
	Clicks      int64 `json:"clicks"`
	Impressions int64 `json:"impressions"`
}

// trendWeek is one week of the trend.
type trendWeek struct {
	WeekStart   string                    `json:"week_start"`
	WeekEnd     string                    `json:"week_end"`
	Pages       int                       `json:"pages"`
	Clicks      int64                     `json:"clicks"`
	Impressions int64                     `json:"impressions"`
	ByType      map[string]trendTypeStats `json:"by_type"`
	// ChangePct is the change of Pages vs the previous week; null for the
	// first week or when the previous week had no pages.
	ChangePct *float64 `json:"change_pct"`
	Alert     bool     `json:"alert"`
}

type trendSummary struct {
	Weeks     int      `json:"weeks"`
	MinPages  int      `json:"min_pages"`
	MaxPages  int      `json:"max_pages"`
	LastPages int      `json:"last_pages"`
	Alerts    []string `json:"alerts"`
}

type pagesTrendRequest struct {
	Start    string   `json:"start"`
	End      string   `json:"end"`
	Weeks    int      `json:"weeks"`
	AlertPct float64  `json:"alert_pct"`
	Filters  []string `json:"filters"`
}

type pagesTrendResult struct {
	Site      string             `json:"site"`
	Request   pagesTrendRequest  `json:"request"`
	Weeks     []trendWeek        `json:"weeks"`
	Summary   trendSummary       `json:"summary"`
	Notes     []string           `json:"notes,omitempty"`
	Truncated *output.Truncation `json:"truncated,omitempty"`
}

func runPagesTrend(cmd *cobra.Command, _ []string) error {
	weeks, notes, err := trendWeeks(ptOpts, cmd.Flags().Changed("weeks"), gscdate.DefaultEnd())
	if err != nil {
		return err
	}
	if ptOpts.AlertPct < 0 {
		return gsc.NewValidationError("--alert-pct must be >= 0", "")
	}
	if ptOpts.Concurrency < 1 || ptOpts.Concurrency > 16 {
		return gsc.NewValidationError("--concurrency must be 1..16", "")
	}
	filters, err := gsc.ParseFilters(ptOpts.Filters)
	if err != nil {
		return err
	}
	c, err := classifier()
	if err != nil {
		return err
	}

	ctx := cmd.Context()
	svc, err := newGSC(ctx)
	if err != nil {
		return err
	}
	site := siteURL()
	fetch := func(ctx context.Context, w gscdate.Week) ([]*searchconsole.ApiDataRow, error) {
		req := &searchconsole.SearchAnalyticsQueryRequest{
			StartDate:             w.StartString(),
			EndDate:               w.EndString(),
			Dimensions:            []string{"page"},
			DimensionFilterGroups: gsc.FilterGroups(filters),
		}
		rows, _, err := gsc.QueryAll(ctx, svc, site, req, true, func(n int) {
			logf("pages trend %s: %d pages fetched", w.StartString(), n)
		})
		return rows, err
	}
	trend, err := collectTrend(ctx, weeks, ptOpts.Concurrency, fetch, c.Classify)
	if err != nil {
		return err
	}
	summary := applyTrend(trend, ptOpts.AlertPct)

	shown, trunc := output.Cap(trend, flagLimit)
	return emit(cmd, pagesTrendResult{
		Site: site,
		Request: pagesTrendRequest{
			Start:    weeks[0].StartString(),
			End:      weeks[len(weeks)-1].EndString(),
			Weeks:    len(weeks),
			AlertPct: ptOpts.AlertPct,
			Filters:  formatFilters(filters),
		},
		Weeks:     shown,
		Summary:   summary,
		Notes:     notes,
		Truncated: trunc,
	})
}

// trendWeeks resolves the weeks to query. weeksSet reports whether --weeks was
// given explicitly (it conflicts with --start). The notes carry caveats worth
// showing next to the numbers.
func trendWeeks(o pagesTrendOpts, weeksSet bool, defaultEnd time.Time) ([]gscdate.Week, []string, error) {
	end := defaultEnd
	if o.End != "" {
		e, err := gscdate.Parse(o.End)
		if err != nil {
			return nil, nil, gsc.NewValidationError("--end: "+err.Error(), "")
		}
		end = e
	}
	var weeks []gscdate.Week
	if o.Start != "" {
		if weeksSet {
			return nil, nil, gsc.NewValidationError("--weeks and --start are mutually exclusive", "")
		}
		s, err := gscdate.Parse(o.Start)
		if err != nil {
			return nil, nil, gsc.NewValidationError("--start: "+err.Error(), "")
		}
		weeks = gscdate.WeeksBetween(s, end)
		if len(weeks) == 0 {
			return nil, nil, gsc.NewValidationError(
				fmt.Sprintf("no complete Monday-Sunday week between %s and %s", gscdate.Format(s), gscdate.Format(end)),
				"widen the range; start snaps forward to a Monday and end back to a Sunday")
		}
	} else {
		if o.Weeks < 1 {
			return nil, nil, gsc.NewValidationError("--weeks must be >= 1", "")
		}
		weeks = gscdate.LastCompleteWeeks(o.Weeks, end)
	}
	if len(weeks) > maxTrendWeeks {
		return nil, nil, gsc.NewValidationError(
			fmt.Sprintf("%d weeks requested, max %d", len(weeks), maxTrendWeeks),
			"Search Console keeps about 16 months of data")
	}
	var notes []string
	if last := weeks[len(weeks)-1].End; last.After(defaultEnd) {
		notes = append(notes, fmt.Sprintf(
			"the last week ends %s, after %s (the newest day with reasonably complete data): its page count may be understated",
			gscdate.Format(last), gscdate.Format(defaultEnd)))
	}
	return weeks, notes, nil
}

// trendFetcher returns every page-dimension row of one week.
type trendFetcher func(ctx context.Context, w gscdate.Week) ([]*searchconsole.ApiDataRow, error)

// collectTrend fetches and aggregates all weeks with bounded concurrency. The
// result is in the order of weeks (ascending when weeks are).
func collectTrend(ctx context.Context, weeks []gscdate.Week, concurrency int, fetch trendFetcher, classify func(string) string) ([]trendWeek, error) {
	out := make([]trendWeek, len(weeks))
	err := forEachBoundedIdx(ctx, len(weeks), concurrency, func(ctx context.Context, i int) error {
		rows, err := fetch(ctx, weeks[i])
		if err != nil {
			return fmt.Errorf("week %s: %w", weeks[i].StartString(), err)
		}
		out[i] = aggregateTrendWeek(weeks[i], rows, classify)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// aggregateTrendWeek counts the distinct pages with impressions in rows (one
// row per page) and splits pages, clicks and impressions by page type.
func aggregateTrendWeek(w gscdate.Week, rows []*searchconsole.ApiDataRow, classify func(string) string) trendWeek {
	tw := trendWeek{WeekStart: w.StartString(), WeekEnd: w.EndString(), ByType: map[string]trendTypeStats{}}
	var clicks, impressions float64
	for _, r := range rows {
		if r.Impressions <= 0 || len(r.Keys) == 0 {
			continue
		}
		t := classify(r.Keys[0])
		s := tw.ByType[t]
		s.Pages++
		s.Clicks += int64(math.Round(r.Clicks))
		s.Impressions += int64(math.Round(r.Impressions))
		tw.ByType[t] = s
		tw.Pages++
		clicks += r.Clicks
		impressions += r.Impressions
	}
	tw.Clicks = int64(math.Round(clicks))
	tw.Impressions = int64(math.Round(impressions))
	return tw
}

// applyTrend fills ChangePct and Alert (page count vs the previous week; weeks
// must be in ascending order) and returns the summary.
func applyTrend(weeks []trendWeek, alertPct float64) trendSummary {
	sum := trendSummary{Weeks: len(weeks), Alerts: []string{}}
	for i := range weeks {
		w := &weeks[i]
		if i == 0 || weeks[i-1].Pages == 0 {
			w.ChangePct = nil
		} else {
			prev := float64(weeks[i-1].Pages)
			pct := math.Round((float64(w.Pages)-prev)/prev*10000) / 100
			w.ChangePct = &pct
		}
		w.Alert = w.ChangePct != nil && math.Abs(*w.ChangePct) > alertPct
		if w.Alert {
			sum.Alerts = append(sum.Alerts, w.WeekStart)
		}
		if i == 0 || w.Pages < sum.MinPages {
			sum.MinPages = w.Pages
		}
		if i == 0 || w.Pages > sum.MaxPages {
			sum.MaxPages = w.Pages
		}
		sum.LastPages = w.Pages
	}
	return sum
}

// forEachBoundedIdx runs fn for i in [0,n) with at most limit calls in flight.
// The first error cancels the context handed to the remaining calls and is
// returned; a cancelled parent context returns its error.
func forEachBoundedIdx(ctx context.Context, n, limit int, fn func(ctx context.Context, i int) error) error {
	if limit < 1 {
		limit = 1
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var (
		wg       sync.WaitGroup
		once     sync.Once
		firstErr error
		sem      = make(chan struct{}, limit)
	)
loop:
	for i := 0; i < n; i++ {
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
			break loop
		}
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			defer func() { <-sem }()
			if err := fn(ctx, i); err != nil {
				once.Do(func() {
					firstErr = err
					cancel()
				})
			}
		}(i)
	}
	wg.Wait()
	if firstErr != nil {
		return firstErr
	}
	return ctx.Err()
}

func init() {
	f := pagesTrendCmd.Flags()
	f.IntVar(&ptOpts.Weeks, "weeks", 16, "number of latest complete Monday-Sunday weeks")
	f.StringVar(&ptOpts.Start, "start", "", "first day (YYYY-MM-DD); snaps forward to a Monday; replaces --weeks")
	f.StringVar(&ptOpts.End, "end", "", "last day (YYYY-MM-DD); snaps back to a Sunday (default: Pacific today - 3 days)")
	f.Float64Var(&ptOpts.AlertPct, "alert-pct", 10, "flag weeks whose page count moved more than this percent vs the previous week")
	f.StringArrayVar(&ptOpts.Filters, "filter", nil, "DIM:OP:EXPR, repeatable, ANDed (e.g. page:includingRegex:/blog/)")
	f.IntVar(&ptOpts.Concurrency, "concurrency", 4, "weeks fetched in parallel (1-16)")

	pagesCmd.AddCommand(pagesTrendCmd)
	rootCmd.AddCommand(pagesCmd)
}
