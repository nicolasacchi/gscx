// Package datadog is a minimal client for the Datadog Logs Aggregate API
// (POST /api/v2/logs/analytics/aggregate). gscx only ever counts log lines
// server-side — it never fetches raw log events — so the surface is three
// shapes of the same call: a scalar count, a count grouped by a facet, and a
// per-UTC-day timeseries (optionally grouped).
//
// Retry policy. The aggregate endpoint is a POST but strictly read-only, so
// unlike a mutating POST it is safe to repeat. clicore's httpclient refuses to
// retry a POST after a network error (a write may already have committed) and
// retries 429/5xx with a Retry-After-aware backoff. This client therefore runs
// httpclient with its own retries disabled and drives the retry loop itself:
// network errors are retried using httpclient.ShouldRetryNetwork as if the
// call were a GET (still never retrying permanent errors such as TLS failures
// or a cancelled context), 429/5xx are retried after a pause, and every
// attempt first passes the rate-limit pacer (see pacer.go).
package datadog

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"strings"
	"time"

	"github.com/nicolasacchi/clicore/cierrors"
	"github.com/nicolasacchi/clicore/httpclient"
	"github.com/nicolasacchi/clicore/redact"
)

const (
	aggregatePath      = "/api/v2/logs/analytics/aggregate"
	defaultMaxAttempts = 4
	defaultGroupLimit  = 10
	defaultSite        = "datadoghq.eu"
	// NoValue labels group_by buckets whose facet is missing on the log line.
	NoValue = "(none)"
)

// Options configures a Client.
type Options struct {
	APIKey string
	AppKey string
	// Site is the Datadog site, e.g. "datadoghq.eu" (default). Requests go to
	// https://api.<site>.
	Site string
	// BaseURL overrides the URL derived from Site (tests).
	BaseURL    string
	HTTPClient *http.Client
	UserAgent  string
	// Logf receives verbose request/response lines (already redacted). May be nil.
	Logf func(format string, args ...any)

	// RateLimit requests per RatePeriod; zero values use the aggregate API's
	// published 2 per 10 s until the first response says otherwise.
	RateLimit  int
	RatePeriod time.Duration
	// MaxAttempts bounds attempts per call (default 4).
	MaxAttempts int
	// RetryDelay overrides the pause before retry number attempt (0-based) after
	// a response with the given HTTP status (0 for a network error). Tests use it
	// to avoid real sleeps.
	RetryDelay func(attempt, status int) time.Duration
}

// Client talks to the Logs Aggregate API.
type Client struct {
	http        *httpclient.Client
	url         string
	pace        *pacer
	logf        func(string, ...any)
	maxAttempts int
	retryDelay  func(attempt, status int) time.Duration
}

// New builds a client. Missing credentials are reported as an auth error so the
// caller can surface a hint before any request is made.
func New(o Options) (*Client, error) {
	if o.APIKey == "" || o.AppKey == "" {
		return nil, &cierrors.APIError{
			Kind:   "auth_failed",
			Detail: "Datadog API key and application key are both required",
			Hint:   "set DD_API_KEY and DD_APP_KEY (or --dd-api-key/--dd-app-key, or [datadog] api_key/app_key in the config file)",
		}
	}
	base := o.BaseURL
	if base == "" {
		base = BaseURL(o.Site)
	}
	hopts := []httpclient.Option{httpclient.WithMaxRetries(0)}
	if o.HTTPClient != nil {
		hopts = append(hopts, httpclient.WithHTTPClient(o.HTTPClient))
	}
	c := &Client{
		http:        httpclient.New(base, keyAuth{api: o.APIKey, app: o.AppKey, ua: o.UserAgent}, hopts...),
		url:         strings.TrimRight(base, "/") + aggregatePath,
		pace:        newPacer(o.RateLimit, o.RatePeriod),
		logf:        o.Logf,
		maxAttempts: o.MaxAttempts,
		retryDelay:  o.RetryDelay,
	}
	if c.maxAttempts <= 0 {
		c.maxAttempts = defaultMaxAttempts
	}
	if c.retryDelay == nil {
		c.retryDelay = c.defaultRetryDelay
	}
	return c, nil
}

// BaseURL maps a Datadog site to its API origin ("datadoghq.eu" ->
// "https://api.datadoghq.eu"). Empty means datadoghq.eu.
func BaseURL(site string) string {
	site = strings.TrimSpace(site)
	if site == "" {
		site = defaultSite
	}
	if strings.HasPrefix(site, "http://") || strings.HasPrefix(site, "https://") {
		return strings.TrimRight(site, "/")
	}
	if strings.Contains(site, "datadoghq") || strings.Contains(site, "ddog-gov") {
		return "https://api." + site
	}
	return "https://" + site
}

// keyAuth injects the Datadog key headers on every attempt.
type keyAuth struct{ api, app, ua string }

func (a keyAuth) Apply(_ context.Context, req *http.Request, _ int) error {
	req.Header.Set("DD-API-KEY", a.api)
	req.Header.Set("DD-APPLICATION-KEY", a.app)
	if a.ua != "" {
		req.Header.Set("User-Agent", a.ua)
	}
	return nil
}

func (keyAuth) OnUnauthorized() bool { return false }

// Meta is the response metadata of one aggregate call.
type Meta struct {
	// Status is "done" for a complete answer; anything else (typically
	// "timeout") means the counts are partial.
	Status    string   `json:"status"`
	RequestID string   `json:"request_id,omitempty"`
	ElapsedMS int      `json:"elapsed_ms,omitempty"`
	Warnings  []string `json:"warnings,omitempty"`
}

// Partial reports whether Datadog flagged the answer as incomplete.
func (m Meta) Partial() bool { return m.Status != "" && m.Status != "done" }

// Bucket is one group_by bucket.
type Bucket struct {
	Value string `json:"value"`
	Count int64  `json:"count"`
}

// DailyBucket is one group_by bucket of a timeseries: counts per UTC day
// (YYYY-MM-DD).
type DailyBucket struct {
	Value string           `json:"value"`
	Days  map[string]int64 `json:"days"`
}

// Count returns the number of log lines matching query in [from, to).
func (c *Client) Count(ctx context.Context, query string, from, to time.Time) (int64, Meta, error) {
	resp, err := c.aggregate(ctx, buildRequest(query, from, to, false, "", 0))
	if err != nil {
		return 0, Meta{}, err
	}
	var total int64
	for _, b := range resp.Data.Buckets {
		n, err := scalar(b.Computes["c0"])
		if err != nil {
			return 0, resp.meta(), err
		}
		total += n
	}
	return total, resp.meta(), nil
}

// CountBy returns the count per value of facet (e.g. "@http.status_code"),
// largest first, at most limit buckets.
func (c *Client) CountBy(ctx context.Context, query string, from, to time.Time, facet string, limit int) ([]Bucket, Meta, error) {
	resp, err := c.aggregate(ctx, buildRequest(query, from, to, false, facet, limit))
	if err != nil {
		return nil, Meta{}, err
	}
	out := make([]Bucket, 0, len(resp.Data.Buckets))
	for _, b := range resp.Data.Buckets {
		n, err := scalar(b.Computes["c0"])
		if err != nil {
			return nil, resp.meta(), err
		}
		out = append(out, Bucket{Value: facetValue(b.By[facet]), Count: n})
	}
	return out, resp.meta(), nil
}

// CountDaily returns the count per UTC day (YYYY-MM-DD) using the aggregate
// API's timeseries compute with a 1d interval. Days without a bucket are
// absent; callers that want zeros fill them from the window.
//
// Datadog stamps buckets at UTC midnight and clips the first and last bucket to
// [from, to), so those two days are partial unless from/to sit on midnight.
func (c *Client) CountDaily(ctx context.Context, query string, from, to time.Time) (map[string]int64, Meta, error) {
	resp, err := c.aggregate(ctx, buildRequest(query, from, to, true, "", 0))
	if err != nil {
		return nil, Meta{}, err
	}
	days := map[string]int64{}
	for _, b := range resp.Data.Buckets {
		if err := addSeries(days, b.Computes["c0"]); err != nil {
			return nil, resp.meta(), err
		}
	}
	return days, resp.meta(), nil
}

// CountDailyBy is CountDaily grouped by facet: one DailyBucket per facet
// value (largest overall first, at most limit values).
func (c *Client) CountDailyBy(ctx context.Context, query string, from, to time.Time, facet string, limit int) ([]DailyBucket, Meta, error) {
	resp, err := c.aggregate(ctx, buildRequest(query, from, to, true, facet, limit))
	if err != nil {
		return nil, Meta{}, err
	}
	out := make([]DailyBucket, 0, len(resp.Data.Buckets))
	for _, b := range resp.Data.Buckets {
		days := map[string]int64{}
		if err := addSeries(days, b.Computes["c0"]); err != nil {
			return nil, resp.meta(), err
		}
		out = append(out, DailyBucket{Value: facetValue(b.By[facet]), Days: days})
	}
	return out, resp.meta(), nil
}

// ---- request / response shapes ----

type aggregateRequest struct {
	Filter  filter       `json:"filter"`
	Compute []compute    `json:"compute"`
	GroupBy []groupBy    `json:"group_by,omitempty"`
	Options *requestOpts `json:"options,omitempty"`
}

type filter struct {
	Query string `json:"query"`
	From  string `json:"from"`
	To    string `json:"to"`
}

type compute struct {
	Aggregation string `json:"aggregation"`
	Type        string `json:"type,omitempty"`
	Interval    string `json:"interval,omitempty"`
}

type groupBy struct {
	Facet string `json:"facet"`
	Limit int    `json:"limit"`
	// Missing names the bucket for log lines that lack the facet. Without it
	// Datadog silently drops those lines from the groups, so the buckets would
	// not add up to the scalar count.
	Missing string `json:"missing,omitempty"`
	Sort    sortBy `json:"sort"`
}

type sortBy struct {
	Aggregation string `json:"aggregation"`
	Order       string `json:"order"`
	Type        string `json:"type"`
}

type requestOpts struct {
	Timezone string `json:"timezone,omitempty"`
}

func buildRequest(query string, from, to time.Time, series bool, facet string, limit int) aggregateRequest {
	comp := compute{Aggregation: "count"}
	if series {
		comp.Type, comp.Interval = "timeseries", "1d"
	}
	req := aggregateRequest{
		Filter: filter{
			Query: query,
			From:  from.UTC().Format(time.RFC3339),
			To:    to.UTC().Format(time.RFC3339),
		},
		Compute: []compute{comp},
	}
	if series {
		// Pin day buckets to UTC: counts are stored per UTC day.
		req.Options = &requestOpts{Timezone: "UTC"}
	}
	if facet != "" {
		n := limit
		if n <= 0 {
			n = defaultGroupLimit
		}
		// Sorting by measure is required: the default is alphabetical, which
		// would make "top N" meaningless once a facet has more than N values.
		req.GroupBy = []groupBy{{
			Facet:   facet,
			Limit:   n,
			Missing: NoValue,
			Sort:    sortBy{Aggregation: "count", Order: "desc", Type: "measure"},
		}}
	}
	return req
}

type aggregateResponse struct {
	Meta struct {
		Status    string `json:"status"`
		RequestID string `json:"request_id"`
		Elapsed   int    `json:"elapsed"`
		Warnings  []struct {
			Code   string `json:"code"`
			Title  string `json:"title"`
			Detail string `json:"detail"`
		} `json:"warnings"`
	} `json:"meta"`
	Data struct {
		Buckets []struct {
			By       map[string]json.RawMessage `json:"by"`
			Computes map[string]json.RawMessage `json:"computes"`
		} `json:"buckets"`
	} `json:"data"`
}

func (r aggregateResponse) meta() Meta {
	m := Meta{Status: r.Meta.Status, RequestID: r.Meta.RequestID, ElapsedMS: r.Meta.Elapsed}
	for _, w := range r.Meta.Warnings {
		parts := []string{}
		for _, s := range []string{w.Code, w.Title, w.Detail} {
			if s != "" {
				parts = append(parts, s)
			}
		}
		if len(parts) > 0 {
			m.Warnings = append(m.Warnings, strings.Join(parts, ": "))
		}
	}
	return m
}

// scalar decodes one compute value as a count. A missing value (a bucket with
// no c0) is zero.
func scalar(raw json.RawMessage) (int64, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return 0, nil
	}
	var f float64
	if err := json.Unmarshal(raw, &f); err != nil {
		return 0, fmt.Errorf("datadog: unexpected compute value %s: %w", raw, err)
	}
	return int64(math.Round(f)), nil
}

// addSeries merges a timeseries compute value into days (summing, so several
// buckets of one day cannot overwrite each other).
func addSeries(days map[string]int64, raw json.RawMessage) error {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var pts []struct {
		Time  string   `json:"time"`
		Value *float64 `json:"value"`
	}
	if err := json.Unmarshal(raw, &pts); err != nil {
		return fmt.Errorf("datadog: unexpected timeseries value %s: %w", raw, err)
	}
	for _, p := range pts {
		t, err := time.Parse(time.RFC3339, p.Time)
		if err != nil {
			return fmt.Errorf("datadog: bad timeseries bucket time %q: %w", p.Time, err)
		}
		var v int64
		if p.Value != nil {
			v = int64(math.Round(*p.Value))
		}
		days[t.UTC().Format("2006-01-02")] += v
	}
	return nil
}

// facetValue renders a group_by "by" value: strings verbatim, numbers without
// a trailing ".0", null (facet missing on the line) as NoValue.
func facetValue(raw json.RawMessage) string {
	s := strings.TrimSpace(string(raw))
	switch {
	case s == "" || s == "null":
		return NoValue
	case strings.HasPrefix(s, `"`):
		var v string
		if json.Unmarshal(raw, &v) == nil {
			return v
		}
	}
	return s
}

// ---- transport ----

func (c *Client) aggregate(ctx context.Context, req aggregateRequest) (*aggregateResponse, error) {
	payload, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("datadog: encode request: %w", err)
	}
	if c.logf != nil {
		c.logf("> POST %s %s", c.url, redact.Body(payload))
	}
	var lastErr error
	for attempt := 0; attempt < c.maxAttempts; attempt++ {
		if err := c.pace.wait(ctx); err != nil {
			return nil, err
		}
		start := time.Now()
		body, resp, err := c.http.Do(ctx, http.MethodPost, c.url, payload, "application/json")
		status := 0
		if resp != nil {
			status = resp.StatusCode
			if resp.Header != nil {
				c.pace.observe(parseRateHeaders(resp.Header.Get))
			}
		}
		if c.logf != nil {
			c.logf("< %d (%s)", status, time.Since(start).Round(time.Millisecond))
		}

		switch {
		case err == nil && status >= 200 && status < 300:
			var out aggregateResponse
			if jerr := json.Unmarshal(body, &out); jerr != nil {
				return nil, fmt.Errorf("datadog: decode response: %w", jerr)
			}
			return &out, nil
		case err == nil:
			// A definitive 4xx (bad query, bad key, missing scope): retrying
			// cannot help.
			return nil, c.apiError(status, body)
		case status == http.StatusTooManyRequests || status >= 500:
			lastErr = c.apiError(status, body)
		case httpclient.ShouldRetryNetwork(http.MethodGet, err): // read-only POST: treat as GET
			lastErr = fmt.Errorf("datadog: request failed: %w", err)
		default:
			return nil, fmt.Errorf("datadog: request failed: %w", err)
		}
		if attempt == c.maxAttempts-1 {
			break
		}
		if c.logf != nil {
			c.logf("! retrying (attempt %d/%d): %v", attempt+2, c.maxAttempts, lastErr)
		}
		if err := sleepCtx(ctx, c.retryDelay(attempt, status)); err != nil {
			return nil, err
		}
	}
	return nil, lastErr
}

func (c *Client) defaultRetryDelay(attempt, status int) time.Duration {
	if status == http.StatusTooManyRequests {
		// The window is fixed-length; waiting one full window guarantees a fresh one.
		return c.pace.Period() + pacerMargin
	}
	return time.Duration(math.Pow(2, float64(attempt))) * time.Second
}

// apiError maps a failed HTTP response to the fleet error type with an
// actionable hint. body may be empty (httpclient drops it for exhausted
// 429/5xx retries).
func (c *Client) apiError(status int, body []byte) *cierrors.APIError {
	e := &cierrors.APIError{
		Status: status,
		Kind:   cierrors.KindForStatus(status),
		Detail: errorDetail(body),
	}
	if e.Detail == "" {
		e.Detail = http.StatusText(status)
	}
	switch {
	case status == http.StatusBadRequest:
		e.Hint = "Datadog rejected the request; check the log query syntax (dd_query rules in the config, --from/--to)"
	case status == http.StatusUnauthorized:
		e.Hint = "DD_API_KEY / DD_APP_KEY were rejected; check the keys and that --dd-site matches the organisation's region (datadoghq.eu vs datadoghq.com)"
	case status == http.StatusForbidden:
		e.Hint = "the application key lacks the logs_read_data scope (or the API key and application key belong to different organisations); re-create the application key with logs_read_data"
	case status == http.StatusTooManyRequests:
		e.Hint = fmt.Sprintf("rate limited: the logs aggregate API allows %d requests per %s per organisation and gscx paces itself, so another client (ddx, a dashboard, another gscx) is probably sharing the quota; retry shortly",
			c.pace.limitNow(), c.pace.Period().Round(time.Second))
	case status >= 500:
		e.Hint = "Datadog returned a server error after retries; check status.datadoghq.eu and retry"
	}
	return e
}

func (p *pacer) limitNow() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.limit
}

// errorDetail extracts the message from Datadog's {"errors":[...]} envelope
// (strings or objects with a "detail"/"title"); other bodies are redacted and
// truncated.
func errorDetail(body []byte) string {
	if len(body) == 0 {
		return ""
	}
	var env struct {
		Errors []json.RawMessage `json:"errors"`
	}
	if json.Unmarshal(body, &env) == nil && len(env.Errors) > 0 {
		var msgs []string
		for _, raw := range env.Errors {
			var s string
			if json.Unmarshal(raw, &s) == nil {
				msgs = append(msgs, s)
				continue
			}
			var o struct{ Title, Detail string }
			if json.Unmarshal(raw, &o) == nil && (o.Title != "" || o.Detail != "") {
				msgs = append(msgs, strings.TrimSpace(o.Title+" "+o.Detail))
			}
		}
		if len(msgs) > 0 {
			return strings.Join(msgs, "; ")
		}
	}
	return redact.Body(body)
}
