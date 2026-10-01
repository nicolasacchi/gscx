package commands

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/nicolasacchi/gscx/internal/gsc"
	"github.com/nicolasacchi/gscx/internal/output"
	"github.com/nicolasacchi/gscx/internal/sitemap"
	"github.com/spf13/cobra"
	searchconsole "google.golang.org/api/searchconsole/v1"
)

// sitemapIndexedNote explains why the "indexed" counter is dropped: Google
// deprecated it and returns 0 for every sitemap, which reads as "nothing is
// indexed".
const sitemapIndexedNote = "deprecated by Google (always 0); use gscx coverage"

const (
	// sitemapRedundantPct: a sitemap is redundant when at least this percent
	// of its URLs also appear in a single other submitted sitemap.
	sitemapRedundantPct = 95
	// sitemapAuditConcurrency bounds sitemaps probed in parallel (each walk
	// already fetches children 4 at a time).
	sitemapAuditConcurrency = 3
)

var sitemapsCmd = &cobra.Command{
	Use:   "sitemaps",
	Short: "Submitted sitemaps: status from Google, reachability and overlap audit",
}

var sitemapsListCmd = &cobra.Command{
	Use:   "list",
	Short: "List the sitemaps submitted for the property",
	Long: `Lists submitted sitemaps as Google reports them (last submitted/downloaded,
warnings, errors, submitted URL counts). The deprecated "indexed" counter is
removed: Google returns it as always 0.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx := cmd.Context()
		svc, err := newGSC(ctx)
		if err != nil {
			return err
		}
		site := siteURL()
		resp, err := svc.Sitemaps.List(site).Context(ctx).Do()
		if err != nil {
			return gsc.Wrap(err)
		}
		views := sitemapViews(resp.Sitemap)
		shown, trunc := output.Cap(views, flagLimit)
		return emit(cmd, sitemapsListResult{
			Site: site, IndexedField: sitemapIndexedNote, Sitemaps: shown, Truncated: trunc,
		})
	},
}

var sitemapsGetCmd = &cobra.Command{
	Use:   "get <feedpath>",
	Short: "Show one submitted sitemap",
	Long:  "Shows one submitted sitemap; <feedpath> is the sitemap URL exactly as submitted.",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx := cmd.Context()
		svc, err := newGSC(ctx)
		if err != nil {
			return err
		}
		site := siteURL()
		sm, err := svc.Sitemaps.Get(site, args[0]).Context(ctx).Do()
		if err != nil {
			return gsc.Wrap(err)
		}
		return emit(cmd, sitemapGetResult{Site: site, IndexedField: sitemapIndexedNote, Sitemap: toSitemapView(sm)})
	},
}

var (
	saStaleDays int
	saExpand    bool
)

var sitemapsAuditCmd = &cobra.Command{
	Use:   "audit",
	Short: "Check every submitted sitemap: reachable? stale? errors? redundant?",
	Long: `Audits each submitted sitemap by fetching it from the web (the way Google
would) and combining that with Google's own status. Issues:

  unreachable_dns, unreachable_http_<code>, unreachable_<kind>
                     the sitemap cannot be fetched or parsed
  stale_download     Google last downloaded it more than --stale-days ago
  has_errors         Google reports errors for it
  has_warnings       Google reports warnings for it
  unreachable_children  (--expand) a child sitemap of an index failed to load
  redundant          (--expand) >= 95% of its URLs are in one other sitemap

--expand walks every sitemap (indexes included), counts unique page URLs and
page types, and measures pairwise overlap: for each sitemap, the other sitemap
holding the largest share of its URLs.`,
	Args: cobra.NoArgs,
	RunE: runSitemapsAudit,
}

// --- output shapes -------------------------------------------------------

type sitemapContentView struct {
	Type      string `json:"type"`
	Submitted int64  `json:"submitted"`
}

type sitemapView struct {
	Path            string               `json:"path"`
	LastSubmitted   string               `json:"last_submitted,omitempty"`
	LastDownloaded  string               `json:"last_downloaded,omitempty"`
	IsPending       bool                 `json:"is_pending"`
	IsSitemapsIndex bool                 `json:"is_sitemaps_index"`
	Type            string               `json:"type,omitempty"`
	Warnings        int64                `json:"warnings"`
	Errors          int64                `json:"errors"`
	Contents        []sitemapContentView `json:"contents"`
}

type sitemapsListResult struct {
	Site         string             `json:"site"`
	IndexedField string             `json:"indexed_field"`
	Sitemaps     []sitemapView      `json:"sitemaps"`
	Truncated    *output.Truncation `json:"truncated,omitempty"`
}

type sitemapGetResult struct {
	Site         string      `json:"site"`
	IndexedField string      `json:"indexed_field"`
	Sitemap      sitemapView `json:"sitemap"`
}

// toSitemapView converts the API shape, dropping the deprecated indexed counter.
func toSitemapView(sm *searchconsole.WmxSitemap) sitemapView {
	v := sitemapView{
		Path:            sm.Path,
		LastSubmitted:   sm.LastSubmitted,
		LastDownloaded:  sm.LastDownloaded,
		IsPending:       sm.IsPending,
		IsSitemapsIndex: sm.IsSitemapsIndex,
		Type:            sm.Type,
		Warnings:        sm.Warnings,
		Errors:          sm.Errors,
		Contents:        []sitemapContentView{},
	}
	for _, c := range sm.Contents {
		if c != nil {
			v.Contents = append(v.Contents, sitemapContentView{Type: c.Type, Submitted: c.Submitted})
		}
	}
	return v
}

// sitemapViews converts and sorts by path (the API order is not stable).
func sitemapViews(in []*searchconsole.WmxSitemap) []sitemapView {
	out := make([]sitemapView, 0, len(in))
	for _, sm := range in {
		if sm != nil {
			out = append(out, toSitemapView(sm))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

type sitemapFetchErrView struct {
	Kind       string `json:"kind"`
	StatusCode int    `json:"status_code,omitempty"`
	Error      string `json:"error"`
}

type sitemapChildError struct {
	URL        string `json:"url"`
	Kind       string `json:"kind"`
	StatusCode int    `json:"status_code,omitempty"`
}

type sitemapOverlap struct {
	With   string  `json:"with"`
	Shared int     `json:"shared"`
	Share  float64 `json:"share"`
}

type sitemapAuditEntry struct {
	Path                  string               `json:"path"`
	Reachable             bool                 `json:"reachable"`
	FetchError            *sitemapFetchErrView `json:"fetch_error,omitempty"`
	Kind                  string               `json:"kind,omitempty"`
	Children              *int                 `json:"children,omitempty"`
	LastDownloaded        string               `json:"last_downloaded,omitempty"`
	LastDownloadedAgeDays *int                 `json:"last_downloaded_age_days,omitempty"`
	Errors                int64                `json:"errors"`
	Warnings              int64                `json:"warnings"`
	Submitted             int64                `json:"submitted"`
	// URLs is the number of unique page URLs; Entries the number of <url>
	// elements before de-duplication (image sitemaps repeat a page once per
	// image, so Entries can be much larger than URLs).
	URLs        *int                `json:"urls,omitempty"`
	Entries     *int                `json:"entries,omitempty"`
	ByType      map[string]int      `json:"by_type,omitempty"`
	ChildErrors []sitemapChildError `json:"child_errors,omitempty"`
	Overlap     *sitemapOverlap     `json:"overlap,omitempty"`
	Issues      []string            `json:"issues"`
}

type sitemapAuditSummary struct {
	Sitemaps     int            `json:"sitemaps"`
	WithIssues   int            `json:"with_issues"`
	IssuesByKind map[string]int `json:"issues_by_kind"`
}

type sitemapAuditResult struct {
	Site      string              `json:"site"`
	StaleDays int                 `json:"stale_days"`
	Expanded  bool                `json:"expanded"`
	Sitemaps  []sitemapAuditEntry `json:"sitemaps"`
	Summary   sitemapAuditSummary `json:"summary"`
	Truncated *output.Truncation  `json:"truncated,omitempty"`
}

// --- audit ---------------------------------------------------------------

func runSitemapsAudit(cmd *cobra.Command, _ []string) error {
	if saStaleDays < 0 {
		return gsc.NewValidationError("--stale-days must be >= 0", "")
	}
	ctx := cmd.Context()
	svc, err := newGSC(ctx)
	if err != nil {
		return err
	}
	site := siteURL()
	resp, err := svc.Sitemaps.List(site).Context(ctx).Do()
	if err != nil {
		return gsc.Wrap(err)
	}
	var c func(string) string
	if saExpand {
		cl, err := classifier()
		if err != nil {
			return err
		}
		c = cl.Classify
	}

	client := sitemap.NewClient(userAgent())
	entries, err := auditSitemaps(ctx, client, resp.Sitemap, saExpand, c, time.Now(), saStaleDays)
	if err != nil {
		return err
	}
	summary := summarizeSitemapAudit(entries)
	shown, trunc := output.Cap(entries, flagLimit)
	return emit(cmd, sitemapAuditResult{
		Site: site, StaleDays: saStaleDays, Expanded: saExpand,
		Sitemaps: shown, Summary: summary, Truncated: trunc,
	})
}

// auditSitemaps audits every submitted sitemap (sorted by path). With expand,
// each is walked fully and pairwise overlap is computed.
func auditSitemaps(ctx context.Context, client *sitemap.Client, in []*searchconsole.WmxSitemap, expand bool, classify func(string) string, now time.Time, staleDays int) ([]sitemapAuditEntry, error) {
	var list []*searchconsole.WmxSitemap
	for _, sm := range in {
		if sm != nil {
			list = append(list, sm)
		}
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Path < list[j].Path })

	entries := make([]sitemapAuditEntry, len(list))
	sets := make([]map[string]struct{}, len(list))
	err := forEachBoundedIdx(ctx, len(list), sitemapAuditConcurrency, func(ctx context.Context, i int) error {
		logf("sitemaps audit: %s", list[i].Path)
		e, set, err := auditSitemap(ctx, client, list[i], expand, classify, now, staleDays)
		if err != nil {
			return err
		}
		entries[i], sets[i] = e, set
		return nil
	})
	if err != nil {
		return nil, err
	}
	if expand {
		applySitemapOverlaps(entries, sets)
	}
	return entries, nil
}

// auditSitemap probes one sitemap. The returned URL set is non-nil only with
// expand and a reachable sitemap.
func auditSitemap(ctx context.Context, client *sitemap.Client, sm *searchconsole.WmxSitemap, expand bool, classify func(string) string, now time.Time, staleDays int) (sitemapAuditEntry, map[string]struct{}, error) {
	e := sitemapAuditEntry{
		Path:           sm.Path,
		LastDownloaded: sm.LastDownloaded,
		Errors:         sm.Errors,
		Warnings:       sm.Warnings,
		Issues:         []string{},
	}
	e.LastDownloadedAgeDays = ageDays(sm.LastDownloaded, now)
	for _, c := range sm.Contents {
		if c != nil && c.Type == "web" {
			e.Submitted += c.Submitted
		}
	}

	var (
		rootKind    sitemap.Kind
		rootEntries int
		fetchErr    *sitemap.FetchError
		urls        []sitemap.URL
		rawEntries  int
	)
	if expand {
		wr, err := client.Walk(ctx, sm.Path, 0)
		if err != nil {
			return e, nil, err
		}
		root := wr.Sitemaps[0] // Walk always lists the root first
		rootKind, rootEntries, fetchErr = root.Kind, root.Entries, root.Error
		urls = wr.URLs
		for _, r := range wr.Sitemaps {
			if r.Kind == sitemap.KindURLSet {
				rawEntries += r.Entries
			}
		}
		for _, r := range wr.Sitemaps[1:] {
			if r.Error != nil {
				e.ChildErrors = append(e.ChildErrors, sitemapChildError{URL: r.URL, Kind: r.Error.Kind, StatusCode: r.Error.StatusCode})
			}
		}
	} else {
		doc, err := client.Fetch(ctx, sm.Path)
		if cerr := ctx.Err(); cerr != nil {
			return e, nil, cerr
		}
		if err != nil {
			fetchErr = asSitemapFetchError(err, sm.Path)
		} else {
			rootKind, rootEntries = doc.Kind, len(doc.Entries)
		}
	}

	var set map[string]struct{}
	if fetchErr != nil {
		e.FetchError = &sitemapFetchErrView{Kind: fetchErr.Kind, StatusCode: fetchErr.StatusCode, Error: fetchErr.Err}
	} else {
		e.Reachable = true
		switch rootKind {
		case sitemap.KindIndex:
			e.Kind = "index"
			n := rootEntries
			e.Children = &n
		default:
			e.Kind = "urlset"
		}
		if expand {
			n := len(urls)
			e.URLs = &n
			e.Entries = &rawEntries
			e.ByType = map[string]int{}
			set = make(map[string]struct{}, len(urls))
			for _, u := range urls {
				e.ByType[classify(u.Loc)]++
				set[u.Loc] = struct{}{}
			}
		}
	}
	e.Issues = deriveSitemapIssues(&e, staleDays)
	return e, set, nil
}

func asSitemapFetchError(err error, url string) *sitemap.FetchError {
	var fe *sitemap.FetchError
	if errors.As(err, &fe) {
		return fe
	}
	return &sitemap.FetchError{URL: url, Kind: "network", Err: err.Error()}
}

// ageDays returns whole days between an RFC 3339 timestamp and now (nil when
// the timestamp is empty or unparseable).
func ageDays(ts string, now time.Time) *int {
	if ts == "" {
		return nil
	}
	t, err := time.Parse(time.RFC3339, ts)
	if err != nil {
		return nil
	}
	d := int(now.Sub(t).Hours() / 24)
	if d < 0 {
		d = 0
	}
	return &d
}

// deriveSitemapIssues lists the issues visible without cross-sitemap context,
// in a stable order.
func deriveSitemapIssues(e *sitemapAuditEntry, staleDays int) []string {
	issues := []string{}
	if !e.Reachable && e.FetchError != nil {
		if e.FetchError.Kind == "http_status" {
			issues = append(issues, fmt.Sprintf("unreachable_http_%d", e.FetchError.StatusCode))
		} else {
			issues = append(issues, "unreachable_"+e.FetchError.Kind)
		}
	}
	if len(e.ChildErrors) > 0 {
		issues = append(issues, "unreachable_children")
	}
	if e.LastDownloadedAgeDays != nil && *e.LastDownloadedAgeDays > staleDays {
		issues = append(issues, "stale_download")
	}
	if e.Errors > 0 {
		issues = append(issues, "has_errors")
	}
	if e.Warnings > 0 {
		issues = append(issues, "has_warnings")
	}
	return issues
}

// overlapPick is, for one sitemap, the other sitemap holding the largest
// share of its URLs.
type overlapPick struct {
	With   int // index of the other sitemap
	Shared int // URLs present in both
	Total  int // URLs in this sitemap
}

// computeOverlaps picks, for each set, the other set containing the largest
// share of its URLs (ties: lower index). Nil or empty sets take no part. The
// result has With == -1 where nothing overlaps.
func computeOverlaps(sets []map[string]struct{}) []overlapPick {
	n := len(sets)
	shared := make([][]int, n)
	for i := range shared {
		shared[i] = make([]int, n)
	}
	for i := 0; i < n; i++ {
		for j := i + 1; j < n; j++ {
			if len(sets[i]) == 0 || len(sets[j]) == 0 {
				continue
			}
			small, big := sets[i], sets[j]
			if len(small) > len(big) {
				small, big = big, small
			}
			c := 0
			for u := range small {
				if _, ok := big[u]; ok {
					c++
				}
			}
			shared[i][j], shared[j][i] = c, c
		}
	}
	picks := make([]overlapPick, n)
	for i := 0; i < n; i++ {
		picks[i] = overlapPick{With: -1, Total: len(sets[i])}
		for j := 0; j < n; j++ {
			s := shared[i][j]
			if j == i || s == 0 {
				continue
			}
			// Total is the same for every j, so the largest share is the largest s.
			if picks[i].With == -1 || s > picks[i].Shared {
				picks[i] = overlapPick{With: j, Shared: s, Total: len(sets[i])}
			}
		}
	}
	return picks
}

// applySitemapOverlaps fills Overlap and the redundant issue from the URL sets.
func applySitemapOverlaps(entries []sitemapAuditEntry, sets []map[string]struct{}) {
	for i, p := range computeOverlaps(sets) {
		if p.With < 0 {
			continue
		}
		share := float64(p.Shared) / float64(p.Total)
		entries[i].Overlap = &sitemapOverlap{
			With:   entries[p.With].Path,
			Shared: p.Shared,
			Share:  math.Round(share*10000) / 10000,
		}
		if p.Shared*100 >= sitemapRedundantPct*p.Total {
			entries[i].Issues = append(entries[i].Issues, "redundant")
		}
	}
}

func summarizeSitemapAudit(entries []sitemapAuditEntry) sitemapAuditSummary {
	s := sitemapAuditSummary{Sitemaps: len(entries), IssuesByKind: map[string]int{}}
	for _, e := range entries {
		if len(e.Issues) > 0 {
			s.WithIssues++
		}
		for _, is := range e.Issues {
			s.IssuesByKind[is]++
		}
	}
	return s
}

func init() {
	sitemapsAuditCmd.Flags().IntVar(&saStaleDays, "stale-days", 30, "flag sitemaps Google last downloaded more than this many days ago")
	sitemapsAuditCmd.Flags().BoolVar(&saExpand, "expand", false, "walk every sitemap: count URLs and page types, find redundant/overlapping sitemaps (slow: downloads them all)")

	sitemapsCmd.AddCommand(sitemapsListCmd, sitemapsGetCmd, sitemapsAuditCmd)
	rootCmd.AddCommand(sitemapsCmd)
}
