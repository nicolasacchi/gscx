package commands

import (
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/nicolasacchi/gscx/internal/pagetype"
	"github.com/nicolasacchi/gscx/internal/stats"
	"github.com/nicolasacchi/gscx/internal/store"
	"github.com/tidwall/gjson"
)

// richJSON builds a richResultsResult fragment: type -> items, each item a
// list of "SEVERITY|message" issues.
func richJSON(types ...richFixtureType) string {
	var det []string
	for _, ty := range types {
		var items []string
		for _, it := range ty.items {
			if len(it) == 0 {
				items = append(items, `{"name":"Unnamed item"}`)
				continue
			}
			var is []string
			for _, i := range it {
				sev, msg, _ := strings.Cut(i, "|")
				is = append(is, fmt.Sprintf(`{"issueMessage":%q,"severity":%q}`, msg, sev))
			}
			items = append(items, `{"name":"n","issues":[`+strings.Join(is, ",")+`]}`)
		}
		det = append(det, fmt.Sprintf(`{"richResultType":%q,"items":[%s]}`, ty.name, strings.Join(items, ",")))
	}
	return `{"verdict":"PASS","detectedItems":[` + strings.Join(det, ",") + `]}`
}

type richFixtureType struct {
	name  string
	items [][]string
}

func rt(name string, items ...[]string) richFixtureType { return richFixtureType{name, items} }

// page builds a parsed page for aggregation tests.
func richTestPage(t *testing.T, u string, indexed bool, fragment string) richPage {
	t.Helper()
	det, err := parseRichResults(fragment)
	if err != nil {
		t.Fatal(err)
	}
	return richPage{Inspection: store.Inspection{URL: u}, PageType: pagetype.MustDefault().Classify(u), Indexed: indexed, Detected: det}
}

func TestParseRichResults(t *testing.T) {
	t.Run("empty fragment", func(t *testing.T) {
		got, err := parseRichResults("")
		if err != nil || got != nil {
			t.Errorf("got %v, %v", got, err)
		}
	})
	t.Run("no detected items", func(t *testing.T) {
		got, err := parseRichResults(`{"verdict":"PASS"}`)
		if err != nil || len(got) != 0 {
			t.Errorf("got %v, %v", got, err)
		}
	})
	t.Run("bad json", func(t *testing.T) {
		if _, err := parseRichResults(`{"detectedItems":[`); err == nil {
			t.Error("want an error")
		}
	})
	t.Run("items, issues and merged duplicate types", func(t *testing.T) {
		got, err := parseRichResults(`{"detectedItems":[
			{"richResultType":"Product snippets","items":[{"name":"a","issues":[{"issueMessage":"Missing field \"review\"","severity":"WARNING"},{"issueMessage":"boom"}]}]},
			{"richResultType":"Breadcrumbs","items":[{"name":"Unnamed item"}]},
			{"richResultType":"Product snippets","items":[{"name":"b"}]},
			{"items":[{"name":"typeless"}]}
		]}`)
		if err != nil {
			t.Fatal(err)
		}
		want := []richDetected{
			{Type: "Product snippets", Items: []richItem{
				{Issues: []richIssue{{"WARNING", `Missing field "review"`}, {"UNSPECIFIED", "boom"}}},
				{},
			}},
			{Type: "Breadcrumbs", Items: []richItem{{}}},
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("got  %+v\nwant %+v", got, want)
		}
	})
}

func TestComputeRichCountsAndWilson(t *testing.T) {
	avail := `WARNING|Missing field "availability"`
	review := `WARNING|Missing field "review"`
	pages := []richPage{
		// Product 1: two Product-snippet items, both missing availability (counted once as a URL issue).
		richTestPage(t, "https://www.example.com/a.html", true,
			richJSON(rt("Product snippets", []string{avail, review}, []string{avail}), rt("Breadcrumbs", nil))),
		// Product 2: availability only, plus a merchant listing with an error.
		richTestPage(t, "https://www.example.com/b.html", true,
			richJSON(rt("Product snippets", []string{avail}), rt("Merchant listings", []string{`ERROR|Missing field "price"`}), rt("Breadcrumbs", nil))),
		// Product 3: clean breadcrumbs only.
		richTestPage(t, "https://www.example.com/c.html", true, richJSON(rt("Breadcrumbs", nil))),
		// Product 4: not indexed, no rich results.
		richTestPage(t, "https://www.example.com/d.html", false, ""),
		richTestPage(t, "https://www.example.com/brands/acme", true, richJSON(rt("Breadcrumbs", nil))),
	}
	overall, by, issues := computeRich(pages)

	p := by["product"]
	if p.N != 4 || p.Indexed != 3 || p.WithRichResults != 3 || *p.WithRichResultsPct != 75 {
		t.Fatalf("product row = %+v", p)
	}
	lo, hi := stats.Wilson(3, 4, stats.Z95)
	if !near(p.WithRichResultsCI.Lo, 100*lo, 0.01) || !near(p.WithRichResultsCI.Hi, 100*hi, 0.01) {
		t.Errorf("with_rich_results_ci95 = %+v, want Wilson(3/4) = [%.2f %.2f]", *p.WithRichResultsCI, 100*lo, 100*hi)
	}

	ps := p.ByRichType["Product snippets"]
	if ps.N != 2 || ps.Pct != 50 || ps.Items != 3 || ps.ItemsIssues != 3 || ps.URLsWithIssue != 2 {
		t.Errorf("Product snippets = %+v", ps)
	}
	if !near(*ps.PctOfIndexed, 66.67, 0.01) {
		t.Errorf("pct_of_indexed = %v, want 66.67 (2 of 3 indexed)", *ps.PctOfIndexed)
	}
	// "availability" is on 2 URLs although it appears on 3 items: URLs, not items.
	if want := map[string]int{"WARNING: " + `Missing field "availability"`: 2, "WARNING: " + `Missing field "review"`: 1}; !reflect.DeepEqual(ps.Issues, want) {
		t.Errorf("Product snippets issues = %v, want %v", ps.Issues, want)
	}
	lo, hi = stats.Wilson(2, 4, stats.Z95)
	if !near(ps.CI95.Lo, 100*lo, 0.01) || !near(ps.CI95.Hi, 100*hi, 0.01) {
		t.Errorf("Product snippets ci95 = %+v, want Wilson(2/4)", ps.CI95)
	}
	bc := p.ByRichType["Breadcrumbs"]
	if bc.N != 3 || bc.Items != 3 || bc.ItemsIssues != 0 || bc.URLsWithIssue != 0 || len(bc.Issues) != 0 {
		t.Errorf("Breadcrumbs = %+v", bc)
	}
	if ml := p.ByRichType["Merchant listings"]; ml.N != 1 || ml.Issues[`ERROR: Missing field "price"`] != 1 {
		t.Errorf("Merchant listings = %+v", ml)
	}

	if overall.N != 5 || overall.WithRichResults != 4 || overall.ByRichType["Breadcrumbs"].N != 4 {
		t.Errorf("overall = %+v", overall)
	}
	if by["brand"].N != 1 || by["brand"].ByRichType["Breadcrumbs"].Pct != 100 {
		t.Errorf("brand = %+v", by["brand"])
	}

	// Issue list: most frequent first, deterministic ties, with both percentages.
	if len(issues) != 3 {
		t.Fatalf("issues = %+v", issues)
	}
	top := issues[0]
	if top.PageType != "product" || top.RichType != "Product snippets" || top.Severity != "WARNING" ||
		top.Message != `Missing field "availability"` || top.URLs != 2 || top.PctOfType != 100 || top.PctOfPages != 50 {
		t.Errorf("top issue = %+v", top)
	}
	if issues[1].URLs != 1 || issues[1].Message != `Missing field "price"` || issues[1].Severity != "ERROR" {
		t.Errorf("second issue = %+v (ties order by page type, rich type, severity, message)", issues[1])
	}
	if issues[2].Message != `Missing field "review"` {
		t.Errorf("third issue = %+v", issues[2])
	}
}

func TestComputeRichEmpty(t *testing.T) {
	overall, by, issues := computeRich(nil)
	if overall.N != 0 || overall.WithRichResultsPct != nil || overall.WithRichResultsCI != nil || len(by) != 0 || len(issues) != 0 {
		t.Errorf("overall=%+v by=%v issues=%v", overall, by, issues)
	}
}

func TestRichPagesReclassifyAndTolerateBadFragment(t *testing.T) {
	cls := pagetype.MustDefault()
	rows := []store.RichInspection{
		{Inspection: store.Inspection{URL: "https://www.example.com/a.html", PageType: "stale-stored-type", Verdict: "PASS"}, RichResults: richJSON(rt("Breadcrumbs", nil))},
		{Inspection: store.Inspection{URL: "https://www.example.com/x.htm", Verdict: "NEUTRAL"}, RichResults: `{"broken`},
	}
	pages := richPages(rows, cls)
	if len(pages) != 2 || pages[0].PageType != "product" || !pages[0].Indexed || len(pages[0].Detected) != 1 {
		t.Errorf("page 0 = %+v", pages[0])
	}
	if pages[1].PageType != "category" || pages[1].Indexed || pages[1].Detected != nil {
		t.Errorf("page 1 = %+v (bad fragment counts as none)", pages[1])
	}
}

func TestFilterRichPages(t *testing.T) {
	avail := `WARNING|Missing field "availability"`
	pages := []richPage{
		richTestPage(t, "https://www.example.com/a.html", true, richJSON(rt("Merchant listings", []string{avail, `WARNING|Missing field "shippingDetails"`}), rt("Breadcrumbs", nil))),
		richTestPage(t, "https://www.example.com/b.html", true, richJSON(rt("Merchant listings", []string{`WARNING|Missing field "description"`}))),
		richTestPage(t, "https://www.example.com/c.html", true, richJSON(rt("Breadcrumbs", nil))),
		richTestPage(t, "https://www.example.com/x.htm", true, richJSON(rt("Merchant listings", []string{avail}))),
	}
	urls := func(rows []richURLRow) []string {
		var out []string
		for _, r := range rows {
			out = append(out, r.URL[len("https://www.example.com/"):])
		}
		return out
	}
	cases := []struct {
		name                 string
		richType, typ, issue string
		want                 []string
	}{
		{"rich type, case-insensitive", "merchant LISTINGS", "", "", []string{"a.html", "b.html", "x.htm"}},
		{"page type", "Merchant listings", "product", "", []string{"a.html", "b.html"}},
		{"issue substring, case-insensitive", "Merchant listings", "", "AVAILABILITY", []string{"a.html", "x.htm"}},
		{"issue with severity prefix", "Merchant listings", "", "warning: missing field \"desc", []string{"b.html"}},
		{"issue and type", "Merchant listings", "category", "availability", []string{"x.htm"}},
		{"issue of another rich type does not match", "Breadcrumbs", "", "availability", nil},
		{"clean rich type", "Breadcrumbs", "", "", []string{"a.html", "c.html"}},
		{"unknown rich type", "Recipes", "", "", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := urls(filterRichPages(pages, tc.richType, tc.typ, tc.issue))
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
	rows := filterRichPages(pages, "Merchant listings", "", "availability")
	if rows[0].RichType != "Merchant listings" || rows[0].Items != 1 || len(rows[0].Issues) != 2 {
		t.Errorf("row = %+v (canonical rich type, item count, all issues of the type)", rows[0])
	}
	if clean := filterRichPages(pages, "Breadcrumbs", "", ""); clean[0].Issues == nil || len(clean[0].Issues) != 0 {
		t.Errorf("a clean URL must have issues: [] (got %#v)", clean[0].Issues)
	}
}

// richRecord builds a stored inspection with a raw result carrying the given
// rich-results fragment ("" = none).
func richRecord(u, verdict string, at time.Time, fragment string) store.Inspection {
	raw := `{"inspectionResultLink":"https://search.google.com/search-console/inspect?u=` + u + `"`
	if fragment != "" {
		raw += `,"richResultsResult":` + fragment
	}
	return store.Inspection{URL: u, InspectedAt: at, Verdict: verdict, RawJSON: raw + `}`}
}

func TestRichEndToEnd(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "gscx.db")
	now := time.Now().UTC()
	avail := `WARNING|Missing field "availability"`
	var recs []store.Inspection
	for i := range 4 {
		frag := richJSON(rt("Breadcrumbs", nil))
		switch i {
		case 0, 1: // p0 is replaced below; p1 misses availability
			frag = richJSON(rt("Product snippets", []string{avail}), rt("Breadcrumbs", nil))
		case 2: // p2 misses review
			frag = richJSON(rt("Product snippets", []string{`WARNING|Missing field "review"`}), rt("Breadcrumbs", nil))
		}
		recs = append(recs, richRecord(pathURL("p%d.html", i), "PASS", now.Add(-time.Duration(i+2)*time.Hour), frag))
	}
	// A re-inspection that dropped Product snippets wins over the older one.
	recs = append(recs, richRecord(pathURL("p0.html"), "PASS", now.Add(-30*time.Minute), richJSON(rt("Breadcrumbs", nil))))
	// Outside the window, failed, other site.
	recs = append(recs, richRecord(pathURL("old.html"), "PASS", now.Add(-60*24*time.Hour), richJSON(rt("Breadcrumbs", nil))))
	recs = append(recs, store.Inspection{URL: pathURL("err.html"), InspectedAt: now.Add(-time.Hour), Error: "boom"})
	recs = append(recs, store.Inspection{Site: "sc-domain:other.com", URL: pathURL("zzz.html"), InspectedAt: now.Add(-time.Hour), Verdict: "PASS", RawJSON: `{"richResultsResult":` + richJSON(rt("Breadcrumbs", nil)) + `}`})
	// A category without rich results and a not-indexed blog post.
	recs = append(recs, richRecord(pathURL("c0.htm"), "PASS", now.Add(-time.Hour), ""))
	recs = append(recs, richRecord(pathURL("blog/x"), "NEUTRAL", now.Add(-time.Hour), ""))
	seedInspections(t, dbPath, recs...)

	out, err := runCLI(t, append([]string{"rich"}, cliBase(dbPath)...)...)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	j := gjson.Parse(out)
	for path, want := range map[string]float64{
		"overall.n": 6, "overall.indexed": 5, "overall.with_rich_results": 4,
		"by_type.product.n": 4, "by_type.product.with_rich_results": 4,
		"by_type.product.by_rich_type.Product snippets.n":                2, // p0 was re-inspected without
		"by_type.product.by_rich_type.Product snippets.pct":              50,
		"by_type.product.by_rich_type.Product snippets.urls_with_issues": 2,
		"by_type.product.by_rich_type.Breadcrumbs.n":                     4,
		"by_type.product.by_rich_type.Breadcrumbs.pct":                   100,
		"by_type.category.n":                 1,
		"by_type.category.with_rich_results": 0,
		"by_type.blog.n":                     1,
		"by_type.blog.indexed":               0,
	} {
		if got := j.Get(path); !got.Exists() || !near(got.Float(), want, 0.01) {
			t.Errorf("%s = %v, want %v", path, got.Raw, want)
		}
	}
	if got := j.Get("by_type.product.by_rich_type.Product snippets.issues").Map(); len(got) != 2 ||
		got[`WARNING: Missing field "availability"`].Int() != 1 || got[`WARNING: Missing field "review"`].Int() != 1 {
		t.Errorf("Product snippets issues = %v", got)
	}
	top := j.Get("issues.0")
	if top.Get("page_type").String() != "product" || top.Get("rich_type").String() != "Product snippets" ||
		top.Get("message").String() != `Missing field "availability"` || top.Get("urls").Int() != 1 {
		t.Errorf("top issue = %s", top.Raw)
	}
	if !strings.Contains(j.Get("note").String(), "Enhancements reports") || j.Get("truncated").Exists() {
		t.Errorf("note/truncated: %s", out)
	}

	// An empty window is valid output with a hint.
	out, err = runCLI(t, append([]string{"rich", "--since", "1m"}, cliBase(dbPath)...)...)
	if err != nil {
		t.Fatal(err)
	}
	j = gjson.Parse(out)
	if j.Get("overall.n").Int() != 0 || !strings.Contains(j.Get("hint").String(), "inspect batch") || len(j.Get("issues").Array()) != 0 {
		t.Errorf("empty window: %s", out)
	}
	if _, err := runCLI(t, append([]string{"rich", "--since", "soon"}, cliBase(dbPath)...)...); err == nil {
		t.Error("bad --since: want an error")
	}

	// --limit caps the issue list and says so.
	out, err = runCLI(t, append([]string{"rich", "--limit", "1"}, cliBase(dbPath)...)...)
	if err != nil {
		t.Fatal(err)
	}
	j = gjson.Parse(out)
	if len(j.Get("issues").Array()) != 1 || !j.Get("truncated.truncated").Bool() || j.Get("truncated.total").Int() != 2 {
		t.Errorf("--limit 1: %s", out)
	}
}

func TestRichURLsEndToEnd(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "gscx.db")
	now := time.Now().UTC()
	avail := `WARNING|Missing field "availability"`
	seedInspections(t, dbPath,
		richRecord(pathURL("a.html"), "PASS", now.Add(-time.Hour), richJSON(rt("Merchant listings", []string{avail}), rt("Breadcrumbs", nil))),
		richRecord(pathURL("b.html"), "PASS", now.Add(-time.Hour), richJSON(rt("Merchant listings", []string{`WARNING|Missing field "description"`}))),
		richRecord(pathURL("c.htm"), "PASS", now.Add(-time.Hour), richJSON(rt("Merchant listings", []string{avail}))),
		richRecord(pathURL("d.html"), "PASS", now.Add(-time.Hour), richJSON(rt("Breadcrumbs", nil))),
		richRecord(pathURL("e.html"), "NEUTRAL", now.Add(-time.Hour), ""),
	)
	run := func(extra ...string) gjson.Result {
		t.Helper()
		out, err := runCLI(t, append(append([]string{"rich", "urls"}, extra...), cliBase(dbPath)...)...)
		if err != nil {
			t.Fatalf("%v\n%s", err, out)
		}
		return gjson.Parse(out)
	}

	j := run("--rich-type", "Merchant listings")
	if j.Get("total").Int() != 3 || len(j.Get("urls").Array()) != 3 || j.Get("filters.rich_type").String() != "Merchant listings" {
		t.Fatalf("by rich type: %s", j.Raw)
	}
	first := j.Get("urls.0")
	if first.Get("url").String() != pathURL("a.html") || first.Get("page_type").String() != "product" ||
		first.Get("rich_type").String() != "Merchant listings" || first.Get("items").Int() != 1 ||
		first.Get("issues.0").String() != `WARNING: Missing field "availability"` {
		t.Errorf("first row = %s", first.Raw)
	}
	if !strings.HasSuffix(first.Get("inspection_link").String(), "u="+pathURL("a.html")) {
		t.Errorf("inspection_link = %q (from the stored raw result)", first.Get("inspection_link"))
	}

	if got := run("--rich-type", "merchant listings", "--issue", "availability"); got.Get("total").Int() != 2 {
		t.Errorf("--issue availability: %s", got.Raw)
	}
	if got := run("--rich-type", "Merchant listings", "--issue", "availability", "--type", "product"); got.Get("total").Int() != 1 ||
		got.Get("filters.type").String() != "product" || got.Get("filters.issue").String() != "availability" {
		t.Errorf("--issue + --type: %s", got.Raw)
	}
	capped := run("--rich-type", "Merchant listings", "--limit", "2")
	if capped.Get("total").Int() != 3 || len(capped.Get("urls").Array()) != 2 || !capped.Get("truncated.truncated").Bool() {
		t.Errorf("--limit 2: %s", capped.Raw)
	}
	unknown := run("--rich-type", "Recipes")
	if unknown.Get("total").Int() != 0 || !strings.Contains(unknown.Get("hint").String(), "Breadcrumbs, Merchant listings") {
		t.Errorf("unknown rich type should list the types seen: %s", unknown.Raw)
	}

	for name, args := range map[string][]string{
		"missing --rich-type": {},
		"unknown --type":      {"--rich-type", "Breadcrumbs", "--type", "nope"},
	} {
		if _, err := runCLI(t, append(append([]string{"rich", "urls"}, args...), cliBase(dbPath)...)...); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
}
