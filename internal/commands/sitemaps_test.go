package commands

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/nicolasacchi/gscx/internal/pagetype"
	"github.com/nicolasacchi/gscx/internal/sitemap"
	searchconsole "google.golang.org/api/searchconsole/v1"
)

func TestToSitemapViewDropsIndexed(t *testing.T) {
	sm := &searchconsole.WmxSitemap{
		Path:            "https://x.test/sitemap.xml",
		LastSubmitted:   "2025-01-01T00:00:00Z",
		LastDownloaded:  "2026-09-01T10:00:00.123Z",
		IsPending:       false,
		IsSitemapsIndex: true,
		Type:            "sitemap",
		Warnings:        3,
		Errors:          1,
		Contents: []*searchconsole.WmxSitemapContent{
			{Type: "web", Submitted: 1000, Indexed: 0},
			{Type: "image", Submitted: 50, Indexed: 0},
			nil,
		},
	}
	b, err := json.Marshal(toSitemapView(sm))
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "indexed") {
		t.Errorf("indexed must be removed: %s", b)
	}
	want := map[string]any{
		"path": "https://x.test/sitemap.xml", "last_submitted": "2025-01-01T00:00:00Z",
		"last_downloaded": "2026-09-01T10:00:00.123Z", "is_pending": false, "is_sitemaps_index": true,
		"type": "sitemap", "warnings": 3.0, "errors": 1.0,
		"contents": []any{
			map[string]any{"type": "web", "submitted": 1000.0},
			map[string]any{"type": "image", "submitted": 50.0},
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got  %v\nwant %v", got, want)
	}

	// a sitemap with no contents prints [] not null
	b, _ = json.Marshal(toSitemapView(&searchconsole.WmxSitemap{Path: "p"}))
	if !strings.Contains(string(b), `"contents":[]`) {
		t.Errorf("empty contents = %s", b)
	}
}

func TestSitemapViewsSorted(t *testing.T) {
	got := sitemapViews([]*searchconsole.WmxSitemap{{Path: "b"}, nil, {Path: "a"}})
	if len(got) != 2 || got[0].Path != "a" || got[1].Path != "b" {
		t.Errorf("got %+v", got)
	}
}

func TestAgeDays(t *testing.T) {
	now := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		ts   string
		want int
		nil  bool
	}{
		{"2026-09-30T00:00:00Z", 0, false},
		{"2026-09-01T12:00:00.250Z", 28, false},
		{"2025-01-29T11:08:29.404Z", 609, false},
		{"2026-10-02T00:00:00Z", 0, false}, // future clamps to 0
		{"", 0, true},
		{"not a date", 0, true},
	}
	for _, tc := range cases {
		got := ageDays(tc.ts, now)
		switch {
		case tc.nil && got != nil:
			t.Errorf("ageDays(%q) = %d, want nil", tc.ts, *got)
		case !tc.nil && (got == nil || *got != tc.want):
			t.Errorf("ageDays(%q) = %v, want %d", tc.ts, got, tc.want)
		}
	}
}

func intp(n int) *int { return &n }

func TestDeriveSitemapIssues(t *testing.T) {
	cases := []struct {
		name string
		e    sitemapAuditEntry
		want []string
	}{
		{"healthy", sitemapAuditEntry{Reachable: true, LastDownloadedAgeDays: intp(2)}, []string{}},
		{"dns", sitemapAuditEntry{FetchError: &sitemapFetchErrView{Kind: "dns"}, LastDownloadedAgeDays: intp(608)}, []string{"unreachable_dns", "stale_download"}},
		{"http 404", sitemapAuditEntry{FetchError: &sitemapFetchErrView{Kind: "http_status", StatusCode: 404}}, []string{"unreachable_http_404"}},
		{"http 503", sitemapAuditEntry{FetchError: &sitemapFetchErrView{Kind: "http_status", StatusCode: 503}}, []string{"unreachable_http_503"}},
		{"timeout", sitemapAuditEntry{FetchError: &sitemapFetchErrView{Kind: "timeout"}}, []string{"unreachable_timeout"}},
		{"parse", sitemapAuditEntry{FetchError: &sitemapFetchErrView{Kind: "parse"}}, []string{"unreachable_parse"}},
		{"too large", sitemapAuditEntry{FetchError: &sitemapFetchErrView{Kind: "too_large"}}, []string{"unreachable_too_large"}},
		{"stale boundary not flagged", sitemapAuditEntry{Reachable: true, LastDownloadedAgeDays: intp(30)}, []string{}},
		{"stale", sitemapAuditEntry{Reachable: true, LastDownloadedAgeDays: intp(31)}, []string{"stale_download"}},
		{"never downloaded is not stale", sitemapAuditEntry{Reachable: true}, []string{}},
		{"errors+warnings", sitemapAuditEntry{Reachable: true, Errors: 2, Warnings: 15}, []string{"has_errors", "has_warnings"}},
		{"child failures", sitemapAuditEntry{Reachable: true, ChildErrors: []sitemapChildError{{URL: "u"}}}, []string{"unreachable_children"}},
		{"everything", sitemapAuditEntry{FetchError: &sitemapFetchErrView{Kind: "dns"}, LastDownloadedAgeDays: intp(99), Errors: 1, Warnings: 1},
			[]string{"unreachable_dns", "stale_download", "has_errors", "has_warnings"}},
	}
	for _, tc := range cases {
		e := tc.e
		if got := deriveSitemapIssues(&e, 30); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: got %v, want %v", tc.name, got, tc.want)
		}
	}
}

func urlSet(urls ...string) map[string]struct{} {
	s := make(map[string]struct{}, len(urls))
	for _, u := range urls {
		s[u] = struct{}{}
	}
	return s
}

func TestComputeOverlaps(t *testing.T) {
	sets := []map[string]struct{}{
		urlSet("a", "b", "c", "d"),      // 0: fully inside 1
		urlSet("a", "b", "c", "d", "e"), // 1: superset of 0, shares 1 with 2
		urlSet("e", "x"),                // 2: shares e with 1
		nil,                             // 3: unreachable / not expanded
		urlSet("q"),                     // 4: disjoint
		urlSet(),                        // 5: empty
	}
	got := computeOverlaps(sets)
	want := []overlapPick{
		{With: 1, Shared: 4, Total: 4},
		{With: 0, Shared: 4, Total: 5},
		{With: 1, Shared: 1, Total: 2},
		{With: -1, Shared: 0, Total: 0},
		{With: -1, Shared: 0, Total: 1},
		{With: -1, Shared: 0, Total: 0},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got  %+v\nwant %+v", got, want)
	}

	// ties go to the lower index
	got = computeOverlaps([]map[string]struct{}{urlSet("a", "b"), urlSet("a"), urlSet("b")})
	if got[0].With != 1 || got[0].Shared != 1 {
		t.Errorf("tie = %+v", got[0])
	}
}

func TestApplySitemapOverlaps(t *testing.T) {
	// 100 URLs in "child", all inside "parent" (120): child is redundant, parent
	// shares 100/120 = 83.33% with child: overlapping but not redundant.
	var parent, child []string
	for i := 0; i < 120; i++ {
		parent = append(parent, fmt.Sprintf("u%d", i))
		if i < 100 {
			child = append(child, fmt.Sprintf("u%d", i))
		}
	}
	// 95 of 100 shared: exactly at the threshold, redundant. 94 of 100: not.
	var edge95, edge94 []string
	for i := 0; i < 100; i++ {
		edge95 = append(edge95, fmt.Sprintf("u%d", i))
		edge94 = append(edge94, fmt.Sprintf("u%d", i))
	}
	for i := 95; i < 100; i++ {
		edge95[i] = fmt.Sprintf("other95-%d", i)
	}
	for i := 94; i < 100; i++ {
		edge94[i] = fmt.Sprintf("other94-%d", i)
	}
	entries := []sitemapAuditEntry{
		{Path: "child", Issues: []string{"has_warnings"}},
		{Path: "edge94", Issues: []string{}},
		{Path: "edge95", Issues: []string{}},
		{Path: "parent", Issues: []string{}},
		{Path: "unreachable", Issues: []string{"unreachable_dns"}},
	}
	sets := []map[string]struct{}{urlSet(child...), urlSet(edge94...), urlSet(edge95...), urlSet(parent...), nil}
	applySitemapOverlaps(entries, sets)

	if o := entries[0].Overlap; o == nil || o.With != "parent" || o.Shared != 100 || o.Share != 1 {
		t.Errorf("child overlap = %+v", o)
	}
	if !reflect.DeepEqual(entries[0].Issues, []string{"has_warnings", "redundant"}) {
		t.Errorf("child issues = %v", entries[0].Issues)
	}
	if o := entries[3].Overlap; o == nil || o.With != "child" || o.Shared != 100 || o.Share != 0.8333 {
		t.Errorf("parent overlap = %+v", o)
	}
	if len(entries[3].Issues) != 0 {
		t.Errorf("parent should not be redundant: %v", entries[3].Issues)
	}
	// edge cases around the 95% threshold; the best match for the edge sets is parent (100 of their URLs minus the swapped-out ones)
	if o := entries[2].Overlap; o == nil || o.Shared != 95 || o.Share != 0.95 {
		t.Errorf("edge95 overlap = %+v", o)
	}
	if !reflect.DeepEqual(entries[2].Issues, []string{"redundant"}) {
		t.Errorf("edge95 issues = %v", entries[2].Issues)
	}
	if o := entries[1].Overlap; o == nil || o.Shared != 94 || o.Share != 0.94 {
		t.Errorf("edge94 overlap = %+v", o)
	}
	if len(entries[1].Issues) != 0 {
		t.Errorf("edge94 issues = %v", entries[1].Issues)
	}
	if entries[4].Overlap != nil || !reflect.DeepEqual(entries[4].Issues, []string{"unreachable_dns"}) {
		t.Errorf("unreachable entry = %+v", entries[4])
	}
}

func TestSummarizeSitemapAudit(t *testing.T) {
	got := summarizeSitemapAudit([]sitemapAuditEntry{
		{Issues: []string{}},
		{Issues: []string{"unreachable_dns", "stale_download"}},
		{Issues: []string{"has_warnings", "stale_download"}},
	})
	want := sitemapAuditSummary{Sitemaps: 3, WithIssues: 2, IssuesByKind: map[string]int{"unreachable_dns": 1, "stale_download": 2, "has_warnings": 1}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v\nwant %+v", got, want)
	}
	if s := summarizeSitemapAudit(nil); s.IssuesByKind == nil || s.Sitemaps != 0 {
		t.Errorf("empty = %+v", s)
	}
}

func urlsetXML(urls ...string) string {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?><urlset xmlns="http://www.sitemaps.org/schemas/sitemap/0.9">`)
	for _, u := range urls {
		fmt.Fprintf(&b, "<url><loc>%s</loc></url>", u)
	}
	b.WriteString("</urlset>")
	return b.String()
}

func indexXML(locs ...string) string {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?><sitemapindex xmlns="http://www.sitemaps.org/schemas/sitemap/0.9">`)
	for _, u := range locs {
		fmt.Fprintf(&b, "<sitemap><loc>%s</loc></sitemap>", u)
	}
	b.WriteString("</sitemapindex>")
	return b.String()
}

// newSitemapServer serves a small fixture site.
func newSitemapServer(t *testing.T) *httptest.Server {
	t.Helper()
	var srv *httptest.Server
	mux := http.NewServeMux()
	base := func() string { return srv.URL }
	mux.HandleFunc("/pages-a.xml", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, urlsetXML(base()+"/p1.html", base()+"/p2.html", base()+"/c.htm"))
	})
	mux.HandleFunc("/pages-b.xml.gz", func(w http.ResponseWriter, r *http.Request) {
		zw := gzip.NewWriter(w)
		fmt.Fprint(zw, urlsetXML(base()+"/p3.html", base()+"/blog/x"))
		zw.Close()
	})
	mux.HandleFunc("/index.xml", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, indexXML(base()+"/pages-a.xml", base()+"/pages-b.xml.gz", base()+"/gone.xml"))
	})
	mux.HandleFunc("/copy-of-a.xml", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, urlsetXML(base()+"/p1.html", base()+"/p2.html", base()+"/c.htm"))
	})
	mux.HandleFunc("/dups.xml", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, urlsetXML(base()+"/p1.html", base()+"/p1.html", base()+"/p2.html"))
	})
	mux.HandleFunc("/broken.xml", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "<html>not a sitemap</html>")
	})
	mux.HandleFunc("/gone.xml", func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) })
	srv = httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func testSitemapClient() *sitemap.Client {
	return &sitemap.Client{HTTP: http.DefaultClient, UserAgent: "gscx-test"}
}

func TestAuditSitemapsWithoutExpand(t *testing.T) {
	srv := newSitemapServer(t)
	now := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	in := []*searchconsole.WmxSitemap{
		{Path: srv.URL + "/index.xml", LastDownloaded: "2026-09-29T00:00:00Z",
			Contents: []*searchconsole.WmxSitemapContent{{Type: "web", Submitted: 5}, {Type: "image", Submitted: 9}}},
		{Path: srv.URL + "/gone.xml", LastDownloaded: "2025-01-29T11:08:29.404Z"},
		{Path: srv.URL + "/broken.xml", Warnings: 15},
		{Path: srv.URL + "/pages-a.xml", LastDownloaded: "2026-09-20T00:00:00Z", Errors: 1,
			Contents: []*searchconsole.WmxSitemapContent{{Type: "web", Submitted: 3}}},
	}
	got, err := auditSitemaps(context.Background(), testSitemapClient(), in, false, nil, now, 30)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 4 {
		t.Fatalf("got %d entries", len(got))
	}
	byName := map[string]sitemapAuditEntry{}
	for i, e := range got {
		byName[strings.TrimPrefix(e.Path, srv.URL)] = e
		if i > 0 && got[i-1].Path > e.Path {
			t.Errorf("entries must be sorted by path")
		}
	}

	idx := byName["/index.xml"]
	if !idx.Reachable || idx.Kind != "index" || idx.Children == nil || *idx.Children != 3 || idx.Submitted != 5 || len(idx.Issues) != 0 {
		t.Errorf("index = %+v", idx)
	}
	if idx.LastDownloadedAgeDays == nil || *idx.LastDownloadedAgeDays != 1 {
		t.Errorf("index age = %v", idx.LastDownloadedAgeDays)
	}
	if idx.URLs != nil || idx.ByType != nil || idx.Overlap != nil {
		t.Errorf("no expand data expected without --expand: %+v", idx)
	}

	gone := byName["/gone.xml"]
	if gone.Reachable || gone.FetchError == nil || gone.FetchError.Kind != "http_status" || gone.FetchError.StatusCode != 404 {
		t.Errorf("gone = %+v", gone)
	}
	if !reflect.DeepEqual(gone.Issues, []string{"unreachable_http_404", "stale_download"}) {
		t.Errorf("gone issues = %v", gone.Issues)
	}

	broken := byName["/broken.xml"]
	if broken.Reachable || broken.FetchError == nil || broken.FetchError.Kind != "parse" {
		t.Errorf("broken = %+v", broken)
	}
	if !reflect.DeepEqual(broken.Issues, []string{"unreachable_parse", "has_warnings"}) {
		t.Errorf("broken issues = %v", broken.Issues)
	}

	a := byName["/pages-a.xml"]
	if !a.Reachable || a.Kind != "urlset" || a.Children != nil || !reflect.DeepEqual(a.Issues, []string{"has_errors"}) {
		t.Errorf("pages-a = %+v", a)
	}
}

func TestAuditSitemapsExpand(t *testing.T) {
	srv := newSitemapServer(t)
	now := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	fresh := "2026-09-29T00:00:00Z"
	in := []*searchconsole.WmxSitemap{
		{Path: srv.URL + "/index.xml", LastDownloaded: fresh},
		{Path: srv.URL + "/pages-a.xml", LastDownloaded: fresh},
		{Path: srv.URL + "/copy-of-a.xml", LastDownloaded: fresh},
		{Path: srv.URL + "/pages-b.xml.gz", LastDownloaded: fresh},
		{Path: srv.URL + "/gone.xml", LastDownloaded: fresh},
	}
	got, err := auditSitemaps(context.Background(), testSitemapClient(), in, true, pagetype.MustDefault().Classify, now, 30)
	if err != nil {
		t.Fatal(err)
	}
	by := map[string]sitemapAuditEntry{}
	for _, e := range got {
		by[strings.TrimPrefix(e.Path, srv.URL)] = e
	}

	idx := by["/index.xml"]
	if idx.URLs == nil || *idx.URLs != 5 {
		t.Fatalf("index urls = %v", idx.URLs)
	}
	if idx.Entries == nil || *idx.Entries != 5 {
		t.Errorf("index entries = %v", idx.Entries)
	}
	if !reflect.DeepEqual(idx.ByType, map[string]int{"product": 3, "category": 1, "blog": 1}) {
		t.Errorf("index by_type = %v", idx.ByType)
	}
	if len(idx.ChildErrors) != 1 || idx.ChildErrors[0].Kind != "http_status" || idx.ChildErrors[0].StatusCode != 404 {
		t.Errorf("index child errors = %+v", idx.ChildErrors)
	}
	if !reflect.DeepEqual(idx.Issues, []string{"unreachable_children"}) {
		t.Errorf("index issues = %v", idx.Issues)
	}
	if idx.Children == nil || *idx.Children != 3 {
		t.Errorf("index children = %v", idx.Children)
	}

	// pages-a and copy-of-a are identical: both redundant against each other,
	// and both fully contained in the index.
	a, cp := by["/pages-a.xml"], by["/copy-of-a.xml"]
	if a.Overlap == nil || cp.Overlap == nil || a.Overlap.Share != 1 || cp.Overlap.Share != 1 {
		t.Fatalf("overlaps: a=%+v copy=%+v", a.Overlap, cp.Overlap)
	}
	// ties on shared count resolve to the lowest path-sorted index: copy-of-a < index.xml
	if a.Overlap.With != srv.URL+"/copy-of-a.xml" || cp.Overlap.With != srv.URL+"/index.xml" {
		t.Errorf("overlap partners: a->%s copy->%s", a.Overlap.With, cp.Overlap.With)
	}
	if !reflect.DeepEqual(a.Issues, []string{"redundant"}) || !reflect.DeepEqual(cp.Issues, []string{"redundant"}) {
		t.Errorf("issues a=%v copy=%v", a.Issues, cp.Issues)
	}

	b := by["/pages-b.xml.gz"]
	if b.URLs == nil || *b.URLs != 2 || b.Overlap == nil || b.Overlap.With != srv.URL+"/index.xml" || b.Overlap.Shared != 2 {
		t.Errorf("pages-b = %+v overlap=%+v", b, b.Overlap)
	}

	gone := by["/gone.xml"]
	if gone.Reachable || gone.URLs != nil || gone.Overlap != nil || !reflect.DeepEqual(gone.Issues, []string{"unreachable_http_404"}) {
		t.Errorf("gone = %+v", gone)
	}
}

func TestAuditSitemapsExpandCountsDuplicateEntries(t *testing.T) {
	srv := newSitemapServer(t)
	got, err := auditSitemaps(context.Background(), testSitemapClient(),
		[]*searchconsole.WmxSitemap{{Path: srv.URL + "/dups.xml"}}, true, pagetype.MustDefault().Classify, time.Now(), 30)
	if err != nil {
		t.Fatal(err)
	}
	e := got[0]
	if e.URLs == nil || *e.URLs != 2 || e.Entries == nil || *e.Entries != 3 {
		t.Errorf("urls=%v entries=%v, want 2 unique of 3 entries", e.URLs, e.Entries)
	}
	if e.Overlap != nil {
		t.Errorf("a single sitemap has nothing to overlap with: %+v", e.Overlap)
	}
}

func TestAuditSitemapsCancelled(t *testing.T) {
	srv := newSitemapServer(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := auditSitemaps(ctx, testSitemapClient(), []*searchconsole.WmxSitemap{{Path: srv.URL + "/pages-a.xml"}}, false, nil, time.Now(), 30)
	if err == nil {
		t.Error("cancelled context must abort the audit, not report 'unreachable'")
	}
	_, err = auditSitemaps(ctx, testSitemapClient(), []*searchconsole.WmxSitemap{{Path: srv.URL + "/pages-a.xml"}}, true, pagetype.MustDefault().Classify, time.Now(), 30)
	if err == nil {
		t.Error("cancelled context must abort the expanded audit too")
	}
}
