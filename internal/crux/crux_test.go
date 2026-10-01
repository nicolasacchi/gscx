package crux

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nicolasacchi/clicore/cierrors"
)

const testKey = "AIzaSyTESTKEY-0123456789abcdef"

// captured is one request seen by the fake server.
type captured struct {
	Method, Path, Key, ContentType, UserAgent string
	Body                                      map[string]any
}

// fakeCrux serves canned responses and records requests.
type fakeCrux struct {
	mu   sync.Mutex
	reqs []captured
	// handler answers each request; n is the 1-based request count.
	handler func(w http.ResponseWriter, r *http.Request, n int)
}

func (f *fakeCrux) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	var body map[string]any
	_ = json.Unmarshal(b, &body)
	f.mu.Lock()
	f.reqs = append(f.reqs, captured{
		Method: r.Method, Path: r.URL.Path, Key: r.URL.Query().Get("key"),
		ContentType: r.Header.Get("Content-Type"), UserAgent: r.Header.Get("User-Agent"), Body: body,
	})
	n := len(f.reqs)
	f.mu.Unlock()
	f.handler(w, r, n)
}

func newTestClient(t *testing.T, h func(w http.ResponseWriter, r *http.Request, n int), mod ...func(*Options)) (*Client, *fakeCrux) {
	t.Helper()
	f := &fakeCrux{handler: h}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	o := Options{APIKey: testKey, BaseURL: srv.URL, UserAgent: "gscx/test"}
	for _, m := range mod {
		m(&o)
	}
	c, err := New(o)
	if err != nil {
		t.Fatal(err)
	}
	return c, f
}

func reply(body string) func(http.ResponseWriter, *http.Request, int) {
	return func(w http.ResponseWriter, _ *http.Request, _ int) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, body)
	}
}

func replyStatus(status int, body string) func(http.ResponseWriter, *http.Request, int) {
	return func(w http.ResponseWriter, _ *http.Request, _ int) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Retry-After", "0") // keeps retried 429/5xx cases instant
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}
}

const recordJSON = `{
  "record": {
    "key": {"formFactor": "PHONE", "origin": "https://www.example.com"},
    "metrics": {
      "largest_contentful_paint": {
        "histogram": [
          {"start": 0, "end": 2500, "density": 0.7945},
          {"start": 2500, "end": 4000, "density": 0.1115},
          {"start": 4000, "density": 0.094}
        ],
        "percentiles": {"p75": 2347}
      },
      "cumulative_layout_shift": {
        "histogram": [
          {"start": "0.00", "end": "0.10", "density": 0.87},
          {"start": "0.10", "end": "0.25", "density": 0.08},
          {"start": "0.25", "density": 0.05}
        ],
        "percentiles": {"p75": "0.04"}
      },
      "interaction_to_next_paint": {
        "histogram": [
          {"start": 0, "end": 200, "density": 0.9},
          {"start": 200, "end": 500, "density": 0.07}
        ]
      }
    },
    "collectionPeriod": {
      "firstDate": {"year": 2026, "month": 9, "day": 3},
      "lastDate": {"year": 2026, "month": 9, "day": 30}
    }
  },
  "urlNormalizationDetails": {"originalUrl": "https://www.example.com/a.html#x", "normalizedUrl": "https://www.example.com/a.html"}
}`

func TestQueryRecordRequestAndParse(t *testing.T) {
	c, f := newTestClient(t, reply(recordJSON))
	rec, err := c.QueryRecord(context.Background(),
		Target{Origin: "https://www.example.com", FormFactor: FormFactorPhone},
		[]string{"largest_contentful_paint", "cumulative_layout_shift"})
	if err != nil {
		t.Fatal(err)
	}

	// Request: POST, the documented path, the key as a query parameter, a JSON body.
	if len(f.reqs) != 1 {
		t.Fatalf("requests = %d", len(f.reqs))
	}
	r := f.reqs[0]
	if r.Method != http.MethodPost || r.Path != "/v1/records:queryRecord" || r.Key != testKey ||
		r.ContentType != "application/json" || r.UserAgent != "gscx/test" {
		t.Errorf("request = %+v", r)
	}
	wantBody := map[string]any{
		"origin": "https://www.example.com", "formFactor": "PHONE",
		"metrics": []any{"largest_contentful_paint", "cumulative_layout_shift"},
	}
	if !jsonEqual(r.Body, wantBody) {
		t.Errorf("body = %v, want %v", r.Body, wantBody)
	}
	if _, ok := r.Body["url"]; ok {
		t.Error("an origin query must not send url")
	}

	// Response.
	if rec.Key != (Key{Origin: "https://www.example.com", FormFactor: "PHONE"}) ||
		rec.Period != (Period{First: "2026-09-03", Last: "2026-09-30"}) ||
		rec.NormalizedURL != "https://www.example.com/a.html" {
		t.Errorf("record = %+v", rec)
	}
	lcp := rec.Metrics["largest_contentful_paint"]
	if lcp.P75 == nil || *lcp.P75 != 2347 || len(lcp.Histogram) != 3 {
		t.Fatalf("lcp = %+v", lcp)
	}
	if lcp.Histogram[0].End == nil || *lcp.Histogram[0].End != 2500 || lcp.Histogram[2].End != nil {
		t.Errorf("lcp bins = %+v (the last bin is open-ended)", lcp.Histogram)
	}
	g, ni, p, ok := lcp.Densities()
	if !ok || g != 0.7945 || ni != 0.1115 || p != 0.094 {
		t.Errorf("lcp densities = %v %v %v %v", g, ni, p, ok)
	}
	// CLS is encoded as strings, in the p75 and in the bin bounds.
	cls := rec.Metrics["cumulative_layout_shift"]
	if cls.P75 == nil || *cls.P75 != 0.04 {
		t.Errorf("cls p75 = %v, want 0.04 (string-encoded)", cls.P75)
	}
	if cls.Histogram[1].Start != 0.10 || cls.Histogram[1].End == nil || *cls.Histogram[1].End != 0.25 {
		t.Errorf("cls bins = %+v", cls.Histogram)
	}
	// A metric without a percentile keeps a nil P75 and cannot give densities (2 bins).
	inp := rec.Metrics["interaction_to_next_paint"]
	if inp.P75 != nil {
		t.Errorf("inp p75 = %v, want nil", *inp.P75)
	}
	if _, _, _, ok := inp.Densities(); ok {
		t.Error("Densities with 2 bins must report !ok")
	}
}

func TestQueryRecordURLTarget(t *testing.T) {
	c, f := newTestClient(t, reply(recordJSON))
	if _, err := c.QueryRecord(context.Background(), Target{URL: "https://www.example.com/a.html"}, nil); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"url": "https://www.example.com/a.html"}
	if got := f.reqs[0].Body; !jsonEqual(got, want) {
		t.Errorf("body = %v, want only the url (no formFactor = all form factors, no metrics = every metric)", got)
	}
}

const historyJSON = `{
  "record": {
    "key": {"formFactor": "PHONE", "origin": "https://www.example.com"},
    "metrics": {
      "largest_contentful_paint": {
        "histogramTimeseries": [
          {"start": 0, "end": 2500, "densities": [0.79, "NaN", 0.8]},
          {"start": 2500, "end": 4000, "densities": [0.11, "NaN", 0.1]},
          {"start": 4000, "densities": [0.1, "NaN", 0.1]}
        ],
        "percentilesTimeseries": {"p75s": [2347, null, 2601]}
      },
      "cumulative_layout_shift": {
        "percentilesTimeseries": {"p75s": ["0.04", null, "0.12"]}
      }
    },
    "collectionPeriods": [
      {"firstDate": {"year": 2026, "month": 8, "day": 16}, "lastDate": {"year": 2026, "month": 9, "day": 12}},
      {"firstDate": {"year": 2026, "month": 8, "day": 23}, "lastDate": {"year": 2026, "month": 9, "day": 19}},
      {"firstDate": {"year": 2026, "month": 8, "day": 30}, "lastDate": {"year": 2026, "month": 9, "day": 26}}
    ]
  }
}`

func TestQueryHistoryRequestAndParse(t *testing.T) {
	c, f := newTestClient(t, reply(historyJSON))
	h, err := c.QueryHistory(context.Background(), Target{Origin: "https://www.example.com", FormFactor: FormFactorPhone},
		[]string{"largest_contentful_paint", "cumulative_layout_shift"}, 3)
	if err != nil {
		t.Fatal(err)
	}
	r := f.reqs[0]
	if r.Method != http.MethodPost || r.Path != "/v1/records:queryHistoryRecord" || r.Key != testKey {
		t.Errorf("request = %+v", r)
	}
	wantBody := map[string]any{
		"origin": "https://www.example.com", "formFactor": "PHONE", "collectionPeriodCount": float64(3),
		"metrics": []any{"largest_contentful_paint", "cumulative_layout_shift"},
	}
	if !jsonEqual(r.Body, wantBody) {
		t.Errorf("body = %v, want %v", r.Body, wantBody)
	}

	if len(h.Periods) != 3 || h.Periods[0] != (Period{First: "2026-08-16", Last: "2026-09-12"}) || h.Periods[2].Last != "2026-09-26" {
		t.Errorf("periods = %+v", h.Periods)
	}
	lcp := h.Metrics["largest_contentful_paint"]
	if len(lcp.P75s) != 3 || lcp.P75s[0] == nil || *lcp.P75s[0] != 2347 || lcp.P75s[1] != nil || *lcp.P75s[2] != 2601 {
		t.Errorf("lcp p75s = %v (a missing period is nil)", fmtPtrs(lcp.P75s))
	}
	cls := h.Metrics["cumulative_layout_shift"]
	if cls.P75s[0] == nil || *cls.P75s[0] != 0.04 || cls.P75s[1] != nil || *cls.P75s[2] != 0.12 {
		t.Errorf("cls p75s = %v (string-encoded)", fmtPtrs(cls.P75s))
	}
	if len(lcp.Histogram) != 3 || lcp.Histogram[0].Densities[1] != nil || *lcp.Histogram[0].Densities[2] != 0.8 || lcp.Histogram[2].End != nil {
		t.Errorf("lcp histogram = %+v (\"NaN\" densities are missing periods)", lcp.Histogram)
	}
}

func TestHistoryDefaultPeriodCountIsOmitted(t *testing.T) {
	c, f := newTestClient(t, reply(historyJSON))
	if _, err := c.QueryHistory(context.Background(), Target{Origin: "https://www.example.com"}, nil, 0); err != nil {
		t.Fatal(err)
	}
	if _, ok := f.reqs[0].Body["collectionPeriodCount"]; ok {
		t.Errorf("body = %v: 0 weeks must leave the API default (25) in place", f.reqs[0].Body)
	}
}

func TestNotFoundIsNoData(t *testing.T) {
	notFound := replyStatus(404, `{"error":{"code":404,"message":"chrome ux report data not found","status":"NOT_FOUND"}}`)
	c, _ := newTestClient(t, notFound)
	if _, err := c.QueryRecord(context.Background(), Target{URL: "https://www.example.com/x.html"}, nil); !errors.Is(err, ErrNoData) {
		t.Errorf("QueryRecord err = %v, want ErrNoData", err)
	}
	if _, err := c.QueryHistory(context.Background(), Target{URL: "https://www.example.com/x.html"}, nil, 5); !errors.Is(err, ErrNoData) {
		t.Errorf("QueryHistory err = %v, want ErrNoData", err)
	}
}

func TestValidation(t *testing.T) {
	c, f := newTestClient(t, reply(recordJSON))
	ctx := context.Background()
	cases := []struct {
		name string
		call func() error
	}{
		{"neither origin nor url", func() error { _, err := c.QueryRecord(ctx, Target{}, nil); return err }},
		{"both origin and url", func() error {
			_, err := c.QueryRecord(ctx, Target{Origin: "https://a.example", URL: "https://a.example/x"}, nil)
			return err
		}},
		{"unknown form factor", func() error {
			_, err := c.QueryRecord(ctx, Target{Origin: "https://a.example", FormFactor: "WATCH"}, nil)
			return err
		}},
		{"too many weeks", func() error {
			_, err := c.QueryHistory(ctx, Target{Origin: "https://a.example"}, nil, MaxHistoryPeriods+1)
			return err
		}},
		{"negative weeks", func() error { _, err := c.QueryHistory(ctx, Target{Origin: "https://a.example"}, nil, -1); return err }},
	}
	for _, tc := range cases {
		var apiErr *cierrors.APIError
		if err := tc.call(); !errors.As(err, &apiErr) || apiErr.Kind != "validation" {
			t.Errorf("%s: err = %v, want a validation error", tc.name, err)
		}
	}
	if len(f.reqs) != 0 {
		t.Errorf("%d requests sent for invalid input", len(f.reqs))
	}
}

func TestNewRequiresKey(t *testing.T) {
	for _, k := range []string{"", "   "} {
		_, err := New(Options{APIKey: k})
		var apiErr *cierrors.APIError
		if !errors.As(err, &apiErr) || apiErr.Kind != "auth_failed" || !strings.Contains(apiErr.Hint, "GSCX_CRUX_API_KEY") {
			t.Errorf("New(%q) err = %v", k, err)
		}
	}
}

func TestErrorMapping(t *testing.T) {
	cases := []struct {
		name     string
		status   int
		body     string
		wantKind string
		wantHint string
	}{
		{"invalid key", 400, `{"error":{"code":400,"message":"API key not valid. Please pass a valid API key.","status":"INVALID_ARGUMENT","details":[{"reason":"API_KEY_INVALID"}]}}`,
			"auth_failed", "GSCX_CRUX_API_KEY"},
		{"api disabled", 403, `{"error":{"code":403,"message":"Chrome UX Report API has not been used in project 123 before or it is disabled.","status":"PERMISSION_DENIED"}}`,
			"forbidden", "enable the Chrome UX Report API"},
		{"bad argument", 400, `{"error":{"code":400,"message":"Invalid JSON payload received.","status":"INVALID_ARGUMENT"}}`,
			"validation", "full http(s) URL"},
		{"server error body-less", 503, ``, "server_error", "server error"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := newTestClient(t, replyStatus(tc.status, tc.body), func(o *Options) { o.MaxRetries = 1 })
			_, err := c.QueryRecord(context.Background(), Target{Origin: "https://www.example.com"}, nil)
			var apiErr *cierrors.APIError
			if !errors.As(err, &apiErr) {
				t.Fatalf("err = %v (%T), want *cierrors.APIError", err, err)
			}
			if apiErr.Status != tc.status || apiErr.Kind != tc.wantKind || !strings.Contains(apiErr.Hint, tc.wantHint) || apiErr.Detail == "" {
				t.Errorf("error = %+v", apiErr)
			}
			if errors.Is(err, ErrNoData) {
				t.Error("only a 404 is 'no data'")
			}
		})
	}
}

func TestErrorDetailNeverEchoesKey(t *testing.T) {
	body := `{"error":{"code":400,"message":"API key not valid: ` + testKey + `","status":"INVALID_ARGUMENT"}}`
	c, _ := newTestClient(t, replyStatus(400, body))
	_, err := c.QueryRecord(context.Background(), Target{Origin: "https://www.example.com"}, nil)
	if err == nil || strings.Contains(err.Error(), testKey) {
		t.Fatalf("err = %v", err)
	}
}

func TestRateLimitIsRetriedThenSucceeds(t *testing.T) {
	c, f := newTestClient(t, func(w http.ResponseWriter, r *http.Request, n int) {
		if n < 3 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		reply(recordJSON)(w, r, n)
	})
	rec, err := c.QueryRecord(context.Background(), Target{Origin: "https://www.example.com"}, nil)
	if err != nil || rec == nil {
		t.Fatalf("rec=%v err=%v", rec, err)
	}
	if len(f.reqs) != 3 {
		t.Errorf("requests = %d, want 3 (2 throttled + 1 ok)", len(f.reqs))
	}
}

func TestRateLimitExhaustedIsRateLimitedError(t *testing.T) {
	c, f := newTestClient(t, func(w http.ResponseWriter, _ *http.Request, _ int) {
		w.Header().Set("Retry-After", "0")
		w.WriteHeader(http.StatusTooManyRequests)
	}, func(o *Options) { o.MaxRetries = 2 })
	_, err := c.QueryRecord(context.Background(), Target{Origin: "https://www.example.com"}, nil)
	var apiErr *cierrors.APIError
	if !errors.As(err, &apiErr) || apiErr.Kind != "rate_limited" || apiErr.Status != 429 || !strings.Contains(apiErr.Hint, "150 queries per minute") {
		t.Fatalf("err = %v", err)
	}
	if len(f.reqs) != 3 {
		t.Errorf("requests = %d, want 1 + 2 retries", len(f.reqs))
	}
}

func TestKeyNeverLeaksIntoErrorsOrLogs(t *testing.T) {
	var logs []string
	logf := func(format string, args ...any) { logs = append(logs, strings.TrimSpace(fmt.Sprintf(format, args...))) }

	// A refused connection yields a *url.Error that embeds the request URL.
	srv := httptest.NewServer(http.NotFoundHandler())
	base := srv.URL
	srv.Close()
	c, err := New(Options{APIKey: testKey, BaseURL: base, Logf: logf})
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.QueryRecord(context.Background(), Target{Origin: "https://www.example.com"}, nil)
	if err == nil {
		t.Fatal("want a transport error")
	}
	if strings.Contains(err.Error(), testKey) {
		t.Errorf("transport error leaks the key: %v", err)
	}

	// A successful call logs the request without its query string.
	c2, _ := newTestClient(t, reply(recordJSON), func(o *Options) { o.Logf = logf })
	if _, err := c2.QueryRecord(context.Background(), Target{Origin: "https://www.example.com"}, nil); err != nil {
		t.Fatal(err)
	}
	if len(logs) < 3 {
		t.Fatalf("logs = %v", logs)
	}
	for _, l := range logs {
		if strings.Contains(l, testKey) || strings.Contains(l, "key=") {
			t.Errorf("log line leaks the key: %q", l)
		}
	}
	if !strings.Contains(strings.Join(logs, "\n"), "/v1/records:queryRecord") {
		t.Errorf("logs should name the endpoint: %v", logs)
	}
}

func TestScrubKeepsErrorChain(t *testing.T) {
	// A cancelled context must stay recognizable after scrubbing.
	c, _ := newTestClient(t, reply(recordJSON))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := c.QueryRecord(ctx, Target{Origin: "https://www.example.com"}, nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled in the chain", err)
	}
	if strings.Contains(err.Error(), testKey) {
		t.Errorf("error leaks the key: %v", err)
	}
}

func TestPacing(t *testing.T) {
	var inflight atomic.Int32
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request, n int) {
		inflight.Add(1)
		reply(recordJSON)(w, r, n)
	}, func(o *Options) { o.MinInterval = 40 * time.Millisecond })
	start := time.Now()
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = c.QueryRecord(context.Background(), Target{Origin: "https://www.example.com"}, nil)
		}()
	}
	wg.Wait()
	// Four request starts at least 40 ms apart span at least 3 intervals.
	if d := time.Since(start); d < 110*time.Millisecond {
		t.Errorf("4 requests took %v, want >= 3 x 40ms of pacing", d)
	}
	if inflight.Load() != 4 {
		t.Errorf("requests = %d", inflight.Load())
	}
}

func TestPacingHonoursContext(t *testing.T) {
	c, _ := newTestClient(t, reply(recordJSON), func(o *Options) { o.MinInterval = time.Hour })
	if _, err := c.QueryRecord(context.Background(), Target{Origin: "https://www.example.com"}, nil); err != nil {
		t.Fatal(err) // the first request is immediate
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := c.QueryRecord(ctx, Target{Origin: "https://www.example.com"}, nil); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want the second request to give up at the context deadline", err)
	}
}

func TestNumParsing(t *testing.T) {
	cases := []struct {
		in     string
		want   float64
		wantOK bool
		bad    bool
	}{
		{`2347`, 2347, true, false},
		{`"0.04"`, 0.04, true, false},
		{`0.7945`, 0.7945, true, false},
		{`null`, 0, false, false},
		{`"NaN"`, 0, false, false},
		{`""`, 0, false, false},
		{`"abc"`, 0, false, true},
	}
	for _, tc := range cases {
		var n num
		err := json.Unmarshal([]byte(tc.in), &n)
		if (err != nil) != tc.bad || n.OK != tc.wantOK || n.V != tc.want {
			t.Errorf("%s -> %+v, %v", tc.in, n, err)
		}
	}
}

func TestRating(t *testing.T) {
	cases := []struct {
		metric string
		p75    float64
		want   string
	}{
		// Good is inclusive; poor starts strictly above the upper threshold.
		{"largest_contentful_paint", 2500, Good},
		{"largest_contentful_paint", 2500.1, NeedsImprovement},
		{"largest_contentful_paint", 4000, NeedsImprovement},
		{"largest_contentful_paint", 4001, Poor},
		{"interaction_to_next_paint", 200, Good},
		{"interaction_to_next_paint", 201, NeedsImprovement},
		{"interaction_to_next_paint", 500, NeedsImprovement},
		{"interaction_to_next_paint", 501, Poor},
		{"cumulative_layout_shift", 0.1, Good},
		{"cumulative_layout_shift", 0.11, NeedsImprovement},
		{"cumulative_layout_shift", 0.25, NeedsImprovement},
		{"cumulative_layout_shift", 0.26, Poor},
		{"first_contentful_paint", 1800, Good},
		{"first_contentful_paint", 3000, NeedsImprovement},
		{"first_contentful_paint", 3001, Poor},
		{"experimental_time_to_first_byte", 800, Good},
		{"experimental_time_to_first_byte", 1800, NeedsImprovement},
		{"experimental_time_to_first_byte", 1801, Poor},
		{"largest_contentful_paint", 0, Good},
	}
	for _, tc := range cases {
		m, ok := ByAPI(tc.metric)
		if !ok {
			t.Fatalf("unknown metric %s", tc.metric)
		}
		if got := m.Rate(tc.p75); got != tc.want {
			t.Errorf("%s %v = %s, want %s", tc.metric, tc.p75, got, tc.want)
		}
	}
	if _, ok := ByAPI("round_trip_time"); ok {
		t.Error("only the reported metrics are known")
	}
	if got := APINames(); len(got) != 5 || got[0] != "largest_contentful_paint" || got[4] != "experimental_time_to_first_byte" {
		t.Errorf("APINames = %v", got)
	}
}

func TestAssess(t *testing.T) {
	cases := []struct {
		name string
		in   map[string]string
		want string
	}{
		{"all core good", map[string]string{"lcp": Good, "inp": Good, "cls": Good}, AssessPass},
		{"non-core ignored", map[string]string{"lcp": Good, "inp": Good, "cls": Good, "ttfb": Poor, "fcp": Poor}, AssessPass},
		{"one needs improvement", map[string]string{"lcp": NeedsImprovement, "inp": Good, "cls": Good}, AssessFail},
		{"one poor", map[string]string{"lcp": Good, "inp": Poor, "cls": Good}, AssessFail},
		{"fail beats missing", map[string]string{"lcp": Poor, "cls": Good}, AssessFail},
		{"missing core metric, rest good", map[string]string{"lcp": Good, "cls": Good}, AssessInsufficient},
		{"empty rating counts as missing", map[string]string{"lcp": Good, "inp": "", "cls": Good}, AssessInsufficient},
		{"nothing", nil, AssessInsufficient},
	}
	for _, tc := range cases {
		if got := Assess(tc.in); got != tc.want {
			t.Errorf("%s: %s, want %s", tc.name, got, tc.want)
		}
	}
}

// ---- helpers ----

func jsonEqual(a, b any) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return string(x) == string(y)
}

func fmtPtrs(ps []*float64) []any {
	out := make([]any, len(ps))
	for i, p := range ps {
		if p != nil {
			out[i] = *p
		}
	}
	return out
}
