package commands

import (
	"encoding/json"
	"fmt"
	"math"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/nicolasacchi/gscx/internal/gsc"
	"github.com/nicolasacchi/gscx/internal/output"
	"github.com/nicolasacchi/gscx/internal/pagetype"
	"github.com/nicolasacchi/gscx/internal/sitemap"
	"github.com/nicolasacchi/gscx/internal/stats"
	"github.com/nicolasacchi/gscx/internal/store"
	"github.com/spf13/cobra"
	"github.com/tidwall/gjson"
)

const richNote = "Sample-based, from the stored URL inspections (no API calls): it mirrors what the Search Console " +
	"Enhancements reports (Products, Merchant listings, Breadcrumbs, Review snippets) show for the sampled URLs, " +
	"and those reports have no API. Each URL counts once, with its latest successful inspection in the window; " +
	"page types are re-derived from the current page_types rules. n counts every sampled URL of the type, but " +
	"Google reports rich results only for pages it has indexed, so 'indexed' (verdict PASS) is the number of URLs " +
	"that could carry any; pct is over n, pct_of_indexed over the indexed URLs. With --frame, estimated_urls scales pct " +
	"to the sitemap URLs of the type: unlike the Enhancements report totals (which Google samples), it estimates how " +
	"many pages actually carry each rich result. 'issues' counts URLs, not items: " +
	"each issue is counted once per URL. In the top-level issues list, pct_of_type is the share of the page type's " +
	"URLs carrying that rich type, pct_of_pages the share of all the page type's sampled URLs."

var (
	flagRichSince string

	flagRichURLsType     string
	flagRichURLsRichType string
	flagRichURLsIssue    string
	flagRichURLsSince    string
)

var richCmd = &cobra.Command{
	Use:   "rich",
	Short: "Rich-result coverage and issues per page type, from stored inspections",
	Long: `Reads the richResultsResult of the stored URL inspections (see 'gscx inspect
batch') and reports, per page type, how many sampled URLs carry each rich
result type (Product snippets, Merchant listings, Breadcrumbs, Review snippets,
…) with a 95% Wilson interval, and which structured-data issues Google flagged
(e.g. WARNING: Missing field "availability") and on how many URLs.

The Search Console Enhancements reports have no API; this is a sample-based
view of the same data. Reads the local cache only (no API calls).
Each URL counts once, with its latest successful inspection in the window.

--since is a duration (30d, 12h, 2w) or a date (2026-09-01); the window ends now.`,
	Example: `  gscx rich
  gscx rich --since 7d --jq 'by_type.product.by_rich_type'
  gscx rich --jq 'issues'`,
	Args: cobra.NoArgs,
	RunE: runRich,
}

var richURLsCmd = &cobra.Command{
	Use:   "urls",
	Short: "List the stored URLs behind a rich-result number",
	Long: `Drill-down for 'gscx rich': the URLs whose latest inspection in the window
detected --rich-type (matched case-insensitively), optionally only those with
an issue containing --issue and/or of one page type, e.g.

  gscx rich urls --rich-type "Merchant listings" --issue availability --type product`,
	Args: cobra.NoArgs,
	RunE: runRichURLs,
}

var (
	flagRichFrame    bool
	flagRichSitemaps []string
)

// applyRichFrame scales each sampled rich-result rate to the number of sitemap
// URLs of its page type. Search Console's Enhancements reports count only a
// sample of items, so this estimate — not the report total — answers "how
// many pages carry this rich result". The overall row sums the per-type
// estimates (bounds are sums of per-type bounds, hence conservative).
func applyRichFrame(overall *richRow, by map[string]*richRow, frameByType map[string]int) {
	sum := map[string]*estimate{}
	frameTotal := 0
	for t, size := range frameByType {
		frameTotal += size
		row := by[t]
		if row == nil || row.N == 0 {
			continue
		}
		n := size
		row.FrameURLs = &n
		for rt, r := range row.ByRichType {
			e := &estimate{
				Point: int(math.Round(r.Pct / 100 * float64(size))),
				Lo:    int(math.Round(r.CI95.Lo / 100 * float64(size))),
				Hi:    int(math.Round(r.CI95.Hi / 100 * float64(size))),
			}
			r.EstimatedURLs = e
			acc := sum[rt]
			if acc == nil {
				acc = &estimate{}
				sum[rt] = acc
			}
			acc.Point += e.Point
			acc.Lo += e.Lo
			acc.Hi += e.Hi
		}
	}
	if overall == nil {
		return
	}
	overall.FrameURLs = &frameTotal
	for rt, e := range sum {
		if r := overall.ByRichType[rt]; r != nil {
			r.EstimatedURLs = e
		}
	}
}

func init() {
	richCmd.Flags().StringVar(&flagRichSince, "since", "30d", "window start: a duration (30d, 12h, 2w) or a date (2026-09-01)")
	richCmd.Flags().BoolVar(&flagRichFrame, "frame", false, "walk the sitemaps (network) and estimate how many URLs of each page type carry each rich result")
	richCmd.Flags().StringArrayVar(&flagRichSitemaps, "sitemap", nil, "sitemap URL for --frame (repeatable; default: config sitemaps)")

	u := richURLsCmd.Flags()
	u.StringVar(&flagRichURLsRichType, "rich-type", "", `rich result type, e.g. "Merchant listings" (required)`)
	u.StringVar(&flagRichURLsIssue, "issue", "", `only URLs with an issue whose text contains this, e.g. "availability" (case-insensitive)`)
	u.StringVar(&flagRichURLsType, "type", "", "page type")
	u.StringVar(&flagRichURLsSince, "since", "30d", "window start: a duration (30d) or a date")

	richCmd.AddCommand(richURLsCmd)
	rootCmd.AddCommand(richCmd)
}

// --- parsing ---------------------------------------------------------------------

// richIssue is one structured-data issue on an item.
type richIssue struct{ Severity, Message string }

// key renders the issue as "SEVERITY: message", the label used in outputs.
func (i richIssue) key() string { return i.Severity + ": " + i.Message }

type richItem struct {
	Issues []richIssue
}

// richDetected is one rich result type found on a page with its items.
type richDetected struct {
	Type  string
	Items []richItem
}

// richPage is one URL's latest inspection reduced to what 'rich' needs.
type richPage struct {
	store.Inspection
	PageType string
	Indexed  bool
	// Detected holds one entry per rich result type, in the order Google
	// listed them. Nil when the result carries none.
	Detected []richDetected
}

// parseRichResults decodes the richResultsResult fragment of a stored
// inspection. An empty fragment means "no rich results detected". A type that
// Google lists twice is merged. Issues without a severity are labelled
// UNSPECIFIED.
func parseRichResults(raw string) ([]richDetected, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	var v struct {
		DetectedItems []struct {
			RichResultType string `json:"richResultType"`
			Items          []struct {
				Issues []struct {
					IssueMessage string `json:"issueMessage"`
					Severity     string `json:"severity"`
				} `json:"issues"`
			} `json:"items"`
		} `json:"detectedItems"`
	}
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		return nil, fmt.Errorf("parse richResultsResult: %w", err)
	}
	var out []richDetected
	index := map[string]int{}
	for _, d := range v.DetectedItems {
		if d.RichResultType == "" {
			continue
		}
		i, ok := index[d.RichResultType]
		if !ok {
			i = len(out)
			index[d.RichResultType] = i
			out = append(out, richDetected{Type: d.RichResultType})
		}
		for _, it := range d.Items {
			ri := richItem{}
			for _, is := range it.Issues {
				sev := is.Severity
				if sev == "" {
					sev = "UNSPECIFIED"
				}
				ri.Issues = append(ri.Issues, richIssue{Severity: sev, Message: is.IssueMessage})
			}
			out[i].Items = append(out[i].Items, ri)
		}
	}
	return out, nil
}

// richPages classifies the rows and parses their rich results. A row whose
// fragment cannot be decoded counts as having none (and is logged).
func richPages(rows []store.RichInspection, cls *pagetype.Classifier) []richPage {
	out := make([]richPage, 0, len(rows))
	for _, r := range rows {
		det, err := parseRichResults(r.RichResults)
		if err != nil {
			logf("%s: %v", r.URL, err)
		}
		out = append(out, richPage{
			Inspection: r.Inspection,
			PageType:   cls.Classify(r.URL),
			Indexed:    r.Verdict == "PASS",
			Detected:   det,
		})
	}
	return out
}

// --- aggregation -----------------------------------------------------------------

type richTypeRow struct {
	N             int      `json:"n"`
	Pct           float64  `json:"pct"`
	CI95          ci95     `json:"ci95"`
	PctOfIndexed  *float64 `json:"pct_of_indexed,omitempty"`
	Items         int      `json:"items"`
	ItemsIssues   int      `json:"items_with_issues"`
	URLsWithIssue int      `json:"urls_with_issues"`
	// Issues maps "SEVERITY: message" to the number of URLs carrying it.
	Issues map[string]int `json:"issues"`
	// EstimatedURLs scales Pct and its interval to the number of sitemap URLs
	// of the page type (--frame): how many pages carry this rich result.
	EstimatedURLs *estimate `json:"estimated_urls,omitempty"`
}

type richRow struct {
	N                  int                     `json:"n"`
	Indexed            int                     `json:"indexed"`
	WithRichResults    int                     `json:"with_rich_results"`
	WithRichResultsPct *float64                `json:"with_rich_results_pct,omitempty"`
	WithRichResultsCI  *ci95                   `json:"with_rich_results_ci95,omitempty"`
	ByRichType         map[string]*richTypeRow `json:"by_rich_type"`
	FrameURLs          *int                    `json:"frame_urls,omitempty"`
}

type richIssueRow struct {
	PageType   string  `json:"page_type"`
	RichType   string  `json:"rich_type"`
	Severity   string  `json:"severity"`
	Message    string  `json:"message"`
	URLs       int     `json:"urls"`
	PctOfType  float64 `json:"pct_of_type"`
	PctOfPages float64 `json:"pct_of_pages"`
}

type richTypeAcc struct {
	urls, items, itemsIssues, urlsIssues int
	indexedURLs                          int
	issues                               map[string]int
}

type richAcc struct {
	n, indexed, withRich int
	types                map[string]*richTypeAcc
}

func newRichAcc() *richAcc { return &richAcc{types: map[string]*richTypeAcc{}} }

func (a *richAcc) add(p richPage) {
	a.n++
	if p.Indexed {
		a.indexed++
	}
	if len(p.Detected) > 0 {
		a.withRich++
	}
	for _, d := range p.Detected {
		t := a.types[d.Type]
		if t == nil {
			t = &richTypeAcc{issues: map[string]int{}}
			a.types[d.Type] = t
		}
		t.urls++
		if p.Indexed {
			t.indexedURLs++
		}
		urlIssues := map[string]bool{}
		for _, it := range d.Items {
			t.items++
			if len(it.Issues) > 0 {
				t.itemsIssues++
			}
			for _, is := range it.Issues {
				urlIssues[is.key()] = true
			}
		}
		if len(urlIssues) > 0 {
			t.urlsIssues++
		}
		for k := range urlIssues {
			t.issues[k]++
		}
	}
}

func (a *richAcc) row() *richRow {
	row := &richRow{N: a.n, Indexed: a.indexed, WithRichResults: a.withRich, ByRichType: map[string]*richTypeRow{}}
	if a.n > 0 {
		pct := round(100*float64(a.withRich)/float64(a.n), 2)
		lo, hi := stats.Wilson(a.withRich, a.n, stats.Z95)
		row.WithRichResultsPct = &pct
		row.WithRichResultsCI = &ci95{Lo: round(100*lo, 2), Hi: round(100*hi, 2)}
	}
	for name, t := range a.types {
		lo, hi := stats.Wilson(t.urls, a.n, stats.Z95)
		tr := &richTypeRow{
			N: t.urls, Pct: round(100*float64(t.urls)/float64(a.n), 2),
			CI95:  ci95{Lo: round(100*lo, 2), Hi: round(100*hi, 2)},
			Items: t.items, ItemsIssues: t.itemsIssues, URLsWithIssue: t.urlsIssues,
			Issues: t.issues,
		}
		if a.indexed > 0 {
			p := round(100*float64(t.urls)/float64(a.indexed), 2)
			tr.PctOfIndexed = &p
		}
		row.ByRichType[name] = tr
	}
	return row
}

// computeRich aggregates pages per page type and pooled, and lists every
// (page type, rich type, issue) with the number of URLs carrying it, most
// frequent first.
func computeRich(pages []richPage) (*richRow, map[string]*richRow, []richIssueRow) {
	all := newRichAcc()
	per := map[string]*richAcc{}
	for _, p := range pages {
		if per[p.PageType] == nil {
			per[p.PageType] = newRichAcc()
		}
		per[p.PageType].add(p)
		all.add(p)
	}
	by := make(map[string]*richRow, len(per))
	var issues []richIssueRow
	for typ, a := range per {
		by[typ] = a.row()
		for rich, t := range a.types {
			for k, n := range t.issues {
				sev, msg, _ := strings.Cut(k, ": ")
				issues = append(issues, richIssueRow{
					PageType: typ, RichType: rich, Severity: sev, Message: msg, URLs: n,
					PctOfType:  round(100*float64(n)/float64(t.urls), 2),
					PctOfPages: round(100*float64(n)/float64(a.n), 2),
				})
			}
		}
	}
	sort.Slice(issues, func(i, j int) bool {
		a, b := issues[i], issues[j]
		switch {
		case a.URLs != b.URLs:
			return a.URLs > b.URLs
		case a.PageType != b.PageType:
			return a.PageType < b.PageType
		case a.RichType != b.RichType:
			return a.RichType < b.RichType
		case a.Severity != b.Severity:
			return a.Severity < b.Severity
		}
		return a.Message < b.Message
	})
	return all.row(), by, issues
}

// --- gscx rich ------------------------------------------------------------------

type richOut struct {
	Site    string              `json:"site"`
	Note    string              `json:"note"`
	Hint    string              `json:"hint,omitempty"`
	Window  windowOut           `json:"window"`
	Overall *richRow            `json:"overall"`
	ByType  map[string]*richRow `json:"by_type"`
	Issues  []richIssueRow      `json:"issues"`
	Trunc   *output.Truncation  `json:"truncated,omitempty"`
}

func runRich(cmd *cobra.Command, _ []string) error {
	ctx := cmd.Context()
	site := siteURL()
	now := time.Now()

	from, err := parseSince(flagRichSince, now)
	if err != nil {
		return err
	}
	if !from.Before(now) {
		return gsc.NewValidationError("--since must be in the past", "")
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

	rows, err := st.LatestPerURLRich(ctx, site, from, now)
	if err != nil {
		return err
	}
	overall, by, issues := computeRich(richPages(rows, cls))
	if flagRichFrame {
		sitemaps := flagRichSitemaps
		if len(sitemaps) == 0 {
			sitemaps = cfg.Sitemaps
		}
		fr, err := loadFrame(ctx, site, cls, sitemap.NewClient(userAgent()), sitemaps, "", nil)
		if err != nil {
			return err
		}
		sizes := map[string]int{}
		for t, urls := range fr.Groups {
			sizes[t] = len(urls)
		}
		applyRichFrame(overall, by, sizes)
	}
	capped, trunc := output.Cap(issues, flagLimit)
	out := richOut{
		Site: site, Note: richNote, Window: timeWindow{From: from, To: now}.out(),
		Overall: overall, ByType: by, Issues: capped, Trunc: trunc,
	}
	if out.Issues == nil {
		out.Issues = []richIssueRow{}
	}
	if overall.N == 0 {
		out.Hint = "no successful inspections stored in this window; run 'gscx inspect batch' first"
	}
	return emit(cmd, out)
}

// --- gscx rich urls ------------------------------------------------------------

type richURLRow struct {
	URL            string   `json:"url"`
	PageType       string   `json:"page_type"`
	RichType       string   `json:"rich_type"`
	Items          int      `json:"items"`
	Issues         []string `json:"issues"`
	InspectedAt    string   `json:"inspected_at"`
	InspectionLink string   `json:"inspection_link,omitempty"`

	id int64
}

type richURLsOut struct {
	Site    string             `json:"site"`
	Window  windowOut          `json:"window"`
	Filters map[string]string  `json:"filters"`
	Hint    string             `json:"hint,omitempty"`
	Total   int                `json:"total"`
	URLs    []richURLRow       `json:"urls"`
	Trunc   *output.Truncation `json:"truncated,omitempty"`
}

// richTypesSeen lists the rich result types present on pages, sorted.
func richTypesSeen(pages []richPage) []string {
	seen := map[string]bool{}
	for _, p := range pages {
		for _, d := range p.Detected {
			seen[d.Type] = true
		}
	}
	out := make([]string, 0, len(seen))
	for t := range seen {
		out = append(out, t)
	}
	sort.Strings(out)
	return out
}

// filterRichPages keeps the pages that detected richType (case-insensitive)
// and, when set, are of page type typ and carry an issue (on that rich type)
// whose "SEVERITY: message" text contains issue (case-insensitive).
func filterRichPages(pages []richPage, richType, typ, issue string) []richURLRow {
	issue = strings.ToLower(issue)
	var out []richURLRow
	for _, p := range pages {
		if typ != "" && p.PageType != typ {
			continue
		}
		for _, d := range p.Detected {
			if !strings.EqualFold(d.Type, richType) {
				continue
			}
			var keys []string
			for _, it := range d.Items {
				for _, is := range it.Issues {
					if k := is.key(); !slices.Contains(keys, k) {
						keys = append(keys, k)
					}
				}
			}
			if issue != "" && !slices.ContainsFunc(keys, func(k string) bool { return strings.Contains(strings.ToLower(k), issue) }) {
				continue
			}
			if keys == nil {
				keys = []string{}
			}
			out = append(out, richURLRow{
				URL: p.URL, PageType: p.PageType, RichType: d.Type, Items: len(d.Items), Issues: keys,
				InspectedAt: p.InspectedAt.UTC().Format(time.RFC3339), id: p.ID,
			})
		}
	}
	return out
}

func runRichURLs(cmd *cobra.Command, _ []string) error {
	ctx := cmd.Context()
	site := siteURL()
	now := time.Now()

	if strings.TrimSpace(flagRichURLsRichType) == "" {
		return gsc.NewValidationError("--rich-type is required", `e.g. --rich-type "Merchant listings"; 'gscx rich' lists the types found`)
	}
	from, err := parseSince(flagRichURLsSince, now)
	if err != nil {
		return err
	}
	cls, err := classifier()
	if err != nil {
		return err
	}
	if flagRichURLsType != "" && !slices.Contains(cls.Names(), flagRichURLsType) {
		return gsc.NewValidationError(fmt.Sprintf("unknown page type %q", flagRichURLsType), "known types: "+strings.Join(cls.Names(), ", "))
	}
	st, err := openStore(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()

	rows, err := st.LatestPerURLRich(ctx, site, from, now)
	if err != nil {
		return err
	}
	pages := richPages(rows, cls)
	matched := filterRichPages(pages, flagRichURLsRichType, flagRichURLsType, flagRichURLsIssue)
	capped, trunc := output.Cap(matched, flagLimit)

	list := make([]richURLRow, 0, len(capped))
	for _, r := range capped {
		raw, err := st.RawJSON(ctx, r.id)
		if err != nil {
			return err
		}
		r.InspectionLink = gjson.Get(raw, "inspectionResultLink").String()
		list = append(list, r)
	}

	filters := map[string]string{"rich_type": flagRichURLsRichType}
	for k, v := range map[string]string{"issue": flagRichURLsIssue, "type": flagRichURLsType} {
		if v != "" {
			filters[k] = v
		}
	}
	out := richURLsOut{
		Site: site, Window: timeWindow{From: from, To: now}.out(),
		Filters: filters, Total: len(matched), URLs: list, Trunc: trunc,
	}
	if len(matched) == 0 {
		if seen := richTypesSeen(pages); len(seen) > 0 {
			out.Hint = "rich types found in this window: " + strings.Join(seen, ", ")
		} else {
			out.Hint = "no rich results found in this window; run 'gscx inspect batch' first or widen --since"
		}
	}
	return emit(cmd, out)
}
