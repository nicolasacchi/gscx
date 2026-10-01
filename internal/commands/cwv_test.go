package commands

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nicolasacchi/gscx/internal/config"
	"github.com/nicolasacchi/gscx/internal/crux"
	"github.com/nicolasacchi/gscx/internal/store"
	"github.com/tidwall/gjson"
)

const cwvTestKey = "AIzaSyCWVTESTKEY-0123456789"

// cruxServer is a fake CrUX API. Answers are chosen per request body.
type cruxServer struct {
	mu   sync.Mutex
	reqs []cruxReq
	// answer returns the HTTP status and body for one request.
	answer func(req cruxReq) (int, string)
}

type cruxReq struct {
	Path, Key string
	Body      map[string]any
}

func (r cruxReq) str(k string) string { s, _ := r.Body[k].(string); return s }

func (f *cruxServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	req := cruxReq{Path: r.URL.Path, Key: r.URL.Query().Get("key")}
	_ = json.Unmarshal(b, &req.Body)
	f.mu.Lock()
	f.reqs = append(f.reqs, req)
	f.mu.Unlock()
	status, body := f.answer(req)
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Retry-After", "0")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, body)
}

func (f *cruxServer) requests() []cruxReq {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]cruxReq(nil), f.reqs...)
}

// useCruxServer points the CrUX client at a fake server (and removes pacing)
// for the duration of the test, with the key supplied through the environment.
func useCruxServer(t *testing.T, answer func(req cruxReq) (int, string)) *cruxServer {
	t.Helper()
	fake := &cruxServer{answer: answer}
	srv := httptest.NewServer(fake)
	old := newCruxClient
	newCruxClient = func(o crux.Options) (*crux.Client, error) {
		o.BaseURL, o.MinInterval = srv.URL, 0
		return old(o)
	}
	t.Cleanup(func() { newCruxClient = old; srv.Close() })
	t.Setenv("GSCX_CRUX_API_KEY", cwvTestKey)
	return fake
}

const cruxNotFound = `{"error":{"code":404,"message":"chrome ux report data not found","status":"NOT_FOUND"}}`

// cruxRecord renders a record. lcp/inp are ms, cls the string-encoded p75;
// an empty string drops the metric.
func cruxRecord(key, lcp, inp, cls string) string {
	var metrics []string
	if lcp != "" {
		metrics = append(metrics, fmt.Sprintf(`"largest_contentful_paint":{"histogram":[{"start":0,"end":2500,"density":0.7},{"start":2500,"end":4000,"density":0.2},{"start":4000,"density":0.1}],"percentiles":{"p75":%s}}`, lcp))
	}
	if inp != "" {
		metrics = append(metrics, fmt.Sprintf(`"interaction_to_next_paint":{"histogram":[{"start":0,"end":200,"density":0.9},{"start":200,"end":500,"density":0.07},{"start":500,"density":0.03}],"percentiles":{"p75":%s}}`, inp))
	}
	if cls != "" {
		metrics = append(metrics, fmt.Sprintf(`"cumulative_layout_shift":{"histogram":[{"start":"0.00","end":"0.10","density":0.85},{"start":"0.10","end":"0.25","density":0.1},{"start":"0.25","density":0.05}],"percentiles":{"p75":%q}}`, cls))
	}
	return fmt.Sprintf(`{"record":{"key":%s,"metrics":{%s},"collectionPeriod":{"firstDate":{"year":2026,"month":9,"day":3},"lastDate":{"year":2026,"month":9,"day":30}}}}`,
		key, strings.Join(metrics, ","))
}

func cwvRun(t *testing.T, dbPath string, args ...string) gjson.Result {
	t.Helper()
	out, err := runCLI(t, append(args, cliBase(dbPath)...)...)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	return gjson.Parse(out)
}

func TestCwvOriginDefault(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "gscx.db")
	fake := useCruxServer(t, func(req cruxReq) (int, string) {
		if req.str("formFactor") == "PHONE" {
			return 200, cruxRecord(`{"formFactor":"PHONE","origin":"https://www.1000farmacie.it"}`, "2347", "180", "0.04")
		}
		return 404, cruxNotFound
	})
	j := cwvRun(t, dbPath, "cwv")

	// Requests: one origin query per form factor, the configured default
	// origin, the key as a query parameter, only the reported metrics.
	reqs := fake.requests()
	if len(reqs) != 2 {
		t.Fatalf("requests = %d, want PHONE + DESKTOP", len(reqs))
	}
	var ffs []string
	for _, r := range reqs {
		ffs = append(ffs, r.str("formFactor"))
		if r.Path != "/v1/records:queryRecord" || r.Key != cwvTestKey || r.str("origin") != "https://www.1000farmacie.it" || r.str("url") != "" {
			t.Errorf("request = %+v", r)
		}
		if got := fmt.Sprint(r.Body["metrics"]); got != "[largest_contentful_paint interaction_to_next_paint cumulative_layout_shift first_contentful_paint experimental_time_to_first_byte]" {
			t.Errorf("metrics = %s", got)
		}
	}
	sort.Strings(ffs)
	if !reflect.DeepEqual(ffs, []string{"DESKTOP", "PHONE"}) {
		t.Errorf("form factors = %v", ffs)
	}

	if j.Get("origin").String() != "https://www.1000farmacie.it" {
		t.Errorf("origin = %s", j.Get("origin"))
	}
	phone := j.Get("form_factors.PHONE")
	if phone.Get("status").String() != "ok" || phone.Get("collection_period.last").String() != "2026-09-30" {
		t.Fatalf("PHONE = %s", phone.Raw)
	}
	for path, want := range map[string]string{
		"metrics.lcp.rating": "good", "metrics.inp.rating": "good", "metrics.cls.rating": "good",
		"metrics.lcp.unit": "ms", "cwv_assessment": "pass",
	} {
		if got := phone.Get(path).String(); got != want {
			t.Errorf("PHONE %s = %q, want %q", path, got, want)
		}
	}
	if phone.Get("metrics.lcp.p75").Float() != 2347 || phone.Get("metrics.cls.p75").Float() != 0.04 {
		t.Errorf("p75s: %s", phone.Get("metrics").Raw)
	}
	if d := phone.Get("metrics.lcp.density"); d.Get("good").Float() != 0.7 || d.Get("needs_improvement").Float() != 0.2 || d.Get("poor").Float() != 0.1 {
		t.Errorf("lcp density = %s", d.Raw)
	}
	if phone.Get("metrics.fcp").Exists() || phone.Get("metrics.cls.unit").Exists() {
		t.Errorf("metrics the API did not return must be absent, CLS has no unit: %s", phone.Get("metrics").Raw)
	}
	if j.Get("form_factors.DESKTOP.status").String() != "no_data" || j.Get("form_factors.DESKTOP.metrics").Exists() {
		t.Errorf("DESKTOP = %s (a 404 is no_data, not an error)", j.Get("form_factors.DESKTOP").Raw)
	}
	if j.Get("thresholds.lcp.good").Float() != 2500 || j.Get("thresholds.cls.poor").Float() != 0.25 {
		t.Errorf("thresholds = %s", j.Get("thresholds").Raw)
	}
}

func TestCwvOriginFlagsAndRatings(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "gscx.db")
	fake := useCruxServer(t, func(req cruxReq) (int, string) {
		return 200, cruxRecord(`{"origin":"x"}`, "4200", "350", "0.3")
	})
	// --origin is normalized to scheme://host; ALL sends no formFactor.
	j := cwvRun(t, dbPath, "cwv", "--origin", "shop.example.com/some/path?x=1", "--form-factor", "all")
	reqs := fake.requests()
	if len(reqs) != 1 || reqs[0].str("origin") != "https://shop.example.com" {
		t.Fatalf("requests = %+v", reqs)
	}
	if _, ok := reqs[0].Body["formFactor"]; ok {
		t.Errorf("ALL must not send formFactor: %v", reqs[0].Body)
	}
	all := j.Get("form_factors.ALL")
	for path, want := range map[string]string{
		"metrics.lcp.rating": "poor", "metrics.inp.rating": "needs_improvement", "metrics.cls.rating": "poor", "cwv_assessment": "fail",
	} {
		if got := all.Get(path).String(); got != want {
			t.Errorf("%s = %q, want %q", path, got, want)
		}
	}

	for name, args := range map[string][]string{
		"bad form factor":                {"cwv", "--form-factor", "WATCH"},
		"bad origin":                     {"cwv", "--origin", "ftp://x.example"},
		"urls + sample":                  {"cwv", "--urls", "https://a.example/", "--sample", "2"},
		"url mode takes one form factor": {"cwv", "--urls", "https://a.example/", "--form-factor", "PHONE,DESKTOP"},
	} {
		if _, err := runCLI(t, append(args, cliBase(dbPath)...)...); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
}

func TestCwvMissingKey(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "gscx.db")
	t.Setenv("GSCX_CRUX_API_KEY", "")
	out, err := runCLI(t, append([]string{"cwv"}, cliBase(dbPath)...)...)
	if err == nil || !strings.Contains(err.Error(), "CrUX API key") {
		t.Errorf("err = %v\n%s", err, out)
	}
}

func TestCwvKeyPrecedenceAndMasking(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "gscx.db")
	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	const fileKey, envKey, flagKey = "file-key-AAAAAAAAAAAA", "env-key-BBBBBBBBBBBBB", "flag-key-CCCCCCCCCCCC"
	if err := os.WriteFile(cfgPath, []byte("[crux]\napi_key = \""+fileKey+"\"\norigin = \"https://cfg.example.com\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	fake := useCruxServer(t, func(cruxReq) (int, string) { return 200, cruxRecord(`{}`, "2000", "100", "0.01") })
	base := []string{"--db", dbPath, "--site", testSite, "--config", cfgPath}
	keyOf := func(args ...string) string {
		t.Helper()
		before := len(fake.requests())
		out, err := runCLI(t, append(append([]string{"cwv", "--form-factor", "PHONE"}, args...), base...)...)
		if err != nil {
			t.Fatalf("%v\n%s", err, out)
		}
		reqs := fake.requests()
		if len(reqs) != before+1 {
			t.Fatalf("requests: %d -> %d", before, len(reqs))
		}
		for _, k := range []string{fileKey, envKey, flagKey} {
			if strings.Contains(out, k) {
				t.Errorf("the output leaks a key: %s", out)
			}
		}
		if got := reqs[len(reqs)-1].str("origin"); got != "https://cfg.example.com" {
			t.Errorf("origin = %q, want the configured one", got)
		}
		return reqs[len(reqs)-1].Key
	}

	t.Setenv("GSCX_CRUX_API_KEY", "")
	if got := keyOf(); got != fileKey {
		t.Errorf("config only: key = %q, want %q", got, fileKey)
	}
	t.Setenv("GSCX_CRUX_API_KEY", envKey)
	if got := keyOf(); got != envKey {
		t.Errorf("env over config: key = %q, want %q", got, envKey)
	}
	if got := keyOf("--crux-key", flagKey); got != flagKey {
		t.Errorf("flag over env: key = %q, want %q", got, flagKey)
	}

	// config show masks the key (and --reveal does not).
	out, err := runCLI(t, "config", "show", "--config", cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, fileKey) || !strings.Contains(out, "[crux]") || !strings.Contains(out, "api_key") {
		t.Errorf("config show leaks or omits the crux key:\n%s", out)
	}
	if out, err = runCLI(t, "config", "show", "--reveal", "--config", cfgPath); err != nil || !strings.Contains(out, fileKey) {
		t.Errorf("config show --reveal: %v\n%s", err, out)
	}
}

func TestCwvURLs(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "gscx.db")
	fake := useCruxServer(t, func(req cruxReq) (int, string) {
		switch req.str("url") {
		case "https://www.example.com/":
			return 200, `{"record":{"key":{"url":"https://www.example.com/"},"metrics":{"largest_contentful_paint":{"percentiles":{"p75":1900}}},"collectionPeriod":{"firstDate":{"year":2026,"month":9,"day":3},"lastDate":{"year":2026,"month":9,"day":30}}},"urlNormalizationDetails":{"originalUrl":"https://www.example.com/","normalizedUrl":"https://www.example.com/"}}`
		case "https://www.example.com/brands/acme#frag":
			return 200, `{"record":{"key":{"url":"https://www.example.com/brands/acme"},"metrics":{"largest_contentful_paint":{"percentiles":{"p75":5000}}}},"urlNormalizationDetails":{"normalizedUrl":"https://www.example.com/brands/acme"}}`
		case "https://www.example.com/boom":
			return 500, `{"error":{"code":500,"message":"backend exploded"}}`
		}
		return 404, cruxNotFound
	})
	j := cwvRun(t, dbPath, "cwv", "--urls",
		"https://www.example.com/, https://www.example.com/p1.html,https://www.example.com/brands/acme#frag,https://www.example.com/p2.html,https://www.example.com/boom,https://www.example.com/")

	reqs := fake.requests()
	for _, r := range reqs {
		if _, ok := r.Body["formFactor"]; ok || r.str("origin") != "" {
			t.Errorf("a URL query must be url-only and all form factors: %v", r.Body)
		}
	}
	if j.Get("queried").Int() != 5 || len(reqs) < 5 { // the duplicate URL is queried once
		t.Fatalf("queried = %d, requests = %d", j.Get("queried").Int(), len(reqs))
	}
	if j.Get("with_data").Int() != 2 || j.Get("no_data").Int() != 2 || j.Get("errors").Int() != 1 || j.Get("form_factor").String() != "ALL" {
		t.Errorf("counts: %s", j.Raw)
	}
	byURL := map[string]gjson.Result{}
	for _, u := range j.Get("urls").Array() {
		byURL[u.Get("url").String()] = u
	}
	home := byURL["https://www.example.com/"]
	if home.Get("status").String() != "ok" || home.Get("page_type").String() != "home" || home.Get("metrics.lcp.rating").String() != "good" ||
		home.Get("cwv_assessment").String() != "insufficient_data" {
		t.Errorf("home = %s", home.Raw)
	}
	brand := byURL["https://www.example.com/brands/acme#frag"]
	if brand.Get("normalized_url").String() != "https://www.example.com/brands/acme" || brand.Get("page_type").String() != "brand" || brand.Get("metrics.lcp.rating").String() != "poor" {
		t.Errorf("brand = %s", brand.Raw)
	}
	if p := byURL["https://www.example.com/p1.html"]; p.Get("status").String() != "no_data" || p.Get("page_type").String() != "product" || p.Get("error").Exists() {
		t.Errorf("product = %s", p.Raw)
	}
	if e := byURL["https://www.example.com/boom"]; e.Get("status").String() != "error" || !strings.Contains(e.Get("error").String(), "500") || e.Get("page_type").String() != "other" {
		t.Errorf("error row = %s", e.Raw)
	}
	prod := j.Get("by_type.product")
	if prod.Get("queried").Int() != 2 || prod.Get("no_data").Int() != 2 || prod.Get("with_data").Int() != 0 || prod.Get("with_data_pct").Float() != 0 || !prod.Get("ci95.hi").Exists() {
		t.Errorf("by_type.product = %s", prod.Raw)
	}
	if home := j.Get("by_type.home"); home.Get("with_data").Int() != 1 || home.Get("with_data_pct").Float() != 100 || home.Get("assessments.insufficient_data").Int() != 1 {
		t.Errorf("by_type.home = %s", home.Raw)
	}
	if j.Get("by_type.other.errors").Int() != 1 || j.Get("by_type.other.with_data_pct").Exists() {
		t.Errorf("by_type.other = %s", j.Get("by_type.other").Raw)
	}
	if j.Get("truncated").Exists() {
		t.Errorf("unexpected truncation: %s", j.Get("truncated").Raw)
	}
}

func TestCwvURLsCredentialErrorAbortsRun(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "gscx.db")
	useCruxServer(t, func(cruxReq) (int, string) {
		return 400, `{"error":{"code":400,"message":"API key not valid. Please pass a valid API key.","status":"INVALID_ARGUMENT","details":[{"reason":"API_KEY_INVALID"}]}}`
	})
	out, err := runCLI(t, append([]string{"cwv", "--urls", "https://www.example.com/a.html,https://www.example.com/b.html", "--concurrency", "1"}, cliBase(dbPath)...)...)
	if err == nil || !strings.Contains(err.Error(), "API key not valid") || strings.Contains(err.Error(), cwvTestKey) {
		t.Errorf("err = %v\n%s", err, out)
	}
	if out != "" {
		t.Errorf("no partial output expected on a credential error, got: %s", out)
	}
}

func TestCwvSampleFromInspections(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "gscx.db")
	now := time.Now().UTC()
	seeded := map[string]string{}
	var recs []store.Inspection
	add := func(u, typ string, at time.Time) {
		if typ != "" {
			seeded[u] = typ
		}
		recs = append(recs, store.Inspection{URL: u, InspectedAt: at, Verdict: "PASS"})
	}
	for i := range 6 {
		add(pathURL("p%d.html", i), "product", now.Add(-time.Hour))
	}
	for i := range 3 {
		add(pathURL("c%d.htm", i), "category", now.Add(-time.Hour))
	}
	add(pathURL("brands/acme"), "brand", now.Add(-time.Hour))
	add(pathURL("old.html"), "", now.Add(-90*24*time.Hour)) // outside --since
	seedInspections(t, dbPath, recs...)

	fake := useCruxServer(t, func(req cruxReq) (int, string) {
		if strings.HasSuffix(req.str("url"), "c0.htm") {
			return 200, cruxRecord(`{}`, "1500", "100", "0.02")
		}
		return 404, cruxNotFound
	})

	j := cwvRun(t, dbPath, "cwv", "--sample", "2")
	queried := map[string]int{}
	for _, r := range fake.requests() {
		u := r.str("url")
		if seeded[u] == "" {
			t.Errorf("queried %q, which is not a stored inspected URL (or is outside --since)", u)
		}
		queried[seeded[u]]++
	}
	if !reflect.DeepEqual(queried, map[string]int{"product": 2, "category": 2, "brand": 1}) {
		t.Errorf("queried per type = %v, want N per type (fewer when the type has fewer URLs)", queried)
	}
	if j.Get("sample.per_type").Int() != 2 || j.Get("sample.source").String() != "inspections" ||
		j.Get("sample.eligible.product").Int() != 6 || j.Get("sample.eligible.category").Int() != 3 {
		t.Errorf("sample = %s", j.Get("sample").Raw)
	}
	if j.Get("queried").Int() != 5 || j.Get("by_type.product.no_data").Int() != 2 || j.Get("by_type.product.with_data").Int() != 0 {
		t.Errorf("summary: queried=%d product=%s", j.Get("queried").Int(), j.Get("by_type.product").Raw)
	}

	// The pick is reproducible within a day (requests arrive in any order).
	first := queriedURLs(fake)
	cwvRun(t, dbPath, "cwv", "--sample", "2")
	all := queriedURLs(fake)
	if len(all) != 10 || !sameSet(all[5:], first) {
		t.Errorf("a second run must pick the same URLs: first=%v second=%v", first, all[5:])
	}

	// --type restricts the page types; --since narrows the pool.
	before := len(fake.requests())
	j = cwvRun(t, dbPath, "cwv", "--sample", "10", "--type", "product")
	if got := len(fake.requests()) - before; got != 6 || j.Get("by_type.product.queried").Int() != 6 || j.Get("by_type.category").Exists() {
		t.Errorf("--type product: %d requests, by_type = %s", got, j.Get("by_type").Raw)
	}
	if _, err := runCLI(t, append([]string{"cwv", "--sample", "2", "--type", "nope"}, cliBase(dbPath)...)...); err == nil {
		t.Error("unknown --type: want an error")
	}
	j = cwvRun(t, dbPath, "cwv", "--sample", "1", "--since", "1m")
	if j.Get("queried").Int() != 0 || !strings.Contains(j.Get("hint").String(), "inspect batch") {
		t.Errorf("empty pool: %s", j.Raw)
	}
}

func queriedURLs(f *cruxServer) []string {
	var out []string
	for _, r := range f.requests() {
		out = append(out, r.str("url"))
	}
	return out
}

func sameSet(a, b []string) bool {
	x, y := append([]string(nil), a...), append([]string(nil), b...)
	sort.Strings(x)
	sort.Strings(y)
	return reflect.DeepEqual(x, y)
}

func TestCwvURLsAllNoDataHint(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "gscx.db")
	useCruxServer(t, func(cruxReq) (int, string) { return 404, cruxNotFound })
	j := cwvRun(t, dbPath, "cwv", "--urls", "https://www.example.com/a.html", "--form-factor", "phone")
	if j.Get("with_data").Int() != 0 || j.Get("no_data").Int() != 1 || j.Get("form_factor").String() != "PHONE" ||
		!strings.Contains(j.Get("hint").String(), "origin figures") {
		t.Errorf("all no_data: %s", j.Raw)
	}
}

func TestCwvURLsLimit(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "gscx.db")
	useCruxServer(t, func(cruxReq) (int, string) { return 404, cruxNotFound })
	j := cwvRun(t, dbPath, "cwv", "--urls", "https://www.example.com/a.html,https://www.example.com/b.html,https://www.example.com/c.html", "--limit", "2")
	if j.Get("queried").Int() != 3 || len(j.Get("urls").Array()) != 2 || !j.Get("truncated.truncated").Bool() || j.Get("truncated.total").Int() != 3 {
		t.Errorf("--limit 2: %s", j.Raw)
	}
}

const cwvHistoryResponse = `{"record":{"key":{"formFactor":"PHONE","origin":"https://www.1000farmacie.it"},
 "metrics":{
  "largest_contentful_paint":{"percentilesTimeseries":{"p75s":[2300,2400,2600,null,2700,2400]}},
  "cumulative_layout_shift":{"percentilesTimeseries":{"p75s":["0.05","0.06","0.07","0.08","0.09","0.11"]}},
  "round_trip_time":{"percentilesTimeseries":{"p75s":[1,2,3,4,5,6]}}
 },
 "collectionPeriods":[
  {"firstDate":{"year":2026,"month":8,"day":23},"lastDate":{"year":2026,"month":9,"day":19}},
  {"firstDate":{"year":2026,"month":8,"day":30},"lastDate":{"year":2026,"month":9,"day":26}},
  {"firstDate":{"year":2026,"month":9,"day":6},"lastDate":{"year":2026,"month":10,"day":3}},
  {"firstDate":{"year":2026,"month":9,"day":13},"lastDate":{"year":2026,"month":10,"day":10}},
  {"firstDate":{"year":2026,"month":9,"day":20},"lastDate":{"year":2026,"month":10,"day":17}},
  {"firstDate":{"year":2026,"month":9,"day":27},"lastDate":{"year":2026,"month":10,"day":24}}
 ]}}`

func TestCwvHistory(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "gscx.db")
	fake := useCruxServer(t, func(cruxReq) (int, string) { return 200, cwvHistoryResponse })
	j := cwvRun(t, dbPath, "cwv", "history", "--weeks", "6")

	reqs := fake.requests()
	if len(reqs) != 1 || reqs[0].Path != "/v1/records:queryHistoryRecord" || reqs[0].Key != cwvTestKey ||
		reqs[0].str("formFactor") != "PHONE" || reqs[0].str("origin") != "https://www.1000farmacie.it" ||
		reqs[0].Body["collectionPeriodCount"] != float64(6) {
		t.Fatalf("request = %+v", reqs)
	}
	if j.Get("status").String() != "ok" || j.Get("form_factor").String() != "PHONE" || j.Get("weeks").Int() != 6 {
		t.Fatalf("header: %s", j.Raw)
	}
	periods := j.Get("periods").Array()
	if len(periods) != 6 || periods[0].Get("first").String() != "2026-08-23" || periods[5].Get("last").String() != "2026-10-24" {
		t.Fatalf("periods = %s", j.Get("periods").Raw)
	}
	// LCP: 2300 good, 2400 good, 2600 needs-improvement (change), null week, 2700 NI (no change), 2400 good (change).
	lcp := func(i int, f string) gjson.Result { return periods[i].Get("metrics.lcp." + f) }
	if lcp(0, "rating").String() != "good" || lcp(2, "rating").String() != "needs_improvement" || !lcp(2, "rating_changed").Bool() {
		t.Errorf("lcp week 2: %s", periods[2].Get("metrics.lcp").Raw)
	}
	if lcp(3, "p75").Type != gjson.Null || lcp(3, "rating").Exists() || lcp(3, "rating_changed").Exists() {
		t.Errorf("a week without data: %s", periods[3].Get("metrics.lcp").Raw)
	}
	if lcp(4, "rating_changed").Exists() || !lcp(5, "rating_changed").Bool() || lcp(5, "rating").String() != "good" {
		t.Errorf("lcp weeks 4-5: %s / %s", periods[4].Get("metrics.lcp").Raw, periods[5].Get("metrics.lcp").Raw)
	}
	// CLS: string-encoded; 0.09 good -> 0.11 needs improvement.
	if periods[4].Get("metrics.cls.rating").String() != "good" || periods[5].Get("metrics.cls.rating").String() != "needs_improvement" ||
		!periods[5].Get("metrics.cls.rating_changed").Bool() || periods[5].Get("metrics.cls.p75").Float() != 0.11 {
		t.Errorf("cls week 5: %s", periods[5].Get("metrics.cls").Raw)
	}
	if periods[0].Get("metrics.round_trip_time").Exists() || periods[0].Get("metrics.inp").Exists() {
		t.Errorf("only the reported metrics that came back: %s", periods[0].Get("metrics").Raw)
	}
	var got []string
	for _, c := range j.Get("rating_changes").Array() {
		got = append(got, fmt.Sprintf("%s %s->%s@%s", c.Get("metric"), c.Get("from"), c.Get("to"), c.Get("last")))
	}
	want := []string{
		"lcp good->needs_improvement@2026-10-03",
		"lcp needs_improvement->good@2026-10-24",
		"cls good->needs_improvement@2026-10-24",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("rating_changes = %v, want %v", got, want)
	}
	if c := j.Get("rating_changes.0"); c.Get("from_p75").Float() != 2400 || c.Get("to_p75").Float() != 2600 || c.Get("unit").String() != "ms" {
		t.Errorf("rating_changes.0 = %s (compared with the previous week with data)", c.Raw)
	}
}

func TestCwvHistoryNoDataAndValidation(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "gscx.db")
	fake := useCruxServer(t, func(cruxReq) (int, string) { return 404, cruxNotFound })
	j := cwvRun(t, dbPath, "cwv", "history", "--form-factor", "desktop")
	if j.Get("status").String() != "no_data" || j.Get("form_factor").String() != "DESKTOP" || len(j.Get("periods").Array()) != 0 ||
		fake.requests()[0].Body["collectionPeriodCount"] != float64(25) {
		t.Errorf("no data: %s (default 25 weeks)", j.Raw)
	}
	for name, args := range map[string][]string{
		"0 weeks":          {"cwv", "history", "--weeks", "0"},
		"41 weeks":         {"cwv", "history", "--weeks", "41"},
		"two form factors": {"cwv", "history", "--form-factor", "PHONE,DESKTOP"},
		"bad form factor":  {"cwv", "history", "--form-factor", "WATCH"},
	} {
		if _, err := runCLI(t, append(args, cliBase(dbPath)...)...); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
}

func TestBuildHistoryTolerantOfShortSeries(t *testing.T) {
	v := func(f float64) *float64 { return &f }
	h := &crux.History{
		Periods: []crux.Period{{First: "a", Last: "A"}, {First: "b", Last: "B"}, {First: "c", Last: "C"}},
		Metrics: map[string]crux.HistoryMetric{
			// Fewer p75s than periods: the rest are treated as missing.
			"largest_contentful_paint": {P75s: []*float64{v(2000), v(3000)}},
			// More p75s than periods: the extras are ignored.
			"interaction_to_next_paint": {P75s: []*float64{v(100), v(100), v(600), v(100)}},
		},
	}
	periods, changes := buildHistory(h)
	if len(periods) != 3 || periods[2].Metrics["lcp"].P75 != nil || !periods[1].Metrics["lcp"].RatingChanged {
		t.Errorf("periods = %+v", periods)
	}
	if len(changes) != 2 || changes[0].Metric != "lcp" || changes[1].Metric != "inp" || changes[1].To != crux.Poor {
		t.Errorf("changes = %+v", changes)
	}
	if _, ok := periods[0].Metrics["cls"]; ok {
		t.Error("a metric absent from the response must not appear")
	}
}

func TestFormFactors(t *testing.T) {
	cases := []struct {
		in, def []string
		want    []string
		wantErr bool
	}{
		{nil, []string{"PHONE", "DESKTOP"}, []string{"PHONE", "DESKTOP"}, false},
		{[]string{"desktop"}, nil, []string{"DESKTOP"}, false},
		{[]string{"phone", "PHONE", " tablet "}, nil, []string{"PHONE", "TABLET"}, false},
		{[]string{"all"}, nil, []string{""}, false},
		{nil, []string{"ALL"}, []string{""}, false},
		{[]string{"watch"}, nil, nil, true},
	}
	for _, tc := range cases {
		got, err := formFactors(tc.in, tc.def)
		if (err != nil) != tc.wantErr || !reflect.DeepEqual(got, tc.want) {
			t.Errorf("formFactors(%v, %v) = %v, %v; want %v (err %v)", tc.in, tc.def, got, err, tc.want, tc.wantErr)
		}
	}
}

func TestNormalizeOrigin(t *testing.T) {
	cases := []struct {
		in, want string
		wantErr  bool
	}{
		{"https://www.example.com", "https://www.example.com", false},
		{"https://www.example.com/", "https://www.example.com", false},
		{"https://www.example.com/a/b?x=1#f", "https://www.example.com", false},
		{"http://localhost:8080/x", "http://localhost:8080", false},
		{"shop.example.com", "https://shop.example.com", false},
		{"  https://www.example.com  ", "https://www.example.com", false},
		{"", "", true},
		{"ftp://x.example", "", true},
		{"https://", "", true},
	}
	for _, tc := range cases {
		got, err := normalizeOrigin(tc.in)
		if (err != nil) != tc.wantErr || got != tc.want {
			t.Errorf("normalizeOrigin(%q) = %q, %v; want %q (err %v)", tc.in, got, err, tc.want, tc.wantErr)
		}
	}
}

func TestCruxKeyAndOriginResolution(t *testing.T) {
	oldCfg := cfg
	t.Cleanup(func() { cfg = oldCfg; flagCruxKey, flagCruxOrigin = "", "" })
	cfg = config.Defaults()

	t.Setenv("GSCX_CRUX_API_KEY", "")
	if cruxKey() != "" || cruxOriginRaw() != "https://www.1000farmacie.it" {
		t.Errorf("defaults: key=%q origin=%q", cruxKey(), cruxOriginRaw())
	}
	cfg.Crux = config.Crux{APIKey: "cfg-key", Origin: "https://cfg.example.com"}
	if cruxKey() != "cfg-key" || cruxOriginRaw() != "https://cfg.example.com" {
		t.Errorf("config: key=%q origin=%q", cruxKey(), cruxOriginRaw())
	}
	t.Setenv("GSCX_CRUX_API_KEY", "env-key")
	if cruxKey() != "env-key" {
		t.Errorf("env over config: %q", cruxKey())
	}
	flagCruxKey, flagCruxOrigin = "flag-key", "https://flag.example.com"
	if cruxKey() != "flag-key" || cruxOriginRaw() != "https://flag.example.com" {
		t.Errorf("flag over env/config: key=%q origin=%q", cruxKey(), cruxOriginRaw())
	}
}

func TestCheckCrux(t *testing.T) {
	oldCfg := cfg
	t.Cleanup(func() { cfg = oldCfg })
	cfg = config.Defaults()
	ctx := context.Background()

	t.Run("no key is fine but reported", func(t *testing.T) {
		t.Setenv("GSCX_CRUX_API_KEY", "")
		c := checkCrux(ctx)
		if !c.OK || c.Name != "crux" || !strings.Contains(c.Detail, "not configured") || !strings.Contains(c.Hint, "GSCX_CRUX_API_KEY") {
			t.Errorf("check = %+v", c)
		}
	})
	t.Run("key + data", func(t *testing.T) {
		fake := useCruxServer(t, func(cruxReq) (int, string) { return 200, cruxRecord(`{}`, "2347", "", "") })
		c := checkCrux(ctx)
		if !c.OK || !strings.Contains(c.Detail, "LCP p75 2347 ms (good)") || !strings.Contains(c.Detail, "https://www.1000farmacie.it") {
			t.Errorf("check = %+v", c)
		}
		reqs := fake.requests()
		if len(reqs) != 1 || reqs[0].str("origin") != "https://www.1000farmacie.it" || reqs[0].Key != cwvTestKey {
			t.Errorf("requests = %+v (exactly one origin query)", reqs)
		}
	})
	t.Run("key accepted, no data", func(t *testing.T) {
		useCruxServer(t, func(cruxReq) (int, string) { return 404, cruxNotFound })
		if c := checkCrux(ctx); !c.OK || !strings.Contains(c.Detail, "no data") {
			t.Errorf("check = %+v", c)
		}
	})
	t.Run("bad key fails without echoing it", func(t *testing.T) {
		useCruxServer(t, func(cruxReq) (int, string) {
			return 400, `{"error":{"code":400,"message":"API key not valid. Please pass a valid API key.","status":"INVALID_ARGUMENT","details":[{"reason":"API_KEY_INVALID"}]}}`
		})
		c := checkCrux(ctx)
		if c.OK || strings.Contains(c.Detail, cwvTestKey) || !strings.Contains(c.Detail, "API key not valid") {
			t.Errorf("check = %+v", c)
		}
	})
	t.Run("bad configured origin", func(t *testing.T) {
		t.Setenv("GSCX_CRUX_API_KEY", cwvTestKey)
		cfg.Crux.Origin = "ftp://nope"
		defer func() { cfg.Crux.Origin = config.DefaultCruxOrigin }()
		if c := checkCrux(ctx); c.OK || !strings.Contains(c.Hint, "[crux] origin") {
			t.Errorf("check = %+v", c)
		}
	})
}

func TestCwvSampleFromSitemaps(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "gscx.db")
	sm := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `<urlset>
<url><loc>https://www.example.com/a.html</loc></url>
<url><loc>https://www.example.com/b.html</loc></url>
<url><loc>https://www.example.com/c.html</loc></url>
<url><loc>https://www.example.com/cat.htm</loc></url>
<url><loc>https://www.other.org/x.html</loc></url>
</urlset>`)
	}))
	defer sm.Close()
	fake := useCruxServer(t, func(cruxReq) (int, string) { return 404, cruxNotFound })

	// The (empty) store is not consulted with --from-sitemaps.
	j := cwvRun(t, dbPath, "cwv", "--sample", "2", "--from-sitemaps", "--sitemap", sm.URL+"/s.xml")
	queried := map[string]int{}
	for _, r := range fake.requests() {
		u := r.str("url")
		if !strings.HasPrefix(u, "https://www.example.com/") {
			t.Errorf("queried %q: URLs outside the property must be dropped", u)
		}
		if strings.HasSuffix(u, ".html") {
			queried["product"]++
		} else {
			queried["category"]++
		}
	}
	if !reflect.DeepEqual(queried, map[string]int{"product": 2, "category": 1}) {
		t.Errorf("queried = %v", queried)
	}
	if j.Get("sample.source").String() != "sitemaps" || j.Get("sample.eligible.product").Int() != 3 || j.Get("queried").Int() != 3 {
		t.Errorf("sample = %s", j.Raw)
	}
}
