package commands

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"net/url"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/nicolasacchi/clicore/cierrors"
	"github.com/nicolasacchi/gscx/internal/gsc"
	"github.com/nicolasacchi/gscx/internal/output"
	"github.com/nicolasacchi/gscx/internal/pagetype"
	"github.com/nicolasacchi/gscx/internal/quota"
	"github.com/nicolasacchi/gscx/internal/sampler"
	"github.com/nicolasacchi/gscx/internal/sitemap"
	"github.com/nicolasacchi/gscx/internal/store"
	"github.com/spf13/cobra"
	searchconsole "google.golang.org/api/searchconsole/v1"
)

const (
	// maxInspectPerMinute is Google's per-property URL Inspection rate limit.
	maxInspectPerMinute = 600
	// abortAfterFailures: when this many calls in a row at the start of a batch
	// all fail, the cause is systemic (auth, permission, wrong property).
	abortAfterFailures = 5

	reasonQuota       = "quota_exhausted"
	reasonInterrupted = "interrupted"
	reasonBudget      = "budget_exhausted"
	reasonStore       = "store_error"
	reasonAborted     = "aborted_all_failed"
)

var (
	flagInspectRaw     bool
	flagInspectSummary bool
	flagInspectForce   bool

	flagBatchSitemaps    []string
	flagBatchURLsFile    string
	flagBatchTypes       []string
	flagBatchPerType     int
	flagBatchFreshDays   int
	flagBatchSeed        int64
	flagBatchBudget      int
	flagBatchPerMinute   int
	flagBatchConcurrency int
	flagBatchDryRun      bool
)

var inspectCmd = &cobra.Command{
	Use:   "inspect <url>...",
	Short: "Inspect URLs with the URL Inspection API (and sample a whole site with 'inspect batch')",
	Long: `Calls the URL Inspection API for each URL and stores the result in the local
cache, so every inspection also grows the sample behind 'gscx coverage'.

The API allows 2,000 inspections per day per property (America/Los_Angeles
calendar day) and 600 per minute, shared with everyone using the property. gscx
counts every stored call, successful or not, against inspect.daily_budget and
refuses to go over it unless --force is given.

Output is {site, results:[…], quota}. Each result has url, page_type, verdict,
coverage_state, indexing_state, robots_txt_state, page_fetch_state,
google_canonical, user_canonical, canonical_mismatch, last_crawl_time,
crawled_as, inspection_link. --raw returns the full API result instead.

verdict PASS means "URL is on Google"; NEUTRAL usually means excluded from the
index (see coverage_state); FAIL means errors.`,
	Args: cobra.MinimumNArgs(1),
	RunE: runInspect,
}

var inspectBatchCmd = &cobra.Command{
	Use:   "batch",
	Short: "Inspect a stratified random sample of the site's URLs",
	Long: `Draws a stratified random sample (--per-type URLs of each page type) from the
sitemaps (--sitemap, default: config sitemaps) or a URL list (--urls-file),
skips URLs successfully inspected within --fresh-days, caps the run at the
remaining daily budget, and inspects the sample at --per-minute across
--concurrency workers. Every result is written to the cache immediately, so an
interrupted run loses nothing.

The default --seed is derived from the quota day: a re-run the same day is
reproducible, and a run on another day picks different URLs. Because fresh URLs
are skipped, re-running keeps extending the sample.

Stops gracefully on HTTP 429 (stopped_reason "quota_exhausted") and on Ctrl-C
("interrupted"). Aborts with the error if the first 5 calls all fail (usually
credentials or permissions). --dry-run prints frame sizes and the plan without
calling the API.`,
	Args: cobra.NoArgs,
	RunE: runInspectBatch,
}

func init() {
	f := inspectCmd.Flags()
	f.BoolVar(&flagInspectSummary, "summary", true, "print the flattened summary per URL (default)")
	f.BoolVar(&flagInspectRaw, "raw", false, "print the full API result per URL instead of the summary")
	f.BoolVar(&flagInspectForce, "force", false, "inspect even if today's budget is exhausted")

	b := inspectBatchCmd.Flags()
	b.StringArrayVar(&flagBatchSitemaps, "sitemap", nil, "sitemap or sitemap-index URL to sample from (repeatable; default: config sitemaps)")
	b.StringVar(&flagBatchURLsFile, "urls-file", "", "file with one URL per line to sample from ('-' = stdin) instead of sitemaps")
	b.StringSliceVar(&flagBatchTypes, "types", nil, "restrict sampling to these page types (comma-separated)")
	b.IntVar(&flagBatchPerType, "per-type", 0, "URLs to sample per page type (default: config inspect.per_type)")
	b.IntVar(&flagBatchFreshDays, "fresh-days", 0, "skip URLs successfully inspected within N days; 0 disables (default: config inspect.fresh_days)")
	b.Int64Var(&flagBatchSeed, "seed", 0, "random seed (default: derived from the quota day)")
	b.IntVar(&flagBatchBudget, "budget", 0, "inspect at most N URLs this run (can only lower the remaining daily budget)")
	b.IntVar(&flagBatchPerMinute, "per-minute", 0, "request rate shared by all workers, max 600 (default: config inspect.per_minute)")
	b.IntVar(&flagBatchConcurrency, "concurrency", 4, "parallel workers")
	b.BoolVar(&flagBatchDryRun, "dry-run", false, "print frame sizes, plan and budget; make no API calls")
	inspectBatchCmd.MarkFlagsMutuallyExclusive("sitemap", "urls-file")

	inspectCmd.AddCommand(inspectBatchCmd)
	rootCmd.AddCommand(inspectCmd)
}

// --- API seam ---------------------------------------------------------------

// urlInspector is the one Search Console call inspect makes; tests fake it.
type urlInspector interface {
	Inspect(ctx context.Context, site, inspectionURL string) (*searchconsole.UrlInspectionResult, error)
}

type gscInspector struct{ svc *searchconsole.Service }

func (g gscInspector) Inspect(ctx context.Context, site, inspectionURL string) (*searchconsole.UrlInspectionResult, error) {
	resp, err := g.svc.UrlInspection.Index.Inspect(&searchconsole.InspectUrlIndexRequest{
		InspectionUrl: inspectionURL,
		SiteUrl:       site,
	}).Context(ctx).Do()
	if err != nil {
		return nil, gsc.Wrap(err)
	}
	if resp.InspectionResult == nil {
		return nil, errors.New("response has no inspection result")
	}
	return resp.InspectionResult, nil
}

func is429(err error) bool {
	var ae *cierrors.APIError
	return errors.As(err, &ae) && ae.Status == 429
}

// --- one inspection: call + store -------------------------------------------

type inspectRunner struct {
	API   urlInspector
	Store *store.Store
	Site  string
	Now   func() time.Time
}

type callResult struct {
	Rec      store.Inspection
	Link     string
	Raw      json.RawMessage
	CallErr  error // API/transport error (recorded in the store)
	StoreErr error // failure to persist the row: fatal
	// Interrupted means ctx was cancelled mid-call; the call was not stored.
	Interrupted bool
}

// call inspects one URL and stores the outcome. Calls cancelled by ctx are not
// stored (we cannot tell whether Google counted them) and return ctx's error
// as CallErr. The row is written with a non-cancelled context so a result that
// arrived just before Ctrl-C is never lost.
func (r *inspectRunner) call(ctx context.Context, rawURL, pageType string) callResult {
	now := time.Now
	if r.Now != nil {
		now = r.Now
	}
	at := now()
	rec := store.Inspection{Site: r.Site, URL: rawURL, PageType: pageType, InspectedAt: at}

	res, err := r.API.Inspect(ctx, r.Site, rawURL)
	if err != nil && ctx.Err() != nil {
		return callResult{Rec: rec, CallErr: ctx.Err(), Interrupted: true}
	}
	out := callResult{}
	if err == nil && res.IndexStatusResult == nil {
		err = errors.New("response has no index status result")
	}
	if err != nil {
		rec.Error = err.Error()
		out.CallErr = err
	} else {
		ix := res.IndexStatusResult
		rec.Verdict = ix.Verdict
		rec.CoverageState = ix.CoverageState
		rec.IndexingState = ix.IndexingState
		rec.RobotsTxtState = ix.RobotsTxtState
		rec.PageFetchState = ix.PageFetchState
		rec.GoogleCanonical = ix.GoogleCanonical
		rec.UserCanonical = ix.UserCanonical
		rec.LastCrawlTime = ix.LastCrawlTime
		rec.CrawledAs = ix.CrawledAs
		if raw, mErr := output.MarshalNoEscape(res); mErr == nil {
			rec.RawJSON = string(raw)
			out.Raw = raw
		}
		out.Link = res.InspectionResultLink
	}
	if sErr := r.Store.InsertInspection(context.WithoutCancel(ctx), rec); sErr != nil {
		out.StoreErr = sErr
	}
	out.Rec = rec
	return out
}

// --- inspect <url>... --------------------------------------------------------

type inspectSummary struct {
	URL               string `json:"url"`
	PageType          string `json:"page_type"`
	Verdict           string `json:"verdict"`
	CoverageState     string `json:"coverage_state"`
	IndexingState     string `json:"indexing_state"`
	RobotsTxtState    string `json:"robots_txt_state"`
	PageFetchState    string `json:"page_fetch_state"`
	GoogleCanonical   string `json:"google_canonical"`
	UserCanonical     string `json:"user_canonical"`
	CanonicalMismatch bool   `json:"canonical_mismatch"`
	LastCrawlTime     string `json:"last_crawl_time"`
	CrawledAs         string `json:"crawled_as"`
	InspectionLink    string `json:"inspection_link"`
	InspectedAt       string `json:"inspected_at"`
	Error             string `json:"error,omitempty"`
}

type inspectRawRow struct {
	URL              string          `json:"url"`
	PageType         string          `json:"page_type"`
	InspectedAt      string          `json:"inspected_at"`
	InspectionResult json.RawMessage `json:"inspection_result,omitempty"`
	Error            string          `json:"error,omitempty"`
}

type quotaOut struct {
	Day        string `json:"day"`
	UsedBefore int    `json:"used_before"`
	UsedAfter  *int   `json:"used_after,omitempty"`
	Budget     int    `json:"budget"`
	Remaining  int    `json:"remaining"`
	BatchLimit int    `json:"batch_limit,omitempty"`
}

type inspectOutput struct {
	Site    string   `json:"site"`
	Results []any    `json:"results"`
	Quota   quotaOut `json:"quota"`
}

// canonicalMismatch reports whether Google chose a different canonical than the
// page declares. Both must be set. A bare origin and the same origin with a
// trailing slash ("https://example.com" vs "https://example.com/") are the same
// URL and do not count.
func canonicalMismatch(google, user string) bool {
	return google != "" && user != "" && normalizeCanonical(google) != normalizeCanonical(user)
}

func normalizeCanonical(s string) string {
	if u, err := url.Parse(s); err == nil && u.Path == "" && u.Host != "" {
		u.Path = "/"
		return u.String()
	}
	return s
}

func summarize(rec store.Inspection, link string) inspectSummary {
	return inspectSummary{
		URL: rec.URL, PageType: rec.PageType,
		Verdict: rec.Verdict, CoverageState: rec.CoverageState, IndexingState: rec.IndexingState,
		RobotsTxtState: rec.RobotsTxtState, PageFetchState: rec.PageFetchState,
		GoogleCanonical: rec.GoogleCanonical, UserCanonical: rec.UserCanonical,
		CanonicalMismatch: canonicalMismatch(rec.GoogleCanonical, rec.UserCanonical),
		LastCrawlTime:     rec.LastCrawlTime, CrawledAs: rec.CrawledAs,
		InspectionLink: link,
		InspectedAt:    rec.InspectedAt.UTC().Format(time.RFC3339),
		Error:          rec.Error,
	}
}

func runInspect(cmd *cobra.Command, args []string) error {
	ctx := cmd.Context()
	site := siteURL()
	urls := dedupe(args)
	for _, u := range urls {
		if !inProperty(site, u) {
			return &cierrors.APIError{
				Kind:   "validation",
				Detail: fmt.Sprintf("%s is not under the property %s", u, site),
				Hint:   "the URL Inspection API only inspects URLs inside the property; check --site",
			}
		}
	}
	cls, err := classifier()
	if err != nil {
		return err
	}
	st, err := openStore(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()

	day := quota.Day(time.Now())
	used, err := st.UsedOnDay(ctx, site, day)
	if err != nil {
		return err
	}
	budget := cfg.Inspect.DailyBudget
	if remaining := budget - used; !flagInspectForce && remaining < len(urls) {
		return &cierrors.APIError{
			Kind:   "budget_exhausted",
			Detail: fmt.Sprintf("daily inspection budget: %d used of %d on %s, %d remaining, %d requested", used, budget, day, max(remaining, 0), len(urls)),
			Hint:   "pass --force to go over inspect.daily_budget (Google's hard limit is 2,000/day per property, shared with other users)",
		}
	}

	svc, err := newGSC(ctx)
	if err != nil {
		return err
	}
	runner := &inspectRunner{API: gscInspector{svc}, Store: st, Site: site}
	rows, runErr := inspectAll(ctx, runner, cls, urls, flagInspectRaw || !flagInspectSummary)
	if len(rows) == 0 && runErr != nil {
		return runErr
	}

	after, qErr := st.UsedOnDay(context.WithoutCancel(ctx), site, day)
	if qErr != nil {
		return qErr
	}
	out := inspectOutput{
		Site: site, Results: rows,
		Quota: quotaOut{Day: day, UsedBefore: used, UsedAfter: &after, Budget: budget, Remaining: max(budget-after, 0)},
	}
	if err := emit(cmd, out); err != nil {
		return err
	}
	return runErr
}

// inspectAll inspects urls in order. It returns the rows produced so far and,
// separately, an error that should fail the command *after* the rows are
// printed (429, interruption, a cache write failure, or every URL failing).
func inspectAll(ctx context.Context, r *inspectRunner, cls *pagetype.Classifier, urls []string, raw bool) ([]any, error) {
	rows := make([]any, 0, len(urls))
	var firstErr error
	failed := 0
	for _, u := range urls {
		res := r.call(ctx, u, cls.Classify(u))
		if res.Interrupted {
			return rows, res.CallErr
		}
		if raw {
			rows = append(rows, inspectRawRow{
				URL: res.Rec.URL, PageType: res.Rec.PageType,
				InspectedAt:      res.Rec.InspectedAt.UTC().Format(time.RFC3339),
				InspectionResult: res.Raw, Error: res.Rec.Error,
			})
		} else {
			rows = append(rows, summarize(res.Rec, res.Link))
		}
		if res.StoreErr != nil {
			return rows, res.StoreErr
		}
		if res.CallErr != nil {
			failed++
			if firstErr == nil {
				firstErr = res.CallErr
			}
			if is429(res.CallErr) {
				return rows, res.CallErr // out of quota: stop, keep what we have
			}
		}
	}
	if failed == len(urls) {
		return rows, firstErr
	}
	return rows, nil
}

// --- frame: the URL universe ------------------------------------------------

type frameOut struct {
	Sources       []string             `json:"sources"`
	URLs          int                  `json:"urls"`
	ByType        map[string]int       `json:"by_type"`
	OutOfProperty int                  `json:"out_of_property,omitempty"`
	SitemapErrors []sitemap.FetchError `json:"sitemap_errors,omitempty"`
}

type frame struct {
	Sources       []string
	Groups        map[string][]string // page type -> URLs
	Total         int
	OutOfProperty int
	SitemapErrors []sitemap.FetchError
}

func (f *frame) out() frameOut {
	by := make(map[string]int, len(f.Groups))
	for t, us := range f.Groups {
		by[t] = len(us)
	}
	return frameOut{Sources: f.Sources, URLs: f.Total, ByType: by, OutOfProperty: f.OutOfProperty, SitemapErrors: f.SitemapErrors}
}

// loadFrame collects the URL universe from a URL list (urlsFile, "-" = stdin)
// or by walking sitemaps, drops URLs outside the property, and groups the rest
// by page type.
func loadFrame(ctx context.Context, site string, cls *pagetype.Classifier, client *sitemap.Client, sitemaps []string, urlsFile string, stdin io.Reader) (*frame, error) {
	f := &frame{Groups: map[string][]string{}}
	var all []string

	if urlsFile != "" {
		r := stdin
		f.Sources = []string{"stdin"}
		if urlsFile != "-" {
			fh, err := os.Open(urlsFile)
			if err != nil {
				return nil, fmt.Errorf("urls file: %w", err)
			}
			defer func() { _ = fh.Close() }()
			r = fh
			f.Sources = []string{"file:" + urlsFile}
		}
		urls, err := readURLList(r)
		if err != nil {
			return nil, fmt.Errorf("read urls: %w", err)
		}
		all = urls
	} else {
		if len(sitemaps) == 0 {
			return nil, errors.New("no sitemap to sample from: pass --sitemap or --urls-file, or set sitemaps in the config")
		}
		f.Sources = append([]string(nil), sitemaps...)
		seen := map[string]bool{}
		for _, root := range sitemaps {
			logf("walking sitemap %s", root)
			w, err := client.Walk(ctx, root, 0)
			if err != nil {
				return nil, err
			}
			for _, sm := range w.Sitemaps {
				if sm.Error != nil {
					f.SitemapErrors = append(f.SitemapErrors, *sm.Error)
				}
			}
			for _, u := range w.URLs {
				if !seen[u.Loc] {
					seen[u.Loc] = true
					all = append(all, u.Loc)
				}
			}
		}
		if len(all) == 0 && len(f.SitemapErrors) > 0 {
			return nil, &f.SitemapErrors[0]
		}
	}

	for _, u := range all {
		if !inProperty(site, u) {
			f.OutOfProperty++
			continue
		}
		t := cls.Classify(u)
		f.Groups[t] = append(f.Groups[t], u)
		f.Total++
	}
	return f, nil
}

func readURLList(r io.Reader) ([]string, error) {
	var out []string
	seen := map[string]bool{}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") || seen[line] {
			continue
		}
		seen[line] = true
		out = append(out, line)
	}
	return out, sc.Err()
}

func dedupe(in []string) []string {
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s != "" && !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

// inProperty reports whether rawURL can be inspected under site: a
// "sc-domain:example.com" property covers example.com and its subdomains over
// http(s); a URL-prefix property covers URLs starting with the prefix.
func inProperty(site, rawURL string) bool {
	if dom, ok := strings.CutPrefix(site, "sc-domain:"); ok {
		u, err := url.Parse(rawURL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
			return false
		}
		host := strings.ToLower(u.Hostname())
		dom = strings.ToLower(dom)
		return host == dom || strings.HasSuffix(host, "."+dom)
	}
	return strings.HasPrefix(rawURL, site)
}

// defaultSeed derives the sampling seed from the quota day.
func defaultSeed(day string) int64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte("gscx-inspect:" + day))
	return int64(h.Sum64() >> 1)
}

// --- inspect batch -----------------------------------------------------------

type batchPlanOut struct {
	Total          int            `json:"total"`
	ByType         map[string]int `json:"by_type"`
	PerType        int            `json:"per_type"`
	EligibleByType map[string]int `json:"eligible_by_type"`
	FreshExcluded  int            `json:"fresh_excluded"`
}

type typeResults struct {
	N              int            `json:"n"`
	Errors         int            `json:"errors,omitempty"`
	Verdicts       map[string]int `json:"verdicts"`
	CoverageStates map[string]int `json:"coverage_states"`
}

type batchDryRunOut struct {
	Site   string       `json:"site"`
	DryRun bool         `json:"dry_run"`
	Seed   int64        `json:"seed"`
	Frame  frameOut     `json:"frame"`
	Plan   batchPlanOut `json:"plan"`
	Quota  quotaOut     `json:"quota"`
}

type batchOut struct {
	Site          string                  `json:"site"`
	Seed          int64                   `json:"seed"`
	Frame         frameOut                `json:"frame"`
	Plan          batchPlanOut            `json:"plan"`
	Inspected     int                     `json:"inspected"`
	Errors        int                     `json:"errors"`
	ErrorSamples  []string                `json:"error_samples,omitempty"`
	StoppedReason string                  `json:"stopped_reason,omitempty"`
	Quota         quotaOut                `json:"quota"`
	ResultsByType map[string]*typeResults `json:"results_by_type"`
}

func runInspectBatch(cmd *cobra.Command, _ []string) error {
	ctx := cmd.Context()
	site := siteURL()
	flags := cmd.Flags()
	now := time.Now()

	cls, err := classifier()
	if err != nil {
		return err
	}
	perType := cfg.Inspect.PerType
	if flags.Changed("per-type") {
		perType = flagBatchPerType
	}
	if perType <= 0 {
		return &cierrors.APIError{Kind: "validation", Detail: "--per-type must be at least 1"}
	}
	freshDays := cfg.Inspect.FreshDays
	if flags.Changed("fresh-days") {
		freshDays = flagBatchFreshDays
	}
	perMinute := cfg.Inspect.PerMinute
	if flags.Changed("per-minute") {
		perMinute = flagBatchPerMinute
	}
	if perMinute > maxInspectPerMinute {
		logf("--per-minute %d exceeds Google's limit; using %d", perMinute, maxInspectPerMinute)
		perMinute = maxInspectPerMinute
	}
	if flagBatchConcurrency < 1 {
		return &cierrors.APIError{Kind: "validation", Detail: "--concurrency must be at least 1"}
	}
	for _, t := range flagBatchTypes {
		if !slices.Contains(cls.Names(), t) {
			return &cierrors.APIError{Kind: "validation",
				Detail: fmt.Sprintf("unknown page type %q", t),
				Hint:   "known types: " + strings.Join(cls.Names(), ", ")}
		}
	}

	sitemaps := flagBatchSitemaps
	if len(sitemaps) == 0 {
		sitemaps = cfg.Sitemaps
	}
	fr, err := loadFrame(ctx, site, cls, sitemap.NewClient(userAgent()), sitemaps, flagBatchURLsFile, cmd.InOrStdin())
	if err != nil {
		return err
	}
	logf("frame: %d URLs", fr.Total)

	st, err := openStore(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()

	day := quota.Day(now)
	used, err := st.UsedOnDay(ctx, site, day)
	if err != nil {
		return err
	}
	budget := cfg.Inspect.DailyBudget
	remaining := budget - used
	q := quotaOut{Day: day, UsedBefore: used, Budget: budget}
	if flags.Changed("budget") && flagBatchBudget < remaining {
		remaining = flagBatchBudget
		q.BatchLimit = flagBatchBudget
	}
	remaining = max(remaining, 0)

	var exclude map[string]bool
	if freshDays > 0 {
		exclude, err = st.RecentlyInspected(ctx, site, now.Add(-time.Duration(freshDays)*24*time.Hour))
		if err != nil {
			return err
		}
	}
	seed := defaultSeed(day)
	if flags.Changed("seed") {
		seed = flagBatchSeed
	}

	groups := fr.Groups
	if len(flagBatchTypes) > 0 {
		groups = map[string][]string{}
		for _, t := range flagBatchTypes {
			groups[t] = fr.Groups[t]
		}
	}
	plan := sampler.Sample(groups, perType, exclude, remaining, seed)
	planOut := batchPlanOut{
		Total: plan.Total(), PerType: perType,
		ByType: map[string]int{}, EligibleByType: plan.Eligible,
	}
	for t, us := range groups {
		planOut.ByType[t] = len(plan.ByType[t])
		planOut.FreshExcluded += len(us) - plan.Eligible[t]
	}
	logf("plan: %d URLs (%v), remaining budget %d, seed %d", plan.Total(), planOut.ByType, remaining, seed)

	q.Remaining = max(budget-used, 0)
	if flagBatchDryRun {
		return emit(cmd, batchDryRunOut{Site: site, DryRun: true, Seed: seed, Frame: fr.out(), Plan: planOut, Quota: q})
	}

	out := batchOut{Site: site, Seed: seed, Frame: fr.out(), Plan: planOut, ResultsByType: map[string]*typeResults{}}
	if remaining == 0 || plan.Total() == 0 {
		if remaining == 0 {
			out.StoppedReason = reasonBudget
		}
		q.UsedAfter = &used
		out.Quota = q
		return emit(cmd, out)
	}

	svc, err := newGSC(ctx)
	if err != nil {
		return err
	}
	br := &batchRunner{
		inspectRunner: inspectRunner{API: gscInspector{svc}, Store: st, Site: site},
		Limiter:       quota.NewLimiter(perMinute),
		Concurrency:   flagBatchConcurrency,
		Progress:      logf,
	}
	res := br.Run(ctx, plan.Order)

	after, qErr := st.UsedOnDay(context.WithoutCancel(ctx), site, day)
	if qErr != nil {
		return qErr
	}
	q.UsedAfter, q.Remaining = &after, max(budget-after, 0)
	if res.AbortErr != nil {
		return res.AbortErr
	}
	out.Inspected, out.Errors, out.ErrorSamples = res.Inspected, res.Errors, res.ErrorSamples
	out.StoppedReason, out.ResultsByType, out.Quota = res.StoppedReason, res.ByType, q
	return emit(cmd, out)
}

// --- batch runner -------------------------------------------------------------

type batchRunner struct {
	inspectRunner
	Limiter     *quota.Limiter
	Concurrency int
	Progress    func(format string, args ...any)
}

type batchOutcome struct {
	Inspected     int
	Errors        int
	ErrorSamples  []string
	StoppedReason string
	ByType        map[string]*typeResults
	// AbortErr is set when the run must fail instead of reporting: the first
	// calls all failed (systemic cause) or the cache could not be written.
	AbortErr error
}

func orNone(s string) string {
	if s == "" {
		return "(none)"
	}
	return s
}

// Run inspects items with Concurrency workers sharing Limiter. It never
// returns before every started call has been stored.
func (b *batchRunner) Run(ctx context.Context, items []sampler.Item) *batchOutcome {
	out := &batchOutcome{ByType: map[string]*typeResults{}}
	var (
		mu        sync.Mutex // guards out, completed, failed
		completed int
		failed    int
		firstErr  error

		stop     = make(chan struct{})
		stopOnce sync.Once
	)
	// halt records why the run stopped (first reason wins) and tells the
	// feeder and workers to take no new items; in-flight calls still finish
	// and are stored.
	halt := func(reason string) {
		stopOnce.Do(func() {
			out.StoppedReason = reason
			close(stop)
		})
	}
	typeRow := func(t string) *typeResults {
		tr := out.ByType[t]
		if tr == nil {
			tr = &typeResults{Verdicts: map[string]int{}, CoverageStates: map[string]int{}}
			out.ByType[t] = tr
		}
		return tr
	}

	record := func(it sampler.Item, res callResult) {
		mu.Lock()
		defer mu.Unlock()
		completed++
		switch {
		case res.StoreErr != nil:
			out.AbortErr = res.StoreErr
			halt(reasonStore)
		case res.CallErr == nil:
			out.Inspected++
			tr := typeRow(it.Type)
			tr.N++
			tr.Verdicts[orNone(res.Rec.Verdict)]++
			tr.CoverageStates[orNone(res.Rec.CoverageState)]++
		default:
			out.Errors++
			typeRow(it.Type).Errors++
			if len(out.ErrorSamples) < 5 {
				out.ErrorSamples = append(out.ErrorSamples, fmt.Sprintf("%s: %s", it.URL, res.Rec.Error))
			}
			if is429(res.CallErr) {
				halt(reasonQuota)
				return
			}
			failed++
			if firstErr == nil {
				firstErr = res.CallErr
			}
			if completed == abortAfterFailures && failed == abortAfterFailures {
				out.AbortErr = firstErr
				halt(reasonAborted)
			}
		}
		if b.Progress != nil && completed%25 == 0 {
			b.Progress("progress: %d/%d done (%d ok, %d errors)", completed, len(items), out.Inspected, out.Errors)
		}
	}

	jobs := make(chan sampler.Item)
	feederDone := make(chan struct{})
	go func() {
		defer close(feederDone)
		defer close(jobs)
		for _, it := range items {
			select {
			case jobs <- it:
			case <-stop:
				return
			case <-ctx.Done():
				halt(reasonInterrupted)
				return
			}
		}
	}()

	var wg sync.WaitGroup
	for range max(b.Concurrency, 1) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for it := range jobs {
				select {
				case <-stop:
					return
				default:
				}
				if err := b.Limiter.Wait(ctx); err != nil {
					halt(reasonInterrupted)
					return
				}
				select {
				case <-stop:
					return
				default:
				}
				res := b.call(ctx, it.URL, it.Type)
				if res.Interrupted {
					halt(reasonInterrupted)
					return
				}
				record(it, res)
			}
		}()
	}
	wg.Wait()
	<-feederDone

	return out
}
