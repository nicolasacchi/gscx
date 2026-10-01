package commands

import (
	"fmt"
	"math"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nicolasacchi/gscx/internal/pagetype"
	"github.com/nicolasacchi/gscx/internal/store"
	"github.com/tidwall/gjson"
)

func near(a, b, eps float64) bool { return math.Abs(a-b) <= eps }

var covNow = time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

// insp builds a stored inspection row inspected at covNow.
func insp(u, verdict, state, lastCrawl string) store.Inspection {
	return store.Inspection{
		URL: u, InspectedAt: covNow, Verdict: verdict, CoverageState: state,
		RobotsTxtState: "ALLOWED", GoogleCanonical: u, UserCanonical: u, LastCrawlTime: lastCrawl,
	}
}

func TestParseSince(t *testing.T) {
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		in      string
		want    time.Time
		wantErr bool
	}{
		{"30d", now.Add(-30 * 24 * time.Hour), false},
		{"2w", now.Add(-14 * 24 * time.Hour), false},
		{"12h", now.Add(-12 * time.Hour), false},
		{"720h30m", now.Add(-720*time.Hour - 30*time.Minute), false},
		{" 7d ", now.Add(-7 * 24 * time.Hour), false},
		{"2026-09-01", time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC), false},
		{"2026-09-01T10:00:00Z", time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC), false},
		{"last week", time.Time{}, true},
		{"", time.Time{}, true},
		{"-5d", time.Time{}, true},
		{"30", time.Time{}, true},
	}
	for _, tc := range cases {
		got, err := parseSince(tc.in, now)
		if (err != nil) != tc.wantErr {
			t.Errorf("parseSince(%q) err = %v, wantErr %v", tc.in, err, tc.wantErr)
			continue
		}
		if !tc.wantErr && !got.Equal(tc.want) {
			t.Errorf("parseSince(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestComputeCoverageSingleType(t *testing.T) {
	cls := pagetype.MustDefault()
	day := func(n int) string { return covNow.Add(-time.Duration(n) * 24 * time.Hour).Format(time.RFC3339) }
	var rows []store.Inspection
	// 8 indexed products crawled 1,2,3,4,10 and (3 of them) 5,6,7 days ago.
	for i, age := range []int{1, 2, 3, 4, 10, 5, 6, 7} {
		rows = append(rows, insp(pathURL("p%d.html", i), "PASS", "Submitted and indexed", day(age)))
	}
	rows = append(rows,
		insp(pathURL("p8.html"), "NEUTRAL", "Crawled - currently not indexed", day(2)),
		insp(pathURL("p9.html"), "NEUTRAL", "Discovered - currently not indexed", ""),
	)
	rows[0].GoogleCanonical = "https://www.example.com/elsewhere.html" // mismatch
	rows[1].RobotsTxtState = "DISALLOWED"
	rows[9].LastCrawlTime = "1970-01-01T00:00:00Z" // epoch placeholder = never crawled

	overall, by := computeCoverage(rows, cls)
	p := by["product"]
	if len(by) != 1 || p == nil {
		t.Fatalf("by_type = %v", by)
	}
	if p.N != 10 || p.Indexed != 8 || *p.IndexedPct != 80 {
		t.Errorf("n=%d indexed=%d pct=%v", p.N, p.Indexed, *p.IndexedPct)
	}
	// Wilson 95% for 8/10 is [49.02, 94.33].
	if !near(p.CI95.Lo, 49.02, 0.05) || !near(p.CI95.Hi, 94.33, 0.05) {
		t.Errorf("ci95 = %+v", *p.CI95)
	}
	if p.CoverageStates["Submitted and indexed"] != 8 || p.CoverageStates["Crawled - currently not indexed"] != 1 ||
		p.CoverageStates["Discovered - currently not indexed"] != 1 {
		t.Errorf("states = %v", p.CoverageStates)
	}
	if p.CanonicalMismatch != 1 || p.RobotsBlocked != 1 || p.NeverCrawled != 1 {
		t.Errorf("mismatch=%d robots=%d never=%d, want 1/1/1", p.CanonicalMismatch, p.RobotsBlocked, p.NeverCrawled)
	}
	// ages of the 9 crawled rows: 1,2,3,4,10,5,6,7,2 -> sorted 1,2,2,3,4,5,6,7,10
	a := p.LastCrawlAgeDays
	if a == nil || a.Median != 4 || a.Max != 10 || !near(a.P90, 7.6, 0.01) {
		t.Errorf("age = %+v, want median 4, p90 7.6, max 10", a)
	}
	if overall.N != 10 || overall.Indexed != 8 {
		t.Errorf("overall = %+v", overall)
	}
}

func pathURL(format string, args ...any) string {
	return "https://www.example.com/" + fmt.Sprintf(format, args...)
}

func TestComputeCoverageMultiTypeAndReclassification(t *testing.T) {
	cls := pagetype.MustDefault()
	rows := []store.Inspection{
		insp("https://www.example.com/a.html", "PASS", "Submitted and indexed", ""),
		insp("https://www.example.com/b.html", "NEUTRAL", "Crawled - currently not indexed", ""),
		insp("https://www.example.com/c.htm", "PASS", "Indexed, not submitted in sitemap", ""),
		insp("https://www.example.com/brands/acme", "PASS", "Submitted and indexed", ""),
		insp("https://www.example.com/blog/post", "FAIL", "Server error (5xx)", ""),
	}
	// A stale stored type must not matter: strata follow the current rules.
	rows[0].PageType = "category"
	overall, by := computeCoverage(rows, cls)

	want := map[string][2]int{"product": {2, 1}, "category": {1, 1}, "brand": {1, 1}, "blog": {1, 0}}
	for typ, nI := range want {
		r := by[typ]
		if r == nil || r.N != nI[0] || r.Indexed != nI[1] {
			t.Errorf("%s = %+v, want n=%d indexed=%d", typ, r, nI[0], nI[1])
		}
	}
	if overall.N != 5 || overall.Indexed != 3 || *overall.IndexedPct != 60 {
		t.Errorf("overall = %+v", overall)
	}
	if overall.NeverCrawled != 5 || overall.LastCrawlAgeDays != nil {
		t.Errorf("no lastCrawlTime anywhere: never=%d age=%v", overall.NeverCrawled, overall.LastCrawlAgeDays)
	}
}

func TestComputeCoverageEmpty(t *testing.T) {
	overall, by := computeCoverage(nil, pagetype.MustDefault())
	if overall.N != 0 || overall.IndexedPct != nil || overall.CI95 != nil || overall.CoverageStates == nil || len(by) != 0 {
		t.Errorf("empty = %+v %v", overall, by)
	}
}

func TestApplyFrame(t *testing.T) {
	cls := pagetype.MustDefault()
	var rows []store.Inspection
	for i := range 40 { // product: 40/40
		rows = append(rows, insp(pathURL("p%d.html", i), "PASS", "Submitted and indexed", ""))
	}
	for i := range 30 { // brand: 24/30
		v := "PASS"
		if i >= 24 {
			v = "NEUTRAL"
		}
		rows = append(rows, insp(pathURL("brands/b%d", i), v, "x", ""))
	}
	overall, by := computeCoverage(rows, cls)
	applyFrame(overall, by, map[string]int{"product": 1000, "brand": 200, "category": 50})

	p := by["product"]
	if *p.FrameURLs != 1000 || p.EstimatedIndexed.Point != 1000 || p.EstimatedIndexed.Hi != 1000 ||
		p.EstimatedIndexed.Lo < 910 || p.EstimatedIndexed.Lo > 914 {
		t.Errorf("product estimate = %+v (frame %d)", p.EstimatedIndexed, *p.FrameURLs)
	}
	b := by["brand"]
	if b.EstimatedIndexed.Point != 160 || b.EstimatedIndexed.Lo >= 160 || b.EstimatedIndexed.Hi <= 160 {
		t.Errorf("brand estimate = %+v, want point 160 inside (lo, hi)", b.EstimatedIndexed)
	}
	c := by["category"]
	if c == nil || c.N != 0 || *c.FrameURLs != 50 || c.EstimatedIndexed != nil || c.IndexedPct != nil {
		t.Errorf("unsampled category = %+v", c)
	}
	o := overall
	if *o.FrameURLs != 1250 || o.EstimatedIndexed.Point != 1160 || *o.UnsampledFrameURLs != 50 {
		t.Errorf("overall frame=%d est=%+v unsampled=%d", *o.FrameURLs, o.EstimatedIndexed, *o.UnsampledFrameURLs)
	}
	if o.EstimatedIndexed.Lo != p.EstimatedIndexed.Lo+b.EstimatedIndexed.Lo || o.EstimatedIndexed.Hi != p.EstimatedIndexed.Hi+b.EstimatedIndexed.Hi {
		t.Errorf("overall bounds are not the sum of the per-type bounds: %+v", o.EstimatedIndexed)
	}
	// 1160 of the 1200 URLs in sampled strata.
	if o.WeightedIndexedPct == nil || !near(*o.WeightedIndexedPct, 96.67, 0.01) {
		t.Errorf("weighted pct = %v", o.WeightedIndexedPct)
	}
}

func TestApplyVS(t *testing.T) {
	cls := pagetype.MustDefault()
	mk := func(pass, total int) *coverageRow {
		var rows []store.Inspection
		for i := range total {
			v := "NEUTRAL"
			if i < pass {
				v = "PASS"
			}
			rows = append(rows, insp(pathURL("p%d.html", i), v, "x", ""))
		}
		o, _ := computeCoverage(rows, cls)
		return o
	}
	cur, prev := mk(9, 10), mk(15, 20) // 90% vs 75%
	applyVS(cur, prev)
	if *cur.VSN != 20 || *cur.VSIndexedPct != 75 || *cur.DeltaPctPoints != 15 {
		t.Errorf("vs = n %v pct %v delta %v", *cur.VSN, *cur.VSIndexedPct, *cur.DeltaPctPoints)
	}
	// Previous window without data: n reported, no rate, no delta.
	cur2 := mk(9, 10)
	applyVS(cur2, mk(0, 0))
	if *cur2.VSN != 0 || cur2.VSIndexedPct != nil || cur2.DeltaPctPoints != nil {
		t.Errorf("empty previous window: %+v", cur2)
	}
	// Type absent from the previous window: untouched.
	cur3 := mk(9, 10)
	applyVS(cur3, nil)
	if cur3.VSN != nil || cur3.DeltaPctPoints != nil {
		t.Errorf("nil previous: %+v", cur3)
	}
}

func TestFilterInspections(t *testing.T) {
	cls := pagetype.MustDefault()
	rows := []store.Inspection{
		insp("https://www.example.com/a.html", "PASS", "Submitted and indexed", ""),
		insp("https://www.example.com/b.html", "NEUTRAL", "Crawled - currently not indexed", ""),
		insp("https://www.example.com/c.htm", "NEUTRAL", "Crawled - currently not indexed", ""),
		insp("https://www.example.com/d.html", "NEUTRAL", "Discovered - currently not indexed", ""),
	}
	cases := []struct {
		name                string
		typ, state, verdict string
		want                int
	}{
		{"no filter", "", "", "", 4},
		{"type", "product", "", "", 3},
		{"state", "", "Crawled - currently not indexed", "", 2},
		{"state is case-insensitive", "", "crawled - currently not indexed", "", 2},
		{"type + state", "product", "Crawled - currently not indexed", "", 1},
		{"verdict", "", "", "neutral", 3},
		{"all three", "product", "Discovered - currently not indexed", "NEUTRAL", 1},
		{"no match", "brand", "", "", 0},
	}
	for _, tc := range cases {
		if got := filterInspections(rows, cls, tc.typ, tc.state, tc.verdict); len(got) != tc.want {
			t.Errorf("%s: %d rows, want %d", tc.name, len(got), tc.want)
		}
	}
}

// --- CLI end to end ----------------------------------------------------------------------

func covRecord(u, verdict, state string, at time.Time) store.Inspection {
	return store.Inspection{
		URL: u, PageType: "ignored-on-purpose", InspectedAt: at, Verdict: verdict, CoverageState: state,
		RobotsTxtState: "ALLOWED", GoogleCanonical: u, UserCanonical: u,
		LastCrawlTime: at.Add(-48 * time.Hour).Format(time.RFC3339), CrawledAs: "MOBILE",
		RawJSON: `{"inspectionResultLink":"https://search.google.com/search-console/inspect?u=` + u + `"}`,
	}
}

func TestCoverageEndToEnd(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "gscx.db")
	now := time.Now().UTC()
	var recs []store.Inspection
	// Current window: 10 products (9 indexed), 5 categories (5 indexed).
	for i := range 10 {
		v, s := "PASS", "Submitted and indexed"
		if i == 0 {
			v, s = "NEUTRAL", "Crawled - currently not indexed"
		}
		recs = append(recs, covRecord(pathURL("p%d.html", i), v, s, now.Add(-time.Duration(i+2)*time.Hour)))
	}
	for i := range 5 {
		recs = append(recs, covRecord(pathURL("c%d.htm", i), "PASS", "Submitted and indexed", now.Add(-time.Hour)))
	}
	// Re-inspection of p0 later flips it to indexed: only the latest counts.
	recs = append(recs, covRecord(pathURL("p0.html"), "PASS", "Submitted and indexed", now.Add(-30*time.Minute)))
	// Previous window (35-45 days ago): 10 products, 5 indexed.
	for i := range 10 {
		v, s := "PASS", "Submitted and indexed"
		if i >= 5 {
			v, s = "NEUTRAL", "Crawled - currently not indexed"
		}
		recs = append(recs, covRecord(pathURL("old%d.html", i), v, s, now.Add(-40*24*time.Hour)))
	}
	// A failed call and another site must be ignored.
	recs = append(recs, store.Inspection{URL: pathURL("err.html"), InspectedAt: now.Add(-time.Hour), Error: "boom"})
	recs = append(recs, store.Inspection{Site: "sc-domain:other.com", URL: pathURL("zzz.html"), InspectedAt: now.Add(-time.Hour), Verdict: "PASS"})
	seedInspections(t, dbPath, recs...)

	out, err := runCLI(t, append([]string{"coverage"}, cliBase(dbPath)...)...)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	j := gjson.Parse(out)
	for path, want := range map[string]float64{
		"overall.n": 15, "overall.indexed": 15, "overall.indexed_pct": 100,
		"by_type.product.n": 10, "by_type.product.indexed": 10, "by_type.product.indexed_pct": 100,
		"by_type.category.n": 5, "by_type.category.indexed_pct": 100,
		"by_type.product.last_crawl_age_days.median": 2, "by_type.product.never_crawled": 0,
	} {
		if got := j.Get(path); !got.Exists() || !near(got.Float(), want, 0.01) {
			t.Errorf("%s = %v, want %v", path, got.Raw, want)
		}
	}
	if !strings.Contains(j.Get("note").String(), "not the Page indexing report") {
		t.Errorf("note = %q", j.Get("note"))
	}
	if j.Get("by_type.product.frame_urls").Exists() || j.Get("vs_window").Exists() {
		t.Errorf("--frame / --vs fields present without flags: %s", out)
	}
	if j.Get("window.days").Float() != 30 {
		t.Errorf("window = %s", j.Get("window").Raw)
	}

	// --vs: the same-length window ending 30 days ago holds the old products.
	vsDate := now.Add(-30 * 24 * time.Hour).Format("2006-01-02")
	out, err = runCLI(t, append([]string{"coverage", "--vs", vsDate}, cliBase(dbPath)...)...)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	j = gjson.Parse(out)
	if got := j.Get("by_type.product.vs_n").Int(); got != 10 {
		t.Errorf("vs_n = %d, want 10\n%s", got, out)
	}
	if got := j.Get("by_type.product.delta_pct_points").Float(); !near(got, 50, 0.01) {
		t.Errorf("delta_pct_points = %v, want +50 (100%% vs 50%%)", got)
	}
	if j.Get("by_type.category.delta_pct_points").Exists() {
		t.Error("category had no previous data: no delta expected")
	}
	if !j.Get("vs_window.to").Exists() {
		t.Errorf("vs_window missing: %s", out)
	}

	// A narrow window sees only the last hours.
	out, err = runCLI(t, append([]string{"coverage", "--since", "45m"}, cliBase(dbPath)...)...)
	if err != nil {
		t.Fatal(err)
	}
	j = gjson.Parse(out)
	if j.Get("overall.n").Int() != 1 || j.Get("by_type.product.n").Int() != 1 {
		t.Errorf("--since 45m: %s", out)
	}

	// Empty window: valid output with a hint, not an error.
	out, err = runCLI(t, append([]string{"coverage", "--since", "1m"}, cliBase(dbPath)...)...)
	if err != nil {
		t.Fatal(err)
	}
	j = gjson.Parse(out)
	if j.Get("overall.n").Int() != 0 || !strings.Contains(j.Get("hint").String(), "inspect batch") {
		t.Errorf("empty window: %s", out)
	}
	if _, err := runCLI(t, append([]string{"coverage", "--since", "soon"}, cliBase(dbPath)...)...); err == nil {
		t.Error("bad --since: want an error")
	}
}

func TestCoverageURLsEndToEnd(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "gscx.db")
	now := time.Now().UTC()
	seedInspections(t, dbPath,
		covRecord(pathURL("a.html"), "PASS", "Submitted and indexed", now.Add(-time.Hour)),
		covRecord(pathURL("b.html"), "NEUTRAL", "Crawled - currently not indexed", now.Add(-time.Hour)),
		covRecord(pathURL("c.html"), "NEUTRAL", "Crawled - currently not indexed", now.Add(-time.Hour)),
		covRecord(pathURL("d.htm"), "NEUTRAL", "Crawled - currently not indexed", now.Add(-time.Hour)),
		covRecord(pathURL("e.html"), "NEUTRAL", "Discovered - currently not indexed", now.Add(-time.Hour)),
	)
	run := func(extra ...string) gjson.Result {
		t.Helper()
		out, err := runCLI(t, append(append([]string{"coverage", "urls"}, extra...), cliBase(dbPath)...)...)
		if err != nil {
			t.Fatalf("%v\n%s", err, out)
		}
		return gjson.Parse(out)
	}

	j := run("--type", "product", "--state", "Crawled - currently not indexed")
	if j.Get("total").Int() != 2 || len(j.Get("urls").Array()) != 2 {
		t.Fatalf("product + state: %s", j.Raw)
	}
	first := j.Get("urls.0")
	if first.Get("url").String() != pathURL("b.html") || first.Get("page_type").String() != "product" ||
		first.Get("verdict").String() != "NEUTRAL" || first.Get("coverage_state").String() != "Crawled - currently not indexed" {
		t.Errorf("first row = %s", first.Raw)
	}
	if !strings.HasSuffix(first.Get("inspection_link").String(), "u="+pathURL("b.html")) {
		t.Errorf("inspection_link = %q (should come from the stored raw result)", first.Get("inspection_link"))
	}
	if j.Get("filters.type").String() != "product" || j.Get("truncated").Exists() {
		t.Errorf("filters/truncated: %s", j.Raw)
	}

	if got := run("--verdict", "NEUTRAL"); got.Get("total").Int() != 4 {
		t.Errorf("--verdict NEUTRAL total = %d", got.Get("total").Int())
	}
	if got := run(); got.Get("total").Int() != 5 || got.Get("filters").Exists() {
		t.Errorf("unfiltered: %s", got.Raw)
	}

	// Cap: total stays the full count, the list is cut, truncated explains.
	capped := run("--verdict", "NEUTRAL", "--limit", "2")
	if capped.Get("total").Int() != 4 || len(capped.Get("urls").Array()) != 2 ||
		!capped.Get("truncated.truncated").Bool() || capped.Get("truncated.shown").Int() != 2 || capped.Get("truncated.total").Int() != 4 {
		t.Errorf("--limit 2: %s", capped.Raw)
	}

	if _, err := runCLI(t, append([]string{"coverage", "urls", "--type", "nope"}, cliBase(dbPath)...)...); err == nil {
		t.Error("unknown --type: want an error")
	}
}
