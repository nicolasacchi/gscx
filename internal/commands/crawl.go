package commands

import (
	"context"
	"fmt"
	"math"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nicolasacchi/gscx/internal/config"
	"github.com/nicolasacchi/gscx/internal/datadog"
	"github.com/nicolasacchi/gscx/internal/gsc"
	"github.com/nicolasacchi/gscx/internal/output"
	"github.com/nicolasacchi/gscx/internal/pagetype"
	"github.com/nicolasacchi/gscx/internal/store"
	"github.com/spf13/cobra"
)

const (
	crawlStatusFacet = "@http.status_code"
	// crawlStatusLimit is far above the number of distinct HTTP statuses a
	// page type can see; hitting it is reported as a warning.
	crawlStatusLimit = 50
	crawlNote        = "Counts are requests that reached the origin: edge-cache (CloudFront) hits are not logged, so they understate what Googlebot fetched. " +
		"Datadog keeps these logs only ~7 days; use --daily --save to accumulate history in the local store (see 'gscx crawl history'). " +
		"Types are disjoint (first match wins, like the page-type rules); 'other' is total minus the typed counts."
)

var (
	flagCrawlFrom        string
	flagCrawlTo          string
	flagCrawlUnverified  bool
	flagCrawlByStatus    bool
	flagCrawlDaily       bool
	flagCrawlSave        bool
	flagCrawlConcurrency int
	flagCrawlSince       string

	flagDDAPIKey string
	flagDDAppKey string
	flagDDSite   string
)

// newDatadogClient is a seam so tests can disable the client's rate-limit
// pacing; production always uses datadog.New.
var newDatadogClient = datadog.New

var crawlCmd = &cobra.Command{
	Use:   "crawl",
	Short: "Googlebot requests per page type, counted from Datadog access logs",
	Long: `Counts Googlebot requests per page type from the Datadog access logs
(service/bot/verified queries and per-type dd_query fragments come from the
config). The per-type queries mirror the page-type rules' first-match order, so
the counts are disjoint and 'other' = total - sum of types.

By default only requests from Google's network are counted (the user agent is
trivially spoofed); --unverified drops that filter and the output's 'spoofed'
field shows how many UA-only requests were excluded.

Counts are requests that reached the origin: edge-cache hits are not logged.
Datadog keeps these logs ~7 days. Run 'gscx crawl --from 7d --daily --save'
regularly to accumulate history, then read it with 'gscx crawl history'.

The Datadog logs aggregate API allows 2 requests per 10 seconds per
organisation, so a run takes about a minute (one query per page type plus the
total); --concurrency only overlaps request latency, the client paces itself.

Credentials: --dd-api-key/--dd-app-key/--dd-site > DD_API_KEY/DD_APP_KEY/DD_SITE
> [datadog] in the config file. The application key needs logs_read_data.`,
	Example: `  gscx crawl --from 24h
  gscx crawl --from 7d --by-status
  gscx crawl --from 7d --daily --save
  gscx crawl history --since 30d`,
	Args: cobra.NoArgs,
	RunE: runCrawl,
}

var crawlHistoryCmd = &cobra.Command{
	Use:   "history",
	Short: "Daily Googlebot requests per page type saved by 'crawl --daily --save'",
	Args:  cobra.NoArgs,
	RunE:  runCrawlHistory,
}

func init() {
	f := crawlCmd.Flags()
	f.StringVar(&flagCrawlFrom, "from", "7d", "window start: 24h, 7d, RFC3339 or YYYY-MM-DD (00:00 UTC)")
	f.StringVar(&flagCrawlTo, "to", "", "window end, same formats (default: now)")
	f.BoolVar(&flagCrawlByStatus, "by-status", false, "break each page type down by HTTP status code")
	f.BoolVar(&flagCrawlDaily, "daily", false, "per-UTC-day counts for each page type")
	f.BoolVar(&flagCrawlSave, "save", false, "store complete UTC days in the local crawl_daily table (requires --daily)")
	f.IntVar(&flagCrawlConcurrency, "concurrency", 4, "parallel Datadog queries (the API is rate limited to 2 requests per 10 s)")
	f.StringVar(&flagDDAPIKey, "dd-api-key", "", "Datadog API key (env DD_API_KEY)")
	f.StringVar(&flagDDAppKey, "dd-app-key", "", "Datadog application key with logs_read_data (env DD_APP_KEY)")
	f.StringVar(&flagDDSite, "dd-site", "", "Datadog site, e.g. datadoghq.eu (env DD_SITE)")
	crawlCmd.PersistentFlags().BoolVar(&flagCrawlUnverified, "unverified", false, "count every request with a Googlebot user agent, not only those from Google's network")

	crawlHistoryCmd.Flags().StringVar(&flagCrawlSince, "since", "30d", "first day to show: 30d, RFC3339 or YYYY-MM-DD")

	crawlCmd.AddCommand(crawlHistoryCmd)
	rootCmd.AddCommand(crawlCmd)
}

// ---- command wiring ----

func runCrawl(cmd *cobra.Command, args []string) error {
	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	now := time.Now().UTC().Truncate(time.Second)
	from, to, err := crawlWindow(flagCrawlFrom, flagCrawlTo, now)
	if err != nil {
		return err
	}
	if flagCrawlSave && !flagCrawlDaily {
		return gsc.NewValidationError("--save needs --daily", "crawl_daily stores one row per UTC day")
	}

	apiKey, appKey, site := ddCredentials(cfg.Datadog)
	client, err := newDatadogClient(datadog.Options{
		APIKey: apiKey, AppKey: appKey, Site: site,
		UserAgent: userAgent(),
		Logf:      logf,
	})
	if err != nil {
		return err
	}
	cl, err := classifier()
	if err != nil {
		return err
	}

	// Open the store before the (slow) queries so an unwritable path fails fast.
	var st *store.Store
	if flagCrawlSave {
		if st, err = openStore(ctx); err != nil {
			return err
		}
		defer func() { _ = st.Close() }()
	}

	res, err := collectCrawl(ctx, client, crawlParams{
		Datadog:     cfg.Datadog,
		Rules:       cl.Rules(),
		From:        from,
		To:          to,
		Now:         now,
		Unverified:  flagCrawlUnverified,
		ByStatus:    flagCrawlByStatus,
		Daily:       flagCrawlDaily,
		Concurrency: flagCrawlConcurrency,
	})
	if err != nil {
		return err
	}

	if flagCrawlWithCoverage {
		if err := attachCrawlCoverage(ctx, res, cl, now); err != nil {
			return err
		}
	}

	if st != nil {
		switch {
		case res.Partial:
			res.Warnings = append(res.Warnings, "not saved: Datadog returned partial results, re-run to store this window")
		default:
			saved, err := saveCrawl(ctx, st, res, crawlKey(cfg.Datadog, flagCrawlUnverified), now)
			if err != nil {
				return err
			}
			res.Saved = saved
		}
	}
	return emit(cmd, res)
}

func runCrawlHistory(cmd *cobra.Command, args []string) error {
	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	since, err := crawlParseTime(flagCrawlSince, time.Now().UTC())
	if err != nil {
		return fmt.Errorf("--since: %w", err)
	}
	cl, err := classifier()
	if err != nil {
		return err
	}
	st, err := openStore(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()

	key := crawlKey(cfg.Datadog, flagCrawlUnverified)
	rows, err := st.CrawlRows(ctx, store.CrawlFilter{
		Source: key.Source, Bot: key.Bot, Verified: key.Verified,
		Since: since.UTC().Format("2006-01-02"),
	})
	if err != nil {
		return err
	}
	types, days := store.PivotCrawl(rows, cl.Names())
	// Newest first, so a --limit cap drops the oldest days.
	for i, j := 0, len(days)-1; i < j; i, j = i+1, j-1 {
		days[i], days[j] = days[j], days[i]
	}
	shown, trunc := output.Cap(days, flagLimit)

	out := crawlHistoryOutput{
		Source:   key.Source,
		Bot:      key.Bot,
		Verified: key.Verified,
		Since:    since.UTC().Format("2006-01-02"),
		Types:    types,
		Days:     len(days),
		Rows:     shown,
		Trunc:    trunc,
	}
	if out.Types == nil {
		out.Types = []string{}
	}
	if out.Rows == nil {
		out.Rows = []store.CrawlDay{}
	}
	if len(days) == 0 {
		out.Note = "no saved crawl data for this source; run 'gscx crawl --from 7d --daily --save' (Datadog keeps logs ~7 days)"
	}
	return emit(cmd, out)
}

type crawlHistoryOutput struct {
	Source   string             `json:"source"`
	Bot      string             `json:"bot"`
	Verified bool               `json:"verified"`
	Since    string             `json:"since"`
	Types    []string           `json:"types"`
	Days     int                `json:"days"`
	Rows     []store.CrawlDay   `json:"rows"`
	Trunc    *output.Truncation `json:"truncated,omitempty"`
	Note     string             `json:"note,omitempty"`
}

// crawlStoreKey identifies one crawl_daily series.
type crawlStoreKey struct {
	Source   string
	Bot      string
	Verified bool
}

func crawlKey(d config.Datadog, unverified bool) crawlStoreKey {
	return crawlStoreKey{Source: "datadog:" + d.Query, Bot: d.BotQuery, Verified: !unverified}
}

// ddCredentials resolves Datadog credentials: flag > env > config file.
func ddCredentials(d config.Datadog) (apiKey, appKey, site string) {
	return firstNonEmpty(flagDDAPIKey, os.Getenv("DD_API_KEY"), d.APIKey),
		firstNonEmpty(flagDDAppKey, os.Getenv("DD_APP_KEY"), d.AppKey),
		firstNonEmpty(flagDDSite, os.Getenv("DD_SITE"), d.Site)
}

// ---- time parsing ----

var crawlRelative = regexp.MustCompile(`^(\d+)([smhdw])$`)

// crawlParseTime accepts "now", a relative age (90m, 24h, 7d, 2w: that long
// before now), an RFC3339 timestamp or a YYYY-MM-DD date (00:00 UTC).
func crawlParseTime(s string, now time.Time) (time.Time, error) {
	s = strings.TrimSpace(s)
	if s == "" || s == "now" {
		return now, nil
	}
	if m := crawlRelative.FindStringSubmatch(s); m != nil {
		n, err := strconv.Atoi(m[1])
		if err != nil {
			return time.Time{}, fmt.Errorf("bad duration %q", s)
		}
		unit := map[string]time.Duration{
			"s": time.Second, "m": time.Minute, "h": time.Hour, "d": 24 * time.Hour, "w": 7 * 24 * time.Hour,
		}[m[2]]
		return now.Add(-time.Duration(n) * unit), nil
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t.UTC(), nil
	}
	if t, err := time.Parse("2006-01-02", s); err == nil {
		return t.UTC(), nil
	}
	return time.Time{}, fmt.Errorf("unrecognised time %q (use 24h, 7d, RFC3339 or YYYY-MM-DD)", s)
}

// crawlWindow resolves --from/--to against now and validates the window.
func crawlWindow(fromS, toS string, now time.Time) (from, to time.Time, err error) {
	if from, err = crawlParseTime(fromS, now); err != nil {
		return from, to, fmt.Errorf("--from: %w", err)
	}
	if to, err = crawlParseTime(toS, now); err != nil {
		return from, to, fmt.Errorf("--to: %w", err)
	}
	if !from.Before(to) {
		return from, to, fmt.Errorf("--from (%s) must be before --to (%s)", from.Format(time.RFC3339), to.Format(time.RFC3339))
	}
	return from, to, nil
}

// crawlDays lists the UTC days (YYYY-MM-DD) touched by [from, to).
func crawlDays(from, to time.Time) []string {
	var days []string
	for d := from.UTC().Truncate(24 * time.Hour); d.Before(to); d = d.Add(24 * time.Hour) {
		days = append(days, d.Format("2006-01-02"))
	}
	return days
}

// crawlDayComplete reports whether the whole UTC day lies inside [from, to).
func crawlDayComplete(day string, from, to time.Time) bool {
	start, err := time.Parse("2006-01-02", day)
	if err != nil {
		return false
	}
	return !start.Before(from) && !start.Add(24*time.Hour).After(to)
}

// ---- collection ----

// crawlAggregator is the slice of *datadog.Client the crawl command needs.
type crawlAggregator interface {
	Count(ctx context.Context, query string, from, to time.Time) (int64, datadog.Meta, error)
	CountBy(ctx context.Context, query string, from, to time.Time, facet string, limit int) ([]datadog.Bucket, datadog.Meta, error)
	CountDaily(ctx context.Context, query string, from, to time.Time) (map[string]int64, datadog.Meta, error)
	CountDailyBy(ctx context.Context, query string, from, to time.Time, facet string, limit int) ([]datadog.DailyBucket, datadog.Meta, error)
}

type crawlParams struct {
	Datadog     config.Datadog
	Rules       []pagetype.Rule
	From, To    time.Time
	Now         time.Time // reference for the retention warning
	Unverified  bool
	ByStatus    bool
	Daily       bool
	Concurrency int
}

type crawlWindowOut struct {
	From string `json:"from"`
	To   string `json:"to"`
}

type crawlStatusOut struct {
	Status   string  `json:"status"`
	Requests int64   `json:"requests"`
	Share    float64 `json:"share"` // of this type's requests
}

type crawlTypeOut struct {
	Type     string            `json:"type"`
	Requests int64             `json:"requests"`
	Share    float64           `json:"share"`              // of all requests in the window
	Statuses *[]crawlStatusOut `json:"statuses,omitempty"` // present (possibly empty) under --by-status
	Daily    map[string]int64  `json:"daily,omitempty"`
	// Coverage is the sampled index coverage of the type (--with-coverage).
	Coverage *crawlCoverage `json:"coverage,omitempty"`

	// DailyStatus is status -> day -> requests; only kept for --save.
	dailyStatus map[string]map[string]int64
}

type crawlDayOut struct {
	Day      string `json:"day"`
	Complete bool   `json:"complete"`
}

type crawlSaved struct {
	DB   string   `json:"db"`
	Rows int      `json:"rows"`
	Days []string `json:"days"`
	// Skipped are days not stored: partial days (window edge), days with no
	// logs, and days whose stored snapshot is larger (logs expired since).
	Skipped []crawlSkipped `json:"skipped,omitempty"`
}

type crawlSkipped struct {
	Day    string `json:"day"`
	Reason string `json:"reason"`
}

type crawlResult struct {
	Source    string         `json:"source"`
	Window    crawlWindowOut `json:"window"`
	BaseQuery string         `json:"base_query"`
	Verified  bool           `json:"verified"`
	Total     int64          `json:"total"`
	// Spoofed counts Googlebot-UA requests from outside the verified network.
	// Only present in verified mode.
	Spoofed  *int64         `json:"spoofed,omitempty"`
	ByType   []crawlTypeOut `json:"by_type"`
	Other    int64          `json:"other"`
	Days     []crawlDayOut  `json:"days,omitempty"`
	Saved    *crawlSaved    `json:"saved,omitempty"`
	Warnings []string       `json:"warnings"`
	Partial  bool           `json:"partial,omitempty"`
	Note     string         `json:"note"`
}

// crawlCounts is the raw outcome of one query, whichever mode it ran in.
type crawlCounts struct {
	requests    int64
	statuses    map[string]int64            // --by-status
	daily       map[string]int64            // --daily
	dailyStatus map[string]map[string]int64 // --daily --by-status: status -> day -> n
}

type crawlRunner struct {
	agg  crawlAggregator
	p    crawlParams
	days []string

	mu       sync.Mutex
	warnings []string
	partial  bool
}

func (r *crawlRunner) warn(format string, args ...any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	msg := fmt.Sprintf(format, args...)
	for _, w := range r.warnings {
		if w == msg {
			return
		}
	}
	r.warnings = append(r.warnings, msg)
}

func (r *crawlRunner) noteMeta(label string, m datadog.Meta) {
	if m.Partial() {
		r.mu.Lock()
		r.partial = true
		r.mu.Unlock()
		r.warn("query %q returned status %q: its counts are partial", label, m.Status)
	}
	for _, w := range m.Warnings {
		r.warn("datadog (%s): %s", label, w)
	}
}

// fetch runs one query in the mode selected by --by-status / --daily. Exactly
// one API call is made, so a run costs one call per page type plus the total.
func (r *crawlRunner) fetch(ctx context.Context, label, query string) (crawlCounts, error) {
	from, to := r.p.From, r.p.To
	var c crawlCounts
	switch {
	case !r.p.ByStatus && !r.p.Daily:
		n, meta, err := r.agg.Count(ctx, query, from, to)
		if err != nil {
			return c, err
		}
		r.noteMeta(label, meta)
		c.requests = n

	case r.p.ByStatus && !r.p.Daily:
		buckets, meta, err := r.agg.CountBy(ctx, query, from, to, crawlStatusFacet, crawlStatusLimit)
		if err != nil {
			return c, err
		}
		r.noteMeta(label, meta)
		c.statuses = map[string]int64{}
		for _, b := range buckets {
			c.statuses[b.Value] += b.Count
			c.requests += b.Count
		}
		if len(buckets) >= crawlStatusLimit {
			r.warn("%s: more than %d distinct status values, the tail is missing", label, crawlStatusLimit)
		}

	case !r.p.ByStatus && r.p.Daily:
		days, meta, err := r.agg.CountDaily(ctx, query, from, to)
		if err != nil {
			return c, err
		}
		r.noteMeta(label, meta)
		c.daily = r.fillDays(days)
		for _, n := range c.daily {
			c.requests += n
		}

	default:
		buckets, meta, err := r.agg.CountDailyBy(ctx, query, from, to, crawlStatusFacet, crawlStatusLimit)
		if err != nil {
			return c, err
		}
		r.noteMeta(label, meta)
		c.statuses = map[string]int64{}
		c.dailyStatus = map[string]map[string]int64{}
		daily := map[string]int64{}
		for _, b := range buckets {
			c.dailyStatus[b.Value] = b.Days
			for day, n := range b.Days {
				c.statuses[b.Value] += n
				daily[day] += n
				c.requests += n
			}
		}
		c.daily = r.fillDays(daily)
		if len(buckets) >= crawlStatusLimit {
			r.warn("%s: more than %d distinct status values, the tail is missing", label, crawlStatusLimit)
		}
	}
	return c, nil
}

// fillDays makes every day of the window present (0 when Datadog returned no
// bucket) and drops buckets outside it.
func (r *crawlRunner) fillDays(in map[string]int64) map[string]int64 {
	out := make(map[string]int64, len(r.days))
	for _, d := range r.days {
		out[d] = in[d]
	}
	return out
}

// collectCrawl runs every query and assembles the result. It performs no
// store access.
func collectCrawl(ctx context.Context, agg crawlAggregator, p crawlParams) (*crawlResult, error) {
	d := p.Datadog
	base := datadog.BaseQuery(d.Query, d.BotQuery, d.VerifiedQuery, p.Unverified)
	verified := !p.Unverified && strings.TrimSpace(d.VerifiedQuery) != ""
	queries, uncounted := datadog.TypeQueries(base, p.Rules)

	r := &crawlRunner{agg: agg, p: p}
	if p.Daily {
		r.days = crawlDays(p.From, p.To)
	}
	for _, name := range uncounted {
		r.warn("page type %q has no dd_query: it cannot be counted separately and falls into other (or a later type)", name)
	}
	if age := p.Now.Sub(p.From); !p.Now.IsZero() && age > 8*24*time.Hour {
		r.warn("--from is %d days ago but Datadog keeps these logs ~7 days: the oldest days will be empty or partial", int(age.Hours()/24))
	}

	var (
		total   crawlCounts
		spoofed int64
		typed   = make([]crawlCounts, len(queries))
	)
	jobs := []func(context.Context) error{
		func(ctx context.Context) (err error) {
			total, err = r.fetch(ctx, "total", base)
			logf("crawl: total = %d", total.requests)
			return err
		},
	}
	if verified {
		sq := datadog.SpoofedQuery(d.Query, d.BotQuery, d.VerifiedQuery)
		jobs = append(jobs, func(ctx context.Context) error {
			n, meta, err := agg.Count(ctx, sq, p.From, p.To)
			if err != nil {
				return err
			}
			r.noteMeta("spoofed", meta)
			spoofed = n
			logf("crawl: spoofed = %d", n)
			return nil
		})
	}
	for i, q := range queries {
		jobs = append(jobs, func(ctx context.Context) (err error) {
			typed[i], err = r.fetch(ctx, q.Type, q.Query)
			logf("crawl: %s = %d", q.Type, typed[i].requests)
			return err
		})
	}
	if err := crawlRunBounded(ctx, p.Concurrency, jobs); err != nil {
		return nil, err
	}

	res := &crawlResult{
		Source:    "datadog:" + d.Query,
		Window:    crawlWindowOut{From: p.From.UTC().Format(time.RFC3339), To: p.To.UTC().Format(time.RFC3339)},
		BaseQuery: base,
		Verified:  verified,
		Total:     total.requests,
		Note:      crawlNote,
	}
	if verified {
		res.Spoofed = &spoofed
	}

	// other = total - sum of the typed counts, field by field.
	other := crawlCounts{requests: total.requests}
	if total.statuses != nil {
		other.statuses = cloneCounts(total.statuses)
	}
	if total.daily != nil {
		other.daily = cloneCounts(total.daily)
	}
	if total.dailyStatus != nil {
		other.dailyStatus = map[string]map[string]int64{}
		for s, m := range total.dailyStatus {
			other.dailyStatus[s] = cloneCounts(m)
		}
	}
	for _, c := range typed {
		other.requests -= c.requests
		for s, n := range c.statuses {
			other.statuses[s] -= n
		}
		for day, n := range c.daily {
			other.daily[day] -= n
		}
		for s, m := range c.dailyStatus {
			if other.dailyStatus[s] == nil {
				other.dailyStatus[s] = map[string]int64{}
			}
			for day, n := range m {
				other.dailyStatus[s][day] -= n
			}
		}
	}
	if other.requests < 0 {
		r.warn("typed counts exceed the total by %d: dd_query rules overlap or logs were still being ingested near the window end; 'other' is clamped to 0", -other.requests)
	}
	if clamped := clampCounts(&other); clamped > 0 {
		r.warn("%d per-status/per-day 'other' cells were negative and clamped to 0 (overlapping dd_query rules or late-arriving logs)", clamped)
	}

	share := func(n int64) float64 { return crawlShare(n, total.requests) }
	for i, q := range queries {
		res.ByType = append(res.ByType, crawlTypeOutFrom(q.Type, typed[i], share(typed[i].requests), p))
	}
	res.ByType = append(res.ByType, crawlTypeOutFrom(pagetype.Other, other, share(other.requests), p))
	res.Other = other.requests

	if p.Daily {
		for _, day := range r.days {
			res.Days = append(res.Days, crawlDayOut{Day: day, Complete: crawlDayComplete(day, p.From, p.To)})
		}
		if partialDays := crawlPartialDays(res.Days); len(partialDays) > 0 {
			r.warn("partial UTC days (window edges): %s; --save stores complete days only", strings.Join(partialDays, ", "))
		}
	}

	r.mu.Lock()
	res.Warnings = append([]string{}, r.warnings...)
	res.Partial = r.partial
	r.mu.Unlock()
	return res, nil
}

func crawlPartialDays(days []crawlDayOut) []string {
	var out []string
	for _, d := range days {
		if !d.Complete {
			out = append(out, d.Day)
		}
	}
	return out
}

func crawlTypeOutFrom(name string, c crawlCounts, share float64, p crawlParams) crawlTypeOut {
	out := crawlTypeOut{Type: name, Requests: c.requests, Share: share}
	if p.ByStatus {
		st := crawlStatuses(c.statuses, c.requests)
		out.Statuses = &st
	}
	if p.Daily {
		out.Daily = c.daily
		out.dailyStatus = c.dailyStatus
	}
	return out
}

// crawlStatuses lists non-zero statuses, largest first (ties by status).
func crawlStatuses(m map[string]int64, total int64) []crawlStatusOut {
	out := []crawlStatusOut{}
	for s, n := range m {
		if n > 0 {
			out = append(out, crawlStatusOut{Status: s, Requests: n, Share: crawlShare(n, total)})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Requests != out[j].Requests {
			return out[i].Requests > out[j].Requests
		}
		return out[i].Status < out[j].Status
	})
	return out
}

func crawlShare(n, total int64) float64 {
	if total <= 0 {
		return 0
	}
	return math.Round(float64(n)/float64(total)*10000) / 10000
}

func cloneCounts(m map[string]int64) map[string]int64 {
	out := make(map[string]int64, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// clampCounts raises negative cells to 0 and returns how many it touched.
func clampCounts(c *crawlCounts) int {
	n := 0
	clamp := func(m map[string]int64) {
		for k, v := range m {
			if v < 0 {
				m[k] = 0
				n++
			}
		}
	}
	if c.requests < 0 {
		c.requests = 0
	}
	clamp(c.statuses)
	clamp(c.daily)
	for _, m := range c.dailyStatus {
		clamp(m)
	}
	return n
}

// crawlRunBounded runs jobs with at most n in flight and stops at the first
// error (cancelling the rest).
func crawlRunBounded(ctx context.Context, n int, jobs []func(context.Context) error) error {
	if n < 1 {
		n = 1
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var (
		wg    sync.WaitGroup
		once  sync.Once
		first error
		sem   = make(chan struct{}, n)
	)
	fail := func(err error) {
		once.Do(func() {
			first = err
			cancel()
		})
	}
loop:
	for _, job := range jobs {
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
			break loop
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			if err := job(ctx); err != nil {
				fail(err)
			}
		}()
	}
	wg.Wait()
	if first != nil {
		return first
	}
	return ctx.Err()
}

// ---- persistence ----

// saveCrawl stores the complete days of res in crawl_daily.
func saveCrawl(ctx context.Context, st *store.Store, res *crawlResult, key crawlStoreKey, now time.Time) (*crawlSaved, error) {
	saved := &crawlSaved{DB: st.Path(), Days: []string{}}
	totalByDay := map[string]int64{}
	for _, t := range res.ByType {
		for day, n := range t.Daily {
			totalByDay[day] += n
		}
	}

	var rows []store.CrawlRow
	for _, d := range res.Days {
		switch {
		case !d.Complete:
			saved.Skipped = append(saved.Skipped, crawlSkipped{Day: d.Day, Reason: "partial day (window edge)"})
			continue
		case totalByDay[d.Day] == 0:
			saved.Skipped = append(saved.Skipped, crawlSkipped{Day: d.Day, Reason: "no logs (expired from Datadog retention?)"})
			continue
		}
		for _, t := range res.ByType {
			rows = append(rows, store.CrawlRow{
				Source: key.Source, Day: d.Day, Bot: key.Bot, Verified: key.Verified,
				PageType: t.Type, Status: store.CrawlStatusAll, Requests: t.Daily[d.Day], CapturedAt: now,
			})
			statuses := make([]string, 0, len(t.dailyStatus))
			for s := range t.dailyStatus {
				statuses = append(statuses, s)
			}
			sort.Strings(statuses)
			for _, s := range statuses {
				if n := t.dailyStatus[s][d.Day]; n > 0 {
					rows = append(rows, store.CrawlRow{
						Source: key.Source, Day: d.Day, Bot: key.Bot, Verified: key.Verified,
						PageType: t.Type, Status: s, Requests: n, CapturedAt: now,
					})
				}
			}
		}
	}
	up, err := st.UpsertCrawlDaily(ctx, rows)
	if err != nil {
		return nil, err
	}
	saved.Rows = up.Rows
	saved.Days = append(saved.Days, up.Days...)
	for _, day := range up.Kept {
		saved.Skipped = append(saved.Skipped, crawlSkipped{Day: day, Reason: "stored snapshot is larger (logs have since expired); kept"})
	}
	sort.Slice(saved.Skipped, func(i, j int) bool { return saved.Skipped[i].Day < saved.Skipped[j].Day })
	return saved, nil
}
