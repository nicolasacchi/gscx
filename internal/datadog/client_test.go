package datadog

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nicolasacchi/clicore/cierrors"
)

var (
	t0 = time.Date(2026, 9, 24, 7, 18, 49, 0, time.UTC)
	t1 = time.Date(2026, 10, 1, 7, 18, 49, 0, time.UTC)
)

// newTestClient points a client at srv with pacing and retry sleeps disabled.
func newTestClient(t *testing.T, srv *httptest.Server, mutate ...func(*Options)) *Client {
	t.Helper()
	o := Options{
		APIKey: "api-key", AppKey: "app-key", BaseURL: srv.URL, HTTPClient: srv.Client(),
		RateLimit: 1000, RatePeriod: time.Millisecond,
		RetryDelay: func(int, int) time.Duration { return 0 },
	}
	for _, m := range mutate {
		m(&o)
	}
	c, err := New(o)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// serve replies with body for every request and captures the last decoded
// request body.
func serve(t *testing.T, status int, body string, got *map[string]any) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/v2/logs/analytics/aggregate" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("DD-API-KEY") != "api-key" || r.Header.Get("DD-APPLICATION-KEY") != "app-key" {
			t.Errorf("missing key headers: %v", r.Header)
		}
		if got != nil {
			b, _ := io.ReadAll(r.Body)
			*got = map[string]any{}
			if err := json.Unmarshal(b, got); err != nil {
				t.Errorf("request body is not JSON: %v", err)
			}
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestNewRequiresBothKeys(t *testing.T) {
	for _, o := range []Options{{}, {APIKey: "a"}, {AppKey: "b"}} {
		_, err := New(o)
		var ae *cierrors.APIError
		if !errors.As(err, &ae) || ae.Kind != "auth_failed" || ae.Hint == "" {
			t.Errorf("New(%+v) err = %v, want auth_failed with hint", o, err)
		}
	}
}

func TestBaseURL(t *testing.T) {
	cases := map[string]string{
		"":                    "https://api.datadoghq.eu",
		"datadoghq.eu":        "https://api.datadoghq.eu",
		"datadoghq.com":       "https://api.datadoghq.com",
		"us3.datadoghq.com":   "https://api.us3.datadoghq.com",
		"proxy.internal":      "https://proxy.internal",
		"http://127.0.0.1:99": "http://127.0.0.1:99",
	}
	for in, want := range cases {
		if got := BaseURL(in); got != want {
			t.Errorf("BaseURL(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCountScalar(t *testing.T) {
	var got map[string]any
	srv := serve(t, 200, `{"meta":{"status":"done","elapsed":361,"request_id":"r1"},"data":{"buckets":[{"by":{},"computes":{"c0":402404}}]}}`, &got)
	c := newTestClient(t, srv)
	q := `service:web @http.url_details.path:\/ -(a:b)`
	n, meta, err := c.Count(context.Background(), q, t0, t1)
	if err != nil {
		t.Fatal(err)
	}
	if n != 402404 || meta.Status != "done" || meta.Partial() || meta.RequestID != "r1" {
		t.Fatalf("n=%d meta=%+v", n, meta)
	}
	filter := got["filter"].(map[string]any)
	if filter["query"] != q {
		t.Errorf("query was altered: %q", filter["query"])
	}
	if filter["from"] != "2026-09-24T07:18:49Z" || filter["to"] != "2026-10-01T07:18:49Z" {
		t.Errorf("window = %v..%v", filter["from"], filter["to"])
	}
	comp := got["compute"].([]any)[0].(map[string]any)
	if comp["aggregation"] != "count" || comp["type"] != nil {
		t.Errorf("compute = %v", comp)
	}
	if _, ok := got["group_by"]; ok {
		t.Error("scalar count must not group")
	}
}

func TestCountNoBucketsIsZero(t *testing.T) {
	srv := serve(t, 200, `{"meta":{"status":"done"},"data":{"buckets":[]}}`, nil)
	n, _, err := newTestClient(t, srv).Count(context.Background(), "q", t0, t1)
	if err != nil || n != 0 {
		t.Fatalf("n=%d err=%v", n, err)
	}
}

func TestCountByGroupsAndSortsByMeasure(t *testing.T) {
	var got map[string]any
	srv := serve(t, 200, `{"meta":{"status":"done"},"data":{"buckets":[
		{"by":{"@http.status_code":"200"},"computes":{"c0":15489}},
		{"by":{"@http.status_code":304},"computes":{"c0":2617}},
		{"by":{"@http.status_code":null},"computes":{"c0":3}}]}}`, &got)
	b, _, err := newTestClient(t, srv).CountBy(context.Background(), "q", t0, t1, "@http.status_code", 20)
	if err != nil {
		t.Fatal(err)
	}
	want := []Bucket{{"200", 15489}, {"304", 2617}, {NoValue, 3}}
	if len(b) != len(want) {
		t.Fatalf("buckets = %+v", b)
	}
	for i := range want {
		if b[i] != want[i] {
			t.Errorf("bucket %d = %+v, want %+v", i, b[i], want[i])
		}
	}
	gb := got["group_by"].([]any)[0].(map[string]any)
	if gb["missing"] != NoValue {
		t.Errorf("group_by.missing = %v: lines without the facet would be dropped from the buckets", gb["missing"])
	}
	sort := gb["sort"].(map[string]any)
	if gb["facet"] != "@http.status_code" || gb["limit"].(float64) != 20 ||
		sort["type"] != "measure" || sort["aggregation"] != "count" || sort["order"] != "desc" {
		t.Errorf("group_by = %v (the default sort is alphabetical; measure sort is required)", gb)
	}
}

func TestCountDailyTimeseries(t *testing.T) {
	var got map[string]any
	srv := serve(t, 200, `{"meta":{"status":"done"},"data":{"buckets":[{"by":{},"computes":{"c0":[
		{"time":"2026-09-30T00:00:00.000Z","value":289515},
		{"time":"2026-10-01T00:00:00.000Z","value":112889},
		{"time":"2026-10-02T00:00:00.000Z","value":null}]}}]}}`, &got)
	days, _, err := newTestClient(t, srv).CountDaily(context.Background(), "q", t0, t1)
	if err != nil {
		t.Fatal(err)
	}
	if len(days) != 3 || days["2026-09-30"] != 289515 || days["2026-10-01"] != 112889 || days["2026-10-02"] != 0 {
		t.Fatalf("days = %v", days)
	}
	comp := got["compute"].([]any)[0].(map[string]any)
	if comp["type"] != "timeseries" || comp["interval"] != "1d" {
		t.Errorf("compute = %v", comp)
	}
	if got["options"].(map[string]any)["timezone"] != "UTC" {
		t.Errorf("options = %v, want UTC day buckets", got["options"])
	}
}

func TestCountDailyBy(t *testing.T) {
	srv := serve(t, 200, `{"meta":{"status":"done"},"data":{"buckets":[
		{"by":{"@http.status_code":"200"},"computes":{"c0":[{"time":"2026-09-30T00:00:00.000Z","value":10},{"time":"2026-10-01T00:00:00.000Z","value":5}]}},
		{"by":{"@http.status_code":"404"},"computes":{"c0":[{"time":"2026-09-30T00:00:00.000Z","value":2}]}}]}}`, nil)
	b, _, err := newTestClient(t, srv).CountDailyBy(context.Background(), "q", t0, t1, "@http.status_code", 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(b) != 2 || b[0].Value != "200" || b[0].Days["2026-10-01"] != 5 || b[1].Value != "404" || b[1].Days["2026-09-30"] != 2 {
		t.Fatalf("buckets = %+v", b)
	}
}

func TestPartialAndWarnings(t *testing.T) {
	srv := serve(t, 200, `{"meta":{"status":"timeout","warnings":[{"code":"unknown_index","title":"Unknown index","detail":"x"}]},"data":{"buckets":[{"by":{},"computes":{"c0":7}}]}}`, nil)
	n, meta, err := newTestClient(t, srv).Count(context.Background(), "q", t0, t1)
	if err != nil || n != 7 {
		t.Fatalf("n=%d err=%v", n, err)
	}
	if !meta.Partial() || meta.Status != "timeout" || len(meta.Warnings) != 1 || !strings.Contains(meta.Warnings[0], "Unknown index") {
		t.Fatalf("meta = %+v", meta)
	}
}

func TestErrorMapping(t *testing.T) {
	cases := []struct {
		status   int
		body     string
		detail   string
		kind     string
		exit     int
		hintPart string
	}{
		{400, `{"errors":["input_validation_error(bad query)"]}`, "input_validation_error(bad query)", "validation", 3, "query syntax"},
		{401, `{"errors":["Unauthorized"]}`, "Unauthorized", "auth_failed", 2, "--dd-site"},
		{403, `{"errors":["Forbidden"]}`, "Forbidden", "forbidden", 2, "logs_read_data"},
		{404, `{"errors":[{"title":"Not","detail":"found"}]}`, "Not found", "not_found", 4, ""},
	}
	for _, tc := range cases {
		var calls int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			atomic.AddInt32(&calls, 1)
			w.WriteHeader(tc.status)
			_, _ = w.Write([]byte(tc.body))
		}))
		_, _, err := newTestClient(t, srv).Count(context.Background(), "q", t0, t1)
		srv.Close()
		var ae *cierrors.APIError
		if !errors.As(err, &ae) {
			t.Fatalf("%d: err = %v, want APIError", tc.status, err)
		}
		if ae.Status != tc.status || ae.Kind != tc.kind || ae.ExitCode() != tc.exit {
			t.Errorf("%d: %+v exit=%d", tc.status, ae, ae.ExitCode())
		}
		if tc.hintPart != "" && !strings.Contains(ae.Hint, tc.hintPart) {
			t.Errorf("%d: hint %q lacks %q", tc.status, ae.Hint, tc.hintPart)
		}
		if ae.Detail != tc.detail {
			t.Errorf("%d: detail = %q, want %q", tc.status, ae.Detail, tc.detail)
		}
		if n := atomic.LoadInt32(&calls); n != 1 {
			t.Errorf("%d: a definitive 4xx must not be retried, got %d calls", tc.status, n)
		}
	}
}

func TestRateLimited429IsRetriedThenSucceeds(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) < 3 {
			w.WriteHeader(429)
			_, _ = w.Write([]byte(`{"errors":["Too many requests"]}`))
			return
		}
		_, _ = w.Write([]byte(`{"meta":{"status":"done"},"data":{"buckets":[{"by":{},"computes":{"c0":5}}]}}`))
	}))
	defer srv.Close()
	n, _, err := newTestClient(t, srv).Count(context.Background(), "q", t0, t1)
	if err != nil || n != 5 || atomic.LoadInt32(&calls) != 3 {
		t.Fatalf("n=%d err=%v calls=%d", n, err, calls)
	}
}

func TestRateLimited429ExhaustsToAPIError(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(429)
	}))
	defer srv.Close()
	c := newTestClient(t, srv, func(o *Options) { o.MaxAttempts = 3 })
	_, _, err := c.Count(context.Background(), "q", t0, t1)
	var ae *cierrors.APIError
	if !errors.As(err, &ae) || ae.Status != 429 || ae.Kind != "rate_limited" || ae.ExitCode() != 5 {
		t.Fatalf("err = %v", err)
	}
	if !strings.Contains(ae.Hint, "rate limited") || !strings.Contains(ae.Hint, "requests per") {
		t.Errorf("hint = %q", ae.Hint)
	}
	if atomic.LoadInt32(&calls) != 3 {
		t.Errorf("calls = %d, want 3 attempts", calls)
	}
}

func TestServerErrorRetriedThenSucceeds(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) == 1 {
			w.WriteHeader(503)
			return
		}
		_, _ = w.Write([]byte(`{"meta":{"status":"done"},"data":{"buckets":[]}}`))
	}))
	defer srv.Close()
	if _, _, err := newTestClient(t, srv).Count(context.Background(), "q", t0, t1); err != nil {
		t.Fatal(err)
	}
	if atomic.LoadInt32(&calls) != 2 {
		t.Errorf("calls = %d", calls)
	}
}

// The aggregate POST is read-only, so (unlike clicore's default for POST) a
// network failure is retried.
func TestNetworkErrorOnReadOnlyPostIsRetried(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) == 1 {
			conn, _, _ := w.(http.Hijacker).Hijack()
			_ = conn.Close()
			return
		}
		_, _ = w.Write([]byte(`{"meta":{"status":"done"},"data":{"buckets":[{"by":{},"computes":{"c0":9}}]}}`))
	}))
	defer srv.Close()
	n, _, err := newTestClient(t, srv).Count(context.Background(), "q", t0, t1)
	if err != nil || n != 9 || atomic.LoadInt32(&calls) != 2 {
		t.Fatalf("n=%d err=%v calls=%d", n, err, calls)
	}
}

func TestCancelledContextIsNotRetried(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
	}))
	defer srv.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, _, err := newTestClient(t, srv).Count(ctx, "q", t0, t1)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if atomic.LoadInt32(&calls) != 0 {
		t.Errorf("calls = %d", calls)
	}
}

func TestMalformedBodyIsAnError(t *testing.T) {
	srv := serve(t, 200, `<html>`, nil)
	if _, _, err := newTestClient(t, srv).Count(context.Background(), "q", t0, t1); err == nil {
		t.Fatal("want decode error")
	}
}

func TestVerboseLogNeverContainsKeys(t *testing.T) {
	srv := serve(t, 200, `{"meta":{"status":"done"},"data":{"buckets":[]}}`, nil)
	var lines []string
	c := newTestClient(t, srv, func(o *Options) {
		o.Logf = func(f string, a ...any) { lines = append(lines, fmt.Sprintf(f, a...)) }
	})
	if _, _, err := c.Count(context.Background(), "service:web", t0, t1); err != nil {
		t.Fatal(err)
	}
	if len(lines) < 2 || !strings.Contains(lines[0], "service:web") {
		t.Fatalf("expected request+response log lines, got %v", lines)
	}
	for _, l := range lines {
		if strings.Contains(l, "api-key") || strings.Contains(l, "app-key") {
			t.Errorf("log line leaks a key: %q", l)
		}
	}
}
