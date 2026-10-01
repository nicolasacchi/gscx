package commands

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nicolasacchi/clicore/cierrors"
	"github.com/nicolasacchi/gscx/internal/pagetype"
	"github.com/nicolasacchi/gscx/internal/quota"
	"github.com/nicolasacchi/gscx/internal/sampler"
	"github.com/nicolasacchi/gscx/internal/sitemap"
	"github.com/nicolasacchi/gscx/internal/store"
	"github.com/tidwall/gjson"
	"google.golang.org/api/option"
	searchconsole "google.golang.org/api/searchconsole/v1"
)

const testSite = "sc-domain:example.com"

// --- helpers -------------------------------------------------------------------

func openTestStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "gscx.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func okResult(u, verdict, state string) *searchconsole.UrlInspectionResult {
	return &searchconsole.UrlInspectionResult{
		InspectionResultLink: "https://search.google.com/search-console/inspect?u=" + u,
		IndexStatusResult: &searchconsole.IndexStatusInspectionResult{
			Verdict: verdict, CoverageState: state, IndexingState: "INDEXING_ALLOWED",
			RobotsTxtState: "ALLOWED", PageFetchState: "SUCCESSFUL",
			GoogleCanonical: u, UserCanonical: u,
			LastCrawlTime: "2026-09-29T04:00:00Z", CrawledAs: "MOBILE",
		},
	}
}

func apiErr(status int, msg string) error {
	return &cierrors.APIError{Status: status, Kind: cierrors.KindForStatus(status), Detail: msg}
}

// fakeAPI is a urlInspector whose behavior is scripted by call number (1-based).
type fakeAPI struct {
	mu    sync.Mutex
	calls []string
	fn    func(n int, u string) (*searchconsole.UrlInspectionResult, error)
}

func (f *fakeAPI) Inspect(_ context.Context, _, u string) (*searchconsole.UrlInspectionResult, error) {
	f.mu.Lock()
	f.calls = append(f.calls, u)
	n := len(f.calls)
	f.mu.Unlock()
	if f.fn == nil {
		return okResult(u, "PASS", "Submitted and indexed"), nil
	}
	return f.fn(n, u)
}

func (f *fakeAPI) called() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func planItems(n int) []sampler.Item {
	items := make([]sampler.Item, n)
	for i := range items {
		typ, ext := "product", ".html"
		if i%2 == 1 {
			typ, ext = "category", ".htm"
		}
		items[i] = sampler.Item{Type: typ, URL: fmt.Sprintf("https://www.example.com/p%03d%s", i, ext)}
	}
	return items
}

func newBatchRunner(t *testing.T, api urlInspector, concurrency int) (*batchRunner, *store.Store) {
	t.Helper()
	st := openTestStore(t)
	return &batchRunner{
		inspectRunner: inspectRunner{API: api, Store: st, Site: testSite},
		Concurrency:   concurrency,
	}, st
}

func usedToday(t *testing.T, st *store.Store) int {
	t.Helper()
	n, err := st.UsedOnDay(context.Background(), testSite, quota.Day(time.Now()))
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// --- pure helpers ---------------------------------------------------------------

func TestInProperty(t *testing.T) {
	cases := []struct {
		site, url string
		want      bool
	}{
		{"sc-domain:example.com", "https://www.example.com/a.html", true},
		{"sc-domain:example.com", "http://example.com/", true},
		{"sc-domain:example.com", "https://shop.eu.example.com/x", true},
		{"sc-domain:example.com", "https://EXAMPLE.com:8443/x", true},
		{"sc-domain:example.com", "https://notexample.com/", false},
		{"sc-domain:example.com", "https://example.com.evil.net/", false},
		{"sc-domain:example.com", "ftp://example.com/", false},
		{"sc-domain:example.com", "not a url", false},
		{"https://www.example.com/", "https://www.example.com/a.html", true},
		{"https://www.example.com/", "https://example.com/a.html", false},
		{"https://www.example.com/blog/", "https://www.example.com/shop/", false},
	}
	for _, tc := range cases {
		if got := inProperty(tc.site, tc.url); got != tc.want {
			t.Errorf("inProperty(%q, %q) = %v, want %v", tc.site, tc.url, got, tc.want)
		}
	}
}

func TestReadURLList(t *testing.T) {
	in := "https://a/1\n\n  https://a/2  \n# a comment\nhttps://a/1\r\nhttps://a/3"
	got, err := readURLList(strings.NewReader(in))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"https://a/1", "https://a/2", "https://a/3"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestDefaultSeed(t *testing.T) {
	first, second := defaultSeed("2026-10-01"), defaultSeed("2026-10-01")
	if first != second {
		t.Error("seed not stable within a day")
	}
	if defaultSeed("2026-10-01") == defaultSeed("2026-10-02") {
		t.Error("seed does not change across days")
	}
	if defaultSeed("2026-10-01") < 0 {
		t.Error("seed is negative")
	}
}

func TestCanonicalMismatch(t *testing.T) {
	cases := []struct {
		g, u string
		want bool
	}{
		{"https://a/x", "https://a/x", false},
		{"https://a/x", "https://a/y", true},
		{"", "https://a/y", false},
		{"https://a/x", "", false},
		{"", "", false},
		{"https://a", "https://a/", false},
		{"https://a/", "https://a", false},
		{"https://a/x", "https://a/x/", true},
		{"https://a", "https://b/", true},
		{"https://a?x=1", "https://a/?x=1", false},
	}
	for _, tc := range cases {
		if got := canonicalMismatch(tc.g, tc.u); got != tc.want {
			t.Errorf("canonicalMismatch(%q,%q) = %v", tc.g, tc.u, got)
		}
	}
}

// --- frame ------------------------------------------------------------------------

func TestLoadFrameWalksSitemapsAndGroupsByType(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/index.xml", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `<sitemapindex><sitemap><loc>http://%s/a.xml</loc></sitemap><sitemap><loc>http://%s/missing.xml</loc></sitemap></sitemapindex>`, r.Host, r.Host)
	})
	mux.HandleFunc("/a.xml", func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, `<urlset>
<url><loc>https://www.example.com/a.html</loc></url>
<url><loc>https://www.example.com/b.html</loc></url>
<url><loc>https://www.example.com/cat.htm</loc></url>
<url><loc>https://www.example.com/brands/acme</loc></url>
<url><loc>https://www.other.org/x.html</loc></url>
<url><loc>https://www.example.com/a.html</loc></url>
</urlset>`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	cls := pagetype.MustDefault()
	fr, err := loadFrame(context.Background(), testSite, cls, sitemap.NewClient("test"),
		[]string{srv.URL + "/index.xml"}, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	out := fr.out()
	if out.URLs != 4 || out.OutOfProperty != 1 {
		t.Errorf("urls=%d out_of_property=%d, want 4/1", out.URLs, out.OutOfProperty)
	}
	want := map[string]int{"product": 2, "category": 1, "brand": 1}
	for typ, n := range want {
		if out.ByType[typ] != n {
			t.Errorf("by_type[%s] = %d, want %d", typ, out.ByType[typ], n)
		}
	}
	if len(out.SitemapErrors) != 1 || out.SitemapErrors[0].StatusCode != 404 {
		t.Errorf("sitemap_errors = %+v, want the 404 child", out.SitemapErrors)
	}
	if len(out.Sources) != 1 {
		t.Errorf("sources = %v", out.Sources)
	}
}

func TestLoadFrameFailsWhenEveryFetchFails(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	defer srv.Close()
	_, err := loadFrame(context.Background(), testSite, pagetype.MustDefault(), sitemap.NewClient("test"),
		[]string{srv.URL + "/nope.xml"}, "", nil)
	var fe *sitemap.FetchError
	if !errors.As(err, &fe) || fe.StatusCode != 404 {
		t.Fatalf("err = %v, want sitemap 404 FetchError", err)
	}
	if _, err := loadFrame(context.Background(), testSite, pagetype.MustDefault(), nil, nil, "", nil); err == nil {
		t.Error("no sources: want an error")
	}
}

func TestLoadFrameFromStdinAndFile(t *testing.T) {
	cls := pagetype.MustDefault()
	list := "https://www.example.com/a.html\nhttps://www.example.com/b.htm\nhttps://elsewhere.net/c.html\n"
	fr, err := loadFrame(context.Background(), testSite, cls, nil, nil, "-", strings.NewReader(list))
	if err != nil {
		t.Fatal(err)
	}
	if fr.Total != 2 || fr.OutOfProperty != 1 || fr.Sources[0] != "stdin" {
		t.Errorf("stdin frame = %+v", fr)
	}
	p := filepath.Join(t.TempDir(), "urls.txt")
	if err := os.WriteFile(p, []byte(list), 0o600); err != nil {
		t.Fatal(err)
	}
	fr, err = loadFrame(context.Background(), testSite, cls, nil, nil, p, nil)
	if err != nil {
		t.Fatal(err)
	}
	if fr.Total != 2 || fr.Sources[0] != "file:"+p {
		t.Errorf("file frame = %+v", fr)
	}
	if _, err := loadFrame(context.Background(), testSite, cls, nil, nil, p+".missing", nil); err == nil {
		t.Error("missing file: want an error")
	}
}

// --- batch runner ---------------------------------------------------------------------

func TestBatchRunnerAllSucceed(t *testing.T) {
	api := &fakeAPI{fn: func(n int, u string) (*searchconsole.UrlInspectionResult, error) {
		if strings.HasSuffix(u, ".htm") {
			return okResult(u, "NEUTRAL", "Crawled - currently not indexed"), nil
		}
		return okResult(u, "PASS", "Submitted and indexed"), nil
	}}
	br, st := newBatchRunner(t, api, 3)
	out := br.Run(context.Background(), planItems(10))

	if out.Inspected != 10 || out.Errors != 0 || out.StoppedReason != "" || out.AbortErr != nil {
		t.Fatalf("outcome = %+v", out)
	}
	if got := out.ByType["product"]; got.N != 5 || got.Verdicts["PASS"] != 5 || got.CoverageStates["Submitted and indexed"] != 5 {
		t.Errorf("product = %+v", got)
	}
	if got := out.ByType["category"]; got.N != 5 || got.Verdicts["NEUTRAL"] != 5 {
		t.Errorf("category = %+v", got)
	}
	if got := usedToday(t, st); got != 10 {
		t.Errorf("rows stored = %d, want 10", got)
	}
	rows, err := st.LatestPerURL(context.Background(), testSite, time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
	if err != nil || len(rows) != 10 {
		t.Fatalf("LatestPerURL = %d rows, %v", len(rows), err)
	}
	raw, _ := st.RawJSON(context.Background(), rows[0].ID)
	if !strings.Contains(raw, "inspectionResultLink") {
		t.Errorf("raw_json = %q", raw)
	}
}

func TestBatchRunnerStopsGracefullyOn429(t *testing.T) {
	api := &fakeAPI{fn: func(n int, u string) (*searchconsole.UrlInspectionResult, error) {
		if n == 4 {
			return nil, apiErr(429, "Quota exceeded")
		}
		return okResult(u, "PASS", "Submitted and indexed"), nil
	}}
	br, st := newBatchRunner(t, api, 1)
	out := br.Run(context.Background(), planItems(10))

	if out.StoppedReason != "quota_exhausted" || out.AbortErr != nil {
		t.Fatalf("outcome = %+v", out)
	}
	if out.Inspected != 3 || out.Errors != 1 || api.called() != 4 {
		t.Errorf("inspected=%d errors=%d calls=%d, want 3/1/4", out.Inspected, out.Errors, api.called())
	}
	if got := usedToday(t, st); got != 4 {
		t.Errorf("rows stored = %d, want 4 (the 429 attempt is recorded)", got)
	}
}

func TestBatchRunner429WithConcurrencyStopsStartingNewCalls(t *testing.T) {
	api := &fakeAPI{fn: func(n int, u string) (*searchconsole.UrlInspectionResult, error) {
		if n >= 5 {
			return nil, apiErr(429, "Quota exceeded")
		}
		return okResult(u, "PASS", "Submitted and indexed"), nil
	}}
	br, st := newBatchRunner(t, api, 4)
	out := br.Run(context.Background(), planItems(200))
	if out.StoppedReason != "quota_exhausted" {
		t.Fatalf("reason = %q", out.StoppedReason)
	}
	if calls := api.called(); calls > 5+4 {
		t.Errorf("calls = %d: workers kept starting calls after 429 (max 5 + 4 in flight)", calls)
	}
	if got := usedToday(t, st); got != api.called() {
		t.Errorf("rows stored = %d, calls made = %d: every call must be stored", got, api.called())
	}
}

func TestBatchRunnerAbortsWhenFirstFiveAllFail(t *testing.T) {
	api := &fakeAPI{fn: func(n int, u string) (*searchconsole.UrlInspectionResult, error) {
		return nil, apiErr(403, "permission denied")
	}}
	br, st := newBatchRunner(t, api, 1)
	out := br.Run(context.Background(), planItems(20))

	var ae *cierrors.APIError
	if !errors.As(out.AbortErr, &ae) || ae.Status != 403 {
		t.Fatalf("AbortErr = %v, want the 403", out.AbortErr)
	}
	if api.called() != 5 || out.StoppedReason != "aborted_all_failed" {
		t.Errorf("calls=%d reason=%q, want 5 / aborted_all_failed", api.called(), out.StoppedReason)
	}
	if got := usedToday(t, st); got != 5 {
		t.Errorf("rows stored = %d, want 5 (failed calls consume quota)", got)
	}
}

func TestBatchRunnerDoesNotAbortOnEarlyBlipOrShortBatch(t *testing.T) {
	cases := []struct {
		name    string
		items   int
		failing func(n int) bool
		wantErr int
	}{
		{"4 failures then success", 10, func(n int) bool { return n <= 4 }, 4},
		{"success then failures", 10, func(n int) bool { return n > 1 }, 9},
		{"only 3 URLs, all fail", 3, func(n int) bool { return true }, 3},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			api := &fakeAPI{fn: func(n int, u string) (*searchconsole.UrlInspectionResult, error) {
				if tc.failing(n) {
					return nil, apiErr(500, "backend error")
				}
				return okResult(u, "PASS", "Submitted and indexed"), nil
			}}
			br, _ := newBatchRunner(t, api, 1)
			out := br.Run(context.Background(), planItems(tc.items))
			if out.AbortErr != nil || out.StoppedReason != "" {
				t.Fatalf("aborted: %+v", out)
			}
			if out.Errors != tc.wantErr || out.Inspected != tc.items-tc.wantErr || api.called() != tc.items {
				t.Errorf("errors=%d inspected=%d calls=%d", out.Errors, out.Inspected, api.called())
			}
		})
	}
}

func TestBatchRunnerRecordsPerURLErrorsAndContinues(t *testing.T) {
	items := planItems(6)
	bad := items[2].URL
	api := &fakeAPI{fn: func(n int, u string) (*searchconsole.UrlInspectionResult, error) {
		switch u {
		case bad:
			return nil, apiErr(400, "URL is not part of the property")
		case items[4].URL:
			return &searchconsole.UrlInspectionResult{}, nil // no index status result
		}
		return okResult(u, "PASS", "Submitted and indexed"), nil
	}}
	br, st := newBatchRunner(t, api, 1)
	out := br.Run(context.Background(), items)

	if out.Inspected != 4 || out.Errors != 2 || out.StoppedReason != "" || out.AbortErr != nil {
		t.Fatalf("outcome = %+v", out)
	}
	if len(out.ErrorSamples) != 2 || !strings.Contains(strings.Join(out.ErrorSamples, "|"), bad) {
		t.Errorf("error samples = %v", out.ErrorSamples)
	}
	if out.ByType["product"].Errors != 2 {
		t.Errorf("product errors = %d, want 2", out.ByType["product"].Errors)
	}
	if got := usedToday(t, st); got != 6 {
		t.Errorf("rows stored = %d, want 6", got)
	}
	// Failed calls are stored but are not "successful inspections".
	recent, err := st.RecentlyInspected(context.Background(), testSite, time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if recent[bad] || recent[items[4].URL] || len(recent) != 4 {
		t.Errorf("recently inspected = %v", recent)
	}
}

func TestBatchRunnerInterruptedAfterResult(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	api := &fakeAPI{fn: func(n int, u string) (*searchconsole.UrlInspectionResult, error) {
		if n == 3 {
			cancel() // Ctrl-C arrives while call 3 is in flight, but call 3 completes
		}
		return okResult(u, "PASS", "Submitted and indexed"), nil
	}}
	br, st := newBatchRunner(t, api, 1)
	out := br.Run(ctx, planItems(50))

	if out.StoppedReason != "interrupted" || out.AbortErr != nil {
		t.Fatalf("outcome = %+v", out)
	}
	if out.Inspected != 3 {
		t.Errorf("inspected = %d, want 3 (the in-flight result must be kept)", out.Inspected)
	}
	if got := usedToday(t, st); got != 3 {
		t.Errorf("rows stored = %d, want 3 (written even though ctx was cancelled)", got)
	}
}

func TestBatchRunnerInterruptedMidCallIsNotStored(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	api := &fakeAPI{fn: func(n int, u string) (*searchconsole.UrlInspectionResult, error) {
		if n == 3 {
			cancel()
			return nil, ctx.Err()
		}
		return okResult(u, "PASS", "Submitted and indexed"), nil
	}}
	br, st := newBatchRunner(t, api, 1)
	out := br.Run(ctx, planItems(50))
	if out.StoppedReason != "interrupted" || out.Inspected != 2 || out.Errors != 0 {
		t.Fatalf("outcome = %+v", out)
	}
	if got := usedToday(t, st); got != 2 {
		t.Errorf("rows stored = %d, want 2 (the cancelled call is not recorded)", got)
	}
}

func TestBatchRunnerAlreadyCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	api := &fakeAPI{}
	br, _ := newBatchRunner(t, api, 4)
	out := br.Run(ctx, planItems(10))
	if out.StoppedReason != "interrupted" || api.called() != 0 {
		t.Errorf("reason=%q calls=%d", out.StoppedReason, api.called())
	}
}

func TestBatchRunnerRunsWorkersConcurrentlyUnderTheSharedLimiter(t *testing.T) {
	var inflight, maxInflight atomic.Int32
	api := &fakeAPI{fn: func(n int, u string) (*searchconsole.UrlInspectionResult, error) {
		cur := inflight.Add(1)
		for {
			m := maxInflight.Load()
			if cur <= m || maxInflight.CompareAndSwap(m, cur) {
				break
			}
		}
		time.Sleep(40 * time.Millisecond)
		inflight.Add(-1)
		return okResult(u, "PASS", "Submitted and indexed"), nil
	}}
	br, _ := newBatchRunner(t, api, 4)
	br.Limiter = quota.NewLimiter(6000) // 10ms apart: slower than nothing, faster than the 40ms call
	start := time.Now()
	out := br.Run(context.Background(), planItems(12))
	if out.Inspected != 12 {
		t.Fatalf("inspected = %d", out.Inspected)
	}
	if m := maxInflight.Load(); m < 2 || m > 4 {
		t.Errorf("max in flight = %d, want between 2 and 4", m)
	}
	if el := time.Since(start); el < 100*time.Millisecond {
		t.Errorf("12 calls at 10ms spacing finished in %v: limiter not applied", el)
	}
}

func TestBatchRunnerProgressCallback(t *testing.T) {
	br, _ := newBatchRunner(t, &fakeAPI{}, 1)
	var lines []string
	br.Progress = func(f string, a ...any) { lines = append(lines, fmt.Sprintf(f, a...)) }
	br.Run(context.Background(), planItems(50))
	if len(lines) != 2 || !strings.HasPrefix(lines[0], "progress: 25/50") {
		t.Errorf("progress lines = %v", lines)
	}
}

// --- inspect <url>... --------------------------------------------------------------------

func TestInspectAll(t *testing.T) {
	cls := pagetype.MustDefault()
	good := "https://www.example.com/geanove-60cpr.html"
	mismatch := "https://www.example.com/dup.html"

	cases := []struct {
		name       string
		urls       []string
		fn         func(n int, u string) (*searchconsole.UrlInspectionResult, error)
		wantRows   int
		wantErr    bool
		wantStored int
	}{
		{"success", []string{good}, nil, 1, false, 1},
		{"one of two fails: both reported, no error", []string{good, "https://www.example.com/bad.html"},
			func(n int, u string) (*searchconsole.UrlInspectionResult, error) {
				if n == 2 {
					return nil, apiErr(400, "bad")
				}
				return okResult(u, "PASS", "Submitted and indexed"), nil
			}, 2, false, 2},
		{"every URL fails: error returned with rows", []string{good, mismatch},
			func(n int, u string) (*searchconsole.UrlInspectionResult, error) {
				return nil, apiErr(403, "no access")
			},
			2, true, 2},
		{"429 stops the loop", []string{good, mismatch, "https://www.example.com/c.html"},
			func(n int, u string) (*searchconsole.UrlInspectionResult, error) {
				if n == 2 {
					return nil, apiErr(429, "quota")
				}
				return okResult(u, "PASS", "Submitted and indexed"), nil
			}, 2, true, 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := openTestStore(t)
			api := &fakeAPI{fn: tc.fn}
			r := &inspectRunner{API: api, Store: st, Site: testSite}
			rows, err := inspectAll(context.Background(), r, cls, tc.urls, false)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
			if len(rows) != tc.wantRows {
				t.Fatalf("rows = %d, want %d", len(rows), tc.wantRows)
			}
			if got := usedToday(t, st); got != tc.wantStored {
				t.Errorf("stored = %d, want %d", got, tc.wantStored)
			}
		})
	}
}

func TestInspectAllSummaryAndRawShape(t *testing.T) {
	cls := pagetype.MustDefault()
	u := "https://www.example.com/geanove-60cpr.html"
	api := &fakeAPI{fn: func(n int, u string) (*searchconsole.UrlInspectionResult, error) {
		res := okResult(u, "PASS", "Submitted and indexed")
		res.IndexStatusResult.GoogleCanonical = "https://www.example.com/other.html"
		return res, nil
	}}
	st := openTestStore(t)
	r := &inspectRunner{API: api, Store: st, Site: testSite}

	rows, err := inspectAll(context.Background(), r, cls, []string{u}, false)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(rows[0])
	j := gjson.ParseBytes(b)
	for field, want := range map[string]string{
		"url": u, "page_type": "product", "verdict": "PASS", "coverage_state": "Submitted and indexed",
		"indexing_state": "INDEXING_ALLOWED", "robots_txt_state": "ALLOWED", "page_fetch_state": "SUCCESSFUL",
		"google_canonical": "https://www.example.com/other.html", "user_canonical": u,
		"last_crawl_time": "2026-09-29T04:00:00Z", "crawled_as": "MOBILE",
	} {
		if got := j.Get(field).String(); got != want {
			t.Errorf("%s = %q, want %q", field, got, want)
		}
	}
	if !j.Get("canonical_mismatch").Bool() {
		t.Error("canonical_mismatch = false, want true")
	}
	if !strings.Contains(j.Get("inspection_link").String(), "search.google.com") {
		t.Errorf("inspection_link = %q", j.Get("inspection_link"))
	}
	if j.Get("error").Exists() {
		t.Error("error present on a successful row")
	}

	rows, err = inspectAll(context.Background(), r, cls, []string{u}, true)
	if err != nil {
		t.Fatal(err)
	}
	b, _ = json.Marshal(rows[0])
	j = gjson.ParseBytes(b)
	if j.Get("inspection_result.indexStatusResult.verdict").String() != "PASS" {
		t.Errorf("raw row = %s", b)
	}
	if j.Get("verdict").Exists() {
		t.Error("raw row must not carry the flattened summary")
	}
}

// --- API adapter ---------------------------------------------------------------------------

func TestGSCInspectorAgainstFakeServer(t *testing.T) {
	var gotBody map[string]any
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		switch gotBody["inspectionUrl"] {
		case "https://www.example.com/quota.html":
			w.WriteHeader(http.StatusTooManyRequests)
			io.WriteString(w, `{"error":{"code":429,"message":"Quota exceeded for quota metric","status":"RESOURCE_EXHAUSTED"}}`)
		case "https://www.example.com/denied.html":
			w.WriteHeader(http.StatusForbidden)
			io.WriteString(w, `{"error":{"code":403,"message":"User does not have sufficient permission for site","status":"PERMISSION_DENIED"}}`)
		case "https://www.example.com/empty.html":
			io.WriteString(w, `{}`)
		default:
			io.WriteString(w, `{"inspectionResult":{"inspectionResultLink":"https://search.google.com/x","indexStatusResult":{"verdict":"PASS","coverageState":"Submitted and indexed","lastCrawlTime":"2026-09-29T04:00:00Z"}}}`)
		}
	}))
	defer srv.Close()

	svc, err := searchconsole.NewService(context.Background(),
		option.WithEndpoint(srv.URL), option.WithoutAuthentication(), option.WithHTTPClient(srv.Client()))
	if err != nil {
		t.Fatal(err)
	}
	api := gscInspector{svc}

	res, err := api.Inspect(context.Background(), testSite, "https://www.example.com/a.html")
	if err != nil {
		t.Fatal(err)
	}
	if res.IndexStatusResult.Verdict != "PASS" || res.InspectionResultLink == "" {
		t.Errorf("result = %+v", res)
	}
	if !strings.HasSuffix(gotPath, "/urlInspection/index:inspect") {
		t.Errorf("path = %q", gotPath)
	}
	if gotBody["siteUrl"] != testSite || gotBody["inspectionUrl"] != "https://www.example.com/a.html" {
		t.Errorf("request body = %v", gotBody)
	}

	_, err = api.Inspect(context.Background(), testSite, "https://www.example.com/quota.html")
	if !is429(err) {
		t.Errorf("429 not recognised through gsc.Wrap: %v", err)
	}
	_, err = api.Inspect(context.Background(), testSite, "https://www.example.com/denied.html")
	var ae *cierrors.APIError
	if !errors.As(err, &ae) || ae.Status != 403 || is429(err) {
		t.Errorf("403 error = %v", err)
	}
	if _, err = api.Inspect(context.Background(), testSite, "https://www.example.com/empty.html"); err == nil {
		t.Error("empty response: want an error")
	}
}

// --- CLI end to end (no network: --dry-run with --urls-file) ----------------------------------

// runCLI executes the real command tree against an isolated state and returns
// stdout. Flag state is reset afterwards so tests do not leak into each other.
func runCLI(t *testing.T, args ...string) (string, error) {
	t.Helper()
	var buf bytes.Buffer
	resetCLIState() // cobra keeps flag values between Execute calls
	t.Cleanup(resetCLIState)
	rootCmd.SetOut(&buf)
	rootCmd.SetErr(io.Discard)
	rootCmd.SetArgs(args)
	err := rootCmd.ExecuteContext(context.Background())
	return buf.String(), err
}

func resetCLIState() {
	flagSite, flagCredentials, flagConfig, flagDB, flagJQ, flagLimit, flagVerbose = "", "", "", "", "", 0, false
	flagInspectRaw, flagInspectSummary, flagInspectForce = false, true, false
	flagBatchSitemaps, flagBatchURLsFile, flagBatchTypes = nil, "", nil
	flagBatchPerType, flagBatchFreshDays, flagBatchSeed, flagBatchBudget, flagBatchPerMinute = 0, 0, 0, 0, 0
	flagBatchConcurrency, flagBatchDryRun = 4, false
	flagCovSince, flagCovVS, flagCovFrame, flagCovSitemaps = "30d", "", false, nil
	flagCovURLsType, flagCovURLsState, flagCovURLsVerdict, flagCovURLsSince = "", "", "", "30d"
	flagRichSince = "30d"
	flagRichURLsType, flagRichURLsRichType, flagRichURLsIssue, flagRichURLsSince = "", "", "", "30d"
	flagCruxKey, flagCruxOrigin, flagCruxForm = "", "", nil
	flagCwvURLs, flagCwvSample, flagCwvTypes, flagCwvSince = nil, 0, nil, "30d"
	flagCwvSitemaps, flagCwvFromMaps, flagCwvConcurrent, flagCwvWeeks = nil, false, 4, 25
	for _, n := range []string{"per-type", "fresh-days", "seed", "budget", "per-minute"} {
		inspectBatchCmd.Flags().Lookup(n).Changed = false
	}
	rootCmd.SetOut(nil)
	rootCmd.SetErr(nil)
	rootCmd.SetArgs(nil)
}

// cliBase are the global flags every CLI test passes.
func cliBase(dbPath string) []string {
	return []string{"--db", dbPath, "--site", testSite, "--config", filepath.Join(filepath.Dir(dbPath), "no-config.toml")}
}

func seedInspections(t *testing.T, dbPath string, recs ...store.Inspection) {
	t.Helper()
	st, err := store.Open(context.Background(), dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	for _, r := range recs {
		if r.Site == "" {
			r.Site = testSite
		}
		if err := st.InsertInspection(context.Background(), r); err != nil {
			t.Fatal(err)
		}
	}
}

func writeURLs(t *testing.T, urls ...string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "urls.txt")
	if err := os.WriteFile(p, []byte(strings.Join(urls, "\n")), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestInspectBatchDryRunEndToEnd(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "gscx.db")
	now := time.Now()
	day := quota.Day(now)
	// Today's usage: 1 fresh success (excluded from the plan) + 1 failed call.
	seedInspections(t, dbPath,
		store.Inspection{URL: "https://www.example.com/p1.html", PageType: "product", InspectedAt: now, Verdict: "PASS"},
		store.Inspection{URL: "https://www.example.com/bad.html", PageType: "product", InspectedAt: now, Error: "boom"},
	)
	urls := writeURLs(t,
		"https://www.example.com/p1.html", "https://www.example.com/p2.html", "https://www.example.com/p3.html",
		"https://www.example.com/c1.htm", "https://www.example.com/c2.htm",
		"https://www.example.com/brands/acme", "https://www.example.com/blog/post",
		"https://other.org/outside.html",
	)

	run := func(extra ...string) gjson.Result {
		t.Helper()
		args := append([]string{"inspect", "batch", "--dry-run", "--urls-file", urls}, cliBase(dbPath)...)
		out, err := runCLI(t, append(args, extra...)...)
		if err != nil {
			t.Fatalf("%v\n%s", err, out)
		}
		return gjson.Parse(out)
	}

	j := run("--per-type", "2")
	checks := map[string]int64{
		"frame.urls": 7, "frame.out_of_property": 1,
		"frame.by_type.product": 3, "frame.by_type.category": 2, "frame.by_type.brand": 1, "frame.by_type.blog": 1,
		"plan.total": 6, "plan.fresh_excluded": 1, // product loses the fresh p1
		"plan.by_type.product": 2, "plan.by_type.category": 2, "plan.by_type.brand": 1,
		"plan.eligible_by_type.product": 2, "plan.per_type": 2,
		"quota.used_before": 2, "quota.budget": 1800, "quota.remaining": 1798,
	}
	for path, want := range checks {
		if got := j.Get(path).Int(); got != want {
			t.Errorf("%s = %d, want %d\n%s", path, got, want, j.Raw)
		}
	}
	if !j.Get("dry_run").Bool() || j.Get("quota.day").String() != day {
		t.Errorf("dry_run/day wrong: %s", j.Raw)
	}
	if j.Get("inspected").Exists() || j.Get("quota.used_after").Exists() {
		t.Errorf("dry run reported results: %s", j.Raw)
	}
	if j.Get("seed").Int() != defaultSeed(day) {
		t.Errorf("default seed = %d, want day-derived %d", j.Get("seed").Int(), defaultSeed(day))
	}
	if again := run("--per-type", "2"); again.Raw != j.Raw {
		t.Error("same-day re-run is not reproducible")
	}

	if got := run("--per-type", "2", "--budget", "3"); got.Get("plan.total").Int() != 3 || got.Get("quota.batch_limit").Int() != 3 {
		t.Errorf("--budget 3: %s", got.Raw)
	}
	if got := run("--per-type", "2", "--budget", "100000"); got.Get("plan.total").Int() != 6 || got.Get("quota.batch_limit").Exists() {
		t.Errorf("--budget cannot raise the remaining budget: %s", got.Raw)
	}
	if got := run("--per-type", "2", "--types", "product,brand"); got.Get("plan.total").Int() != 3 || got.Get("plan.by_type.category").Exists() {
		t.Errorf("--types product,brand: %s", got.Raw)
	}
	if got := run("--per-type", "1", "--seed", "7"); got.Get("seed").Int() != 7 {
		t.Errorf("--seed 7: %s", got.Raw)
	}
	// fresh-days 0 disables the freshness filter: p1 is eligible again.
	if got := run("--per-type", "5", "--fresh-days", "0"); got.Get("plan.by_type.product").Int() != 3 || got.Get("plan.fresh_excluded").Int() != 0 {
		t.Errorf("--fresh-days 0: %s", got.Raw)
	}

	args := append([]string{"inspect", "batch", "--dry-run", "--urls-file", urls, "--types", "nope"}, cliBase(dbPath)...)
	if _, err := runCLI(t, args...); err == nil || !strings.Contains(err.Error(), "unknown page type") {
		t.Errorf("unknown --types: err = %v", err)
	}
}

func TestInspectBatchBudgetExhaustedMakesNoCalls(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "gscx.db")
	cfgPath := filepath.Join(filepath.Dir(dbPath), "config.toml")
	if err := os.WriteFile(cfgPath, []byte("[inspect]\ndaily_budget = 2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	seedInspections(t, dbPath,
		store.Inspection{URL: "https://www.example.com/x1.html", InspectedAt: now, Verdict: "PASS"},
		store.Inspection{URL: "https://www.example.com/x2.html", InspectedAt: now, Error: "boom"},
	)
	urls := writeURLs(t, "https://www.example.com/p1.html", "https://www.example.com/p2.html")
	// No credentials are available in tests: reaching newGSC would fail, so a
	// clean exit proves no API client was needed.
	out, err := runCLI(t, "inspect", "batch", "--urls-file", urls, "--db", dbPath, "--site", testSite, "--config", cfgPath)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	j := gjson.Parse(out)
	if j.Get("stopped_reason").String() != "budget_exhausted" || j.Get("inspected").Int() != 0 ||
		j.Get("plan.total").Int() != 0 || j.Get("quota.remaining").Int() != 0 || j.Get("quota.used_before").Int() != 2 {
		t.Errorf("output = %s", out)
	}
}

func TestInspectSingleRefusesWhenBudgetExhaustedUnlessForced(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "gscx.db")
	cfgPath := filepath.Join(filepath.Dir(dbPath), "config.toml")
	if err := os.WriteFile(cfgPath, []byte("[inspect]\ndaily_budget = 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	seedInspections(t, dbPath, store.Inspection{URL: "https://www.example.com/x1.html", InspectedAt: time.Now(), Verdict: "PASS"})

	_, err := runCLI(t, "inspect", "https://www.example.com/p1.html", "--db", dbPath, "--site", testSite, "--config", cfgPath)
	var ae *cierrors.APIError
	if !errors.As(err, &ae) || ae.Kind != "budget_exhausted" || !strings.Contains(ae.Hint, "--force") {
		t.Fatalf("err = %v, want budget_exhausted with a --force hint", err)
	}

	// Outside the property: refused before touching the budget or the API.
	_, err = runCLI(t, "inspect", "https://other.org/p1.html", "--db", dbPath, "--site", testSite, "--config", cfgPath)
	if !errors.As(err, &ae) || ae.Kind != "validation" {
		t.Errorf("out-of-property err = %v", err)
	}

	// --force gets past the budget check (and then fails on missing
	// credentials/network instead, which is not the budget error).
	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", filepath.Join(t.TempDir(), "missing-key.json"))
	_, err = runCLI(t, "inspect", "--force", "https://www.example.com/p1.html", "--db", dbPath, "--site", testSite, "--config", cfgPath)
	if err == nil {
		t.Fatal("expected a credentials error")
	}
	if errors.As(err, &ae) && ae.Kind == "budget_exhausted" {
		t.Errorf("--force did not bypass the budget check: %v", err)
	}
}
