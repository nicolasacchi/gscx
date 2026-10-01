package commands

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/nicolasacchi/clicore/cierrors"
	"github.com/nicolasacchi/gscx/internal/config"
	"github.com/nicolasacchi/gscx/internal/crux"
	"github.com/nicolasacchi/gscx/internal/gsc"
	"github.com/nicolasacchi/gscx/internal/output"
	"github.com/nicolasacchi/gscx/internal/pagetype"
	"github.com/nicolasacchi/gscx/internal/quota"
	"github.com/nicolasacchi/gscx/internal/sampler"
	"github.com/nicolasacchi/gscx/internal/sitemap"
	"github.com/nicolasacchi/gscx/internal/stats"
	"github.com/spf13/cobra"
)

const cwvNote = "Real-user field data from the Chrome UX Report (CrUX): Chrome users who opted in to usage statistics, " +
	"a rolling 28-day window refreshed daily. p75 is the 75th percentile of page loads; rating uses Google's " +
	"Core Web Vitals thresholds (see 'thresholds'); density is the share of page loads rated good / " +
	"needs_improvement / poor. cwv_assessment is 'pass' only when LCP, INP and CLS are all good. " +
	"URL-level data exists only for URLs with enough traffic: expect most product URLs to be no_data " +
	"(then the origin figures are the best available)."

var (
	flagCruxKey       string
	flagCruxOrigin    string
	flagCruxForm      []string
	flagCwvURLs       []string
	flagCwvSample     int
	flagCwvTypes      []string
	flagCwvSince      string
	flagCwvSitemaps   []string
	flagCwvFromMaps   bool
	flagCwvConcurrent int
	flagCwvWeeks      int
)

// newCruxClient is a seam so tests can point the client at httptest and turn
// pacing off; production always uses crux.New.
var newCruxClient = crux.New

var cwvCmd = &cobra.Command{
	Use:   "cwv",
	Short: "Core Web Vitals from the Chrome UX Report (CrUX) API",
	Long: `Real-user Core Web Vitals (LCP, INP, CLS, plus FCP and TTFB) from the Chrome UX
Report API, rated against Google's thresholds (LCP 2500/4000 ms, INP 200/500
ms, CLS 0.1/0.25, FCP 1800/3000 ms, TTFB 800/1800 ms).

Default: origin-level p75 for PHONE and DESKTOP with the good / needs
improvement / poor densities.

--urls a,b or --sample N queries URL-level records instead (all form factors by
default: URL data is sparse). --sample picks N URLs per page type from the
stored inspections (or, with --from-sitemaps, from the sitemaps) and reports
how many have data: most product URLs have none ("no_data", not an error).

API key: --crux-key > GSCX_CRUX_API_KEY > [crux] api_key in the config file.
Create one in Google Cloud Console (APIs & Services → Credentials) and enable
the Chrome UX Report API. The default origin is [crux] origin.`,
	Example: `  gscx cwv
  gscx cwv --form-factor PHONE --jq 'form_factors.PHONE.metrics.lcp'
  gscx cwv --urls https://www.1000farmacie.it/,https://www.1000farmacie.it/brands/cardelli
  gscx cwv --sample 10 --type product --type category
  gscx cwv history --weeks 25`,
	Args: cobra.NoArgs,
	RunE: runCwv,
}

var cwvHistoryCmd = &cobra.Command{
	Use:   "history",
	Short: "Weekly origin p75 per metric, flagging the weeks where the rating changed",
	Long: `Queries the CrUX History API: one 28-day aggregate per week, oldest first (25
weeks by default, up to 40). Each metric's p75 is rated per week and the weeks
where the rating differs from the previous week with data are flagged
(rating_changed) and listed in rating_changes. Takes one --form-factor
(default PHONE).`,
	Args: cobra.NoArgs,
	RunE: runCwvHistory,
}

func init() {
	pf := cwvCmd.PersistentFlags()
	pf.StringVar(&flagCruxKey, "crux-key", "", "CrUX API key (env GSCX_CRUX_API_KEY; config [crux] api_key)")
	pf.StringVar(&flagCruxOrigin, "origin", "", "origin to query, e.g. https://www.example.com (config [crux] origin)")
	pf.StringSliceVar(&flagCruxForm, "form-factor", nil, "PHONE, DESKTOP, TABLET or ALL (comma-separated or repeated; default: PHONE,DESKTOP for the origin, ALL for URLs, PHONE for history)")

	f := cwvCmd.Flags()
	f.StringSliceVar(&flagCwvURLs, "urls", nil, "query these URLs instead of the origin (comma-separated or repeated)")
	f.IntVar(&flagCwvSample, "sample", 0, "query N URLs per page type, picked from the stored inspections (see --from-sitemaps)")
	f.StringArrayVar(&flagCwvTypes, "type", nil, "with --sample: only these page types (repeatable)")
	f.StringVar(&flagCwvSince, "since", "30d", "with --sample: stored inspections newer than this are the pool: a duration (30d) or a date")
	f.BoolVar(&flagCwvFromMaps, "from-sitemaps", false, "with --sample: pick from the sitemaps (network) instead of the stored inspections")
	f.StringArrayVar(&flagCwvSitemaps, "sitemap", nil, "sitemap URL for --from-sitemaps (repeatable; default: config sitemaps)")
	f.IntVar(&flagCwvConcurrent, "concurrency", 4, "parallel CrUX queries for --urls/--sample (the API allows 150 per minute; gscx paces itself)")

	cwvHistoryCmd.Flags().IntVar(&flagCwvWeeks, "weeks", 25, fmt.Sprintf("weekly periods to fetch, 1-%d", crux.MaxHistoryPeriods))

	cwvCmd.AddCommand(cwvHistoryCmd)
	rootCmd.AddCommand(cwvCmd)
}

// --- configuration -------------------------------------------------------------

// cruxKey resolves the API key: flag > env > config file.
func cruxKey() string {
	return firstNonEmpty(flagCruxKey, os.Getenv("GSCX_CRUX_API_KEY"), cfg.Crux.APIKey)
}

// cruxOriginRaw resolves the origin: flag > config file (default
// https://www.1000farmacie.it).
func cruxOriginRaw() string {
	return firstNonEmpty(flagCruxOrigin, cfg.Crux.Origin, config.DefaultCruxOrigin)
}

// normalizeOrigin reduces v to scheme://host[:port]: CrUX origins carry no
// path. A bare host gets https://.
func normalizeOrigin(v string) (string, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return "", gsc.NewValidationError("no origin to query", "set --origin or [crux] origin in the config file")
	}
	if !strings.Contains(v, "://") {
		v = "https://" + v
	}
	u, err := url.Parse(v)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return "", gsc.NewValidationError(fmt.Sprintf("cannot use %q as an origin", v), "expected e.g. https://www.example.com")
	}
	return u.Scheme + "://" + u.Host, nil
}

// cruxClient builds the paced CrUX client for the resolved key.
func cruxClient() (*crux.Client, error) {
	return newCruxClient(crux.Options{
		APIKey:      cruxKey(),
		UserAgent:   userAgent(),
		Logf:        logf,
		MinInterval: crux.DefaultMinInterval,
	})
}

// formFactors validates --form-factor values (case-insensitive; ALL = every
// form factor, sent as no formFactor) and applies def when none were given.
func formFactors(vals, def []string) ([]string, error) {
	if len(vals) == 0 {
		vals = def
	}
	var out []string
	for _, v := range vals {
		v = strings.ToUpper(strings.TrimSpace(v))
		switch v {
		case "ALL":
			v = crux.FormFactorAll
		case crux.FormFactorPhone, crux.FormFactorDesktop, crux.FormFactorTablet:
		default:
			return nil, gsc.NewValidationError(fmt.Sprintf("unknown form factor %q", v), "use PHONE, DESKTOP, TABLET or ALL")
		}
		if !slices.Contains(out, v) {
			out = append(out, v)
		}
	}
	return out, nil
}

// ffLabel is the output name of a form factor ("" = ALL).
func ffLabel(ff string) string {
	if ff == crux.FormFactorAll {
		return "ALL"
	}
	return ff
}

// --- output shapes ---------------------------------------------------------------

type cwvDensity struct {
	Good             float64 `json:"good"`
	NeedsImprovement float64 `json:"needs_improvement"`
	Poor             float64 `json:"poor"`
}

type cwvMetricOut struct {
	P75     *float64    `json:"p75,omitempty"`
	Unit    string      `json:"unit,omitempty"`
	Rating  string      `json:"rating,omitempty"`
	Density *cwvDensity `json:"density,omitempty"`
}

// cwvRecordOut is one CrUX record (an origin or URL, one form factor) or the
// reason there is none.
type cwvRecordOut struct {
	Status           string                  `json:"status"` // ok, no_data, error
	URL              string                  `json:"url,omitempty"`
	PageType         string                  `json:"page_type,omitempty"`
	FormFactor       string                  `json:"form_factor"`
	NormalizedURL    string                  `json:"normalized_url,omitempty"`
	CollectionPeriod *cwvPeriod              `json:"collection_period,omitempty"`
	Assessment       string                  `json:"cwv_assessment,omitempty"`
	Metrics          map[string]cwvMetricOut `json:"metrics,omitempty"`
	Error            string                  `json:"error,omitempty"`
}

type cwvPeriod struct {
	First string `json:"first"`
	Last  string `json:"last"`
}

type cwvThreshold struct {
	Good float64 `json:"good"`
	Poor float64 `json:"poor"`
	Unit string  `json:"unit,omitempty"`
}

func cwvThresholds() map[string]cwvThreshold {
	out := make(map[string]cwvThreshold, len(crux.Metrics))
	for _, m := range crux.Metrics {
		out[m.Short] = cwvThreshold{Good: m.Good, Poor: m.Poor, Unit: m.Unit}
	}
	return out
}

// recordOut converts an API record. Metrics without a percentile still show
// their densities (when present) but get no rating.
func recordOut(rec *crux.Record, ff string) cwvRecordOut {
	out := cwvRecordOut{Status: "ok", FormFactor: ffLabel(ff), Metrics: map[string]cwvMetricOut{}}
	if rec.Period.First != "" || rec.Period.Last != "" {
		out.CollectionPeriod = &cwvPeriod{First: rec.Period.First, Last: rec.Period.Last}
	}
	ratings := map[string]string{}
	for _, m := range crux.Metrics {
		rm, ok := rec.Metrics[m.API]
		if !ok {
			continue
		}
		mo := cwvMetricOut{P75: rm.P75, Unit: m.Unit}
		if rm.P75 != nil {
			mo.Rating = m.Rate(*rm.P75)
			ratings[m.Short] = mo.Rating
		}
		if g, ni, p, ok := rm.Densities(); ok {
			mo.Density = &cwvDensity{Good: round(g, 4), NeedsImprovement: round(ni, 4), Poor: round(p, 4)}
		}
		out.Metrics[m.Short] = mo
	}
	out.Assessment = crux.Assess(ratings)
	return out
}

// --- gscx cwv ------------------------------------------------------------------

type cwvOriginOut struct {
	Origin      string                  `json:"origin"`
	Note        string                  `json:"note"`
	Thresholds  map[string]cwvThreshold `json:"thresholds"`
	FormFactors map[string]cwvRecordOut `json:"form_factors"`
}

type cwvTypeRow struct {
	Queried     int            `json:"queried"`
	WithData    int            `json:"with_data"`
	NoData      int            `json:"no_data"`
	Errors      int            `json:"errors,omitempty"`
	WithDataPct *float64       `json:"with_data_pct,omitempty"`
	CI95        *ci95          `json:"ci95,omitempty"`
	Assessments map[string]int `json:"assessments,omitempty"`
}

type cwvSampleInfo struct {
	PerType  int            `json:"per_type"`
	Source   string         `json:"source"`
	Eligible map[string]int `json:"eligible"`
}

type cwvURLsOut struct {
	Note       string                  `json:"note"`
	Thresholds map[string]cwvThreshold `json:"thresholds"`
	FormFactor string                  `json:"form_factor"`
	Sample     *cwvSampleInfo          `json:"sample,omitempty"`
	Hint       string                  `json:"hint,omitempty"`
	Queried    int                     `json:"queried"`
	WithData   int                     `json:"with_data"`
	NoData     int                     `json:"no_data"`
	Errors     int                     `json:"errors"`
	ByType     map[string]*cwvTypeRow  `json:"by_type"`
	URLs       []cwvRecordOut          `json:"urls"`
	Trunc      *output.Truncation      `json:"truncated,omitempty"`
}

func runCwv(cmd *cobra.Command, _ []string) error {
	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	if len(flagCwvURLs) > 0 && flagCwvSample > 0 {
		return gsc.NewValidationError("--urls and --sample are mutually exclusive", "")
	}
	if flagCwvSample < 0 {
		return gsc.NewValidationError("--sample must be positive", "")
	}
	urlMode := len(flagCwvURLs) > 0 || flagCwvSample > 0
	def := []string{crux.FormFactorPhone, crux.FormFactorDesktop}
	if urlMode {
		def = []string{"ALL"}
	}
	ffs, err := formFactors(flagCruxForm, def)
	if err != nil {
		return err
	}
	client, err := cruxClient()
	if err != nil {
		return err
	}
	if urlMode {
		return runCwvURLs(ctx, cmd, client, ffs)
	}

	origin, err := normalizeOrigin(cruxOriginRaw())
	if err != nil {
		return err
	}
	out := cwvOriginOut{Origin: origin, Note: cwvNote, Thresholds: cwvThresholds(), FormFactors: map[string]cwvRecordOut{}}
	for _, ff := range ffs {
		rec, err := client.QueryRecord(ctx, crux.Target{Origin: origin, FormFactor: ff}, crux.APINames())
		switch {
		case errors.Is(err, crux.ErrNoData):
			out.FormFactors[ffLabel(ff)] = cwvRecordOut{Status: "no_data", FormFactor: ffLabel(ff)}
		case err != nil:
			return err
		default:
			out.FormFactors[ffLabel(ff)] = recordOut(rec, ff)
		}
	}
	return emit(cmd, out)
}

// cwvTarget is one URL to query.
type cwvTarget struct{ URL, PageType string }

func runCwvURLs(ctx context.Context, cmd *cobra.Command, client *crux.Client, ffs []string) error {
	if len(ffs) != 1 {
		return gsc.NewValidationError("URL queries take a single --form-factor", "use PHONE, DESKTOP, TABLET or ALL")
	}
	ff := ffs[0]
	cls, err := classifier()
	if err != nil {
		return err
	}
	out := cwvURLsOut{Note: cwvNote, Thresholds: cwvThresholds(), FormFactor: ffLabel(ff), ByType: map[string]*cwvTypeRow{}}

	var targets []cwvTarget
	if len(flagCwvURLs) > 0 {
		for _, u := range dedupe(trimAll(flagCwvURLs)) {
			targets = append(targets, cwvTarget{URL: u, PageType: cls.Classify(u)})
		}
	} else {
		groups, source, err := cwvSamplePool(ctx, cls)
		if err != nil {
			return err
		}
		picked := sampler.Sample(groups, flagCwvSample, nil, -1, defaultSeed("cwv:"+quota.Day(time.Now())))
		for _, it := range picked.Order {
			targets = append(targets, cwvTarget{URL: it.URL, PageType: it.Type})
		}
		out.Sample = &cwvSampleInfo{PerType: flagCwvSample, Source: source, Eligible: picked.Eligible}
		if len(targets) == 0 {
			out.Hint = "no URLs to sample: run 'gscx inspect batch' first, or use --from-sitemaps"
		}
	}

	results, err := queryURLs(ctx, client, targets, ff, flagCwvConcurrent)
	if err != nil {
		return err
	}
	summarizeCwvURLs(&out, results)
	capped, trunc := output.Cap(results, flagLimit)
	out.URLs, out.Trunc = capped, trunc
	if out.URLs == nil {
		out.URLs = []cwvRecordOut{}
	}
	if out.Queried > 0 && out.WithData == 0 && out.Errors == 0 {
		out.Hint = "none of these URLs has CrUX data (not enough traffic); the origin figures ('gscx cwv') are the best available"
	}
	return emit(cmd, out)
}

func trimAll(in []string) []string {
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// cwvSamplePool groups the URLs --sample draws from by page type: the latest
// stored inspections in --since, or the sitemaps with --from-sitemaps.
// --type restricts the page types.
func cwvSamplePool(ctx context.Context, cls *pagetype.Classifier) (map[string][]string, string, error) {
	for _, t := range flagCwvTypes {
		if !slices.Contains(cls.Names(), t) {
			return nil, "", gsc.NewValidationError(fmt.Sprintf("unknown page type %q", t), "known types: "+strings.Join(cls.Names(), ", "))
		}
	}
	site := siteURL()
	groups := map[string][]string{}
	source := "inspections"
	if flagCwvFromMaps {
		source = "sitemaps"
		sitemaps := flagCwvSitemaps
		if len(sitemaps) == 0 {
			sitemaps = cfg.Sitemaps
		}
		fr, err := loadFrame(ctx, site, cls, sitemap.NewClient(userAgent()), sitemaps, "", nil)
		if err != nil {
			return nil, "", err
		}
		groups = fr.Groups
	} else {
		now := time.Now()
		from, err := parseSince(flagCwvSince, now)
		if err != nil {
			return nil, "", err
		}
		st, err := openStore(ctx)
		if err != nil {
			return nil, "", err
		}
		defer func() { _ = st.Close() }()
		rows, err := st.LatestPerURL(ctx, site, from, now)
		if err != nil {
			return nil, "", err
		}
		for _, r := range rows {
			t := cls.Classify(r.URL)
			groups[t] = append(groups[t], r.URL)
		}
	}
	if len(flagCwvTypes) > 0 {
		for t := range groups {
			if !slices.Contains(flagCwvTypes, t) {
				delete(groups, t)
			}
		}
	}
	return groups, source, nil
}

// queryURLs runs one URL-level query per target with bounded concurrency,
// returning one result per target in order. A missing record is no_data; a
// request that failed is recorded per URL, except credential/permission
// failures, which affect every query and abort the run.
func queryURLs(ctx context.Context, client *crux.Client, targets []cwvTarget, ff string, concurrency int) ([]cwvRecordOut, error) {
	if concurrency < 1 {
		concurrency = 1
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	results := make([]cwvRecordOut, len(targets))
	var (
		wg    sync.WaitGroup
		mu    sync.Mutex
		fatal error
		sem   = make(chan struct{}, concurrency)
	)
	for i, t := range targets {
		if ctx.Err() != nil {
			break // a credential error (or Ctrl-C) already doomed the run
		}
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			res := cwvRecordOut{URL: t.URL, PageType: t.PageType, FormFactor: ffLabel(ff)}
			rec, err := client.QueryRecord(ctx, crux.Target{URL: t.URL, FormFactor: ff}, crux.APINames())
			switch {
			case errors.Is(err, crux.ErrNoData):
				res.Status = "no_data"
			case err != nil:
				if isCredentialError(err) {
					mu.Lock()
					if fatal == nil {
						fatal = err
						cancel()
					}
					mu.Unlock()
				}
				res.Status, res.Error = "error", err.Error()
			default:
				res = recordOut(rec, ff)
				res.URL, res.PageType = t.URL, t.PageType
				if rec.NormalizedURL != "" && rec.NormalizedURL != t.URL {
					res.NormalizedURL = rec.NormalizedURL
				}
			}
			results[i] = res
		}()
	}
	wg.Wait()
	if fatal != nil {
		return nil, fatal
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return results, nil
}

// isCredentialError reports failures that no other URL's query can escape: a
// rejected key, a disabled API, a quota/permission error.
func isCredentialError(err error) bool {
	var apiErr *cierrors.APIError
	if !errors.As(err, &apiErr) {
		return false
	}
	return apiErr.Kind == "auth_failed" || apiErr.Status == 401 || apiErr.Status == 403
}

// summarizeCwvURLs fills the counters and the per-page-type breakdown.
func summarizeCwvURLs(out *cwvURLsOut, results []cwvRecordOut) {
	for _, r := range results {
		row := out.ByType[r.PageType]
		if row == nil {
			row = &cwvTypeRow{Assessments: map[string]int{}}
			out.ByType[r.PageType] = row
		}
		out.Queried++
		row.Queried++
		switch r.Status {
		case "ok":
			out.WithData++
			row.WithData++
			row.Assessments[r.Assessment]++
		case "no_data":
			out.NoData++
			row.NoData++
		default:
			out.Errors++
			row.Errors++
		}
	}
	for _, row := range out.ByType {
		if answered := row.WithData + row.NoData; answered > 0 {
			pct := round(100*float64(row.WithData)/float64(answered), 2)
			lo, hi := stats.Wilson(row.WithData, answered, stats.Z95)
			row.WithDataPct = &pct
			row.CI95 = &ci95{Lo: round(100*lo, 2), Hi: round(100*hi, 2)}
		}
		if len(row.Assessments) == 0 {
			row.Assessments = nil
		}
	}
}

// --- gscx cwv history -------------------------------------------------------------

type cwvHistPoint struct {
	P75           *float64 `json:"p75"`
	Rating        string   `json:"rating,omitempty"`
	RatingChanged bool     `json:"rating_changed,omitempty"`
}

type cwvHistPeriod struct {
	First   string                  `json:"first"`
	Last    string                  `json:"last"`
	Metrics map[string]cwvHistPoint `json:"metrics"`
}

type cwvRatingChange struct {
	Metric string  `json:"metric"`
	First  string  `json:"first"`
	Last   string  `json:"last"`
	From   string  `json:"from"`
	To     string  `json:"to"`
	FromP  float64 `json:"from_p75"`
	ToP    float64 `json:"to_p75"`
	Unit   string  `json:"unit,omitempty"`
}

type cwvHistoryOut struct {
	Origin        string                  `json:"origin"`
	FormFactor    string                  `json:"form_factor"`
	Status        string                  `json:"status"`
	Note          string                  `json:"note"`
	Thresholds    map[string]cwvThreshold `json:"thresholds"`
	Weeks         int                     `json:"weeks"`
	Periods       []cwvHistPeriod         `json:"periods"`
	RatingChanges []cwvRatingChange       `json:"rating_changes"`
}

const cwvHistoryNote = "Each period is a 28-day aggregate ending on 'last', one per week, so adjacent weeks overlap " +
	"by three weeks: a change shows up gradually. rating_changed marks a week whose rating differs from the " +
	"previous week with data; weeks without data have p75 null and are skipped when comparing."

// buildHistory rates every weekly p75 and flags rating changes.
func buildHistory(h *crux.History) ([]cwvHistPeriod, []cwvRatingChange) {
	periods := make([]cwvHistPeriod, len(h.Periods))
	for i, p := range h.Periods {
		periods[i] = cwvHistPeriod{First: p.First, Last: p.Last, Metrics: map[string]cwvHistPoint{}}
	}
	changes := []cwvRatingChange{}
	for _, m := range crux.Metrics {
		series, ok := h.Metrics[m.API]
		if !ok {
			continue
		}
		prevRating, prevP75 := "", 0.0
		for i := range periods {
			var p75 *float64
			if i < len(series.P75s) {
				p75 = series.P75s[i]
			}
			pt := cwvHistPoint{P75: p75}
			if p75 != nil {
				pt.Rating = m.Rate(*p75)
				if prevRating != "" && pt.Rating != prevRating {
					pt.RatingChanged = true
					changes = append(changes, cwvRatingChange{
						Metric: m.Short, First: periods[i].First, Last: periods[i].Last,
						From: prevRating, To: pt.Rating, FromP: prevP75, ToP: *p75, Unit: m.Unit,
					})
				}
				prevRating, prevP75 = pt.Rating, *p75
			}
			periods[i].Metrics[m.Short] = pt
		}
	}
	sort.SliceStable(changes, func(i, j int) bool { return changes[i].Last < changes[j].Last })
	return periods, changes
}

func runCwvHistory(cmd *cobra.Command, _ []string) error {
	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	if flagCwvWeeks < 1 || flagCwvWeeks > crux.MaxHistoryPeriods {
		return gsc.NewValidationError(fmt.Sprintf("--weeks %d out of range", flagCwvWeeks), fmt.Sprintf("use 1-%d", crux.MaxHistoryPeriods))
	}
	ffs, err := formFactors(flagCruxForm, []string{crux.FormFactorPhone})
	if err != nil {
		return err
	}
	if len(ffs) != 1 {
		return gsc.NewValidationError("history takes a single --form-factor", "use PHONE, DESKTOP, TABLET or ALL")
	}
	origin, err := normalizeOrigin(cruxOriginRaw())
	if err != nil {
		return err
	}
	client, err := cruxClient()
	if err != nil {
		return err
	}
	out := cwvHistoryOut{
		Origin: origin, FormFactor: ffLabel(ffs[0]), Note: cwvHistoryNote, Thresholds: cwvThresholds(),
		Periods: []cwvHistPeriod{}, RatingChanges: []cwvRatingChange{},
	}
	h, err := client.QueryHistory(ctx, crux.Target{Origin: origin, FormFactor: ffs[0]}, crux.APINames(), flagCwvWeeks)
	switch {
	case errors.Is(err, crux.ErrNoData):
		out.Status = "no_data"
	case err != nil:
		return err
	default:
		out.Status = "ok"
		out.Periods, out.RatingChanges = buildHistory(h)
		out.Weeks = len(out.Periods)
	}
	return emit(cmd, out)
}
