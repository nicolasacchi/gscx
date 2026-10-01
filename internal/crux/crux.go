// Package crux is a minimal client for the Chrome UX Report API
// (https://developer.chrome.com/docs/crux/api): real-user Core Web Vitals for
// an origin or a URL, as a 28-day aggregate (records:queryRecord) or a series
// of weekly aggregates (records:queryHistoryRecord).
//
// Authentication is an API key sent as the ?key= query parameter. The key is
// never logged and is scrubbed from transport errors, which otherwise embed
// the full request URL.
//
// A 404 NOT_FOUND means Google holds no data for the origin or URL (too little
// traffic, or not publicly discoverable); it is reported as ErrNoData, not as
// a failure.
package crux

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nicolasacchi/clicore/cierrors"
	"github.com/nicolasacchi/clicore/httpclient"
)

const (
	// DefaultBaseURL is the CrUX API origin.
	DefaultBaseURL = "https://chromeuxreport.googleapis.com"
	recordPath     = "/v1/records:queryRecord"
	historyPath    = "/v1/records:queryHistoryRecord"

	// DefaultMinInterval spaces requests to stay under the API's quota of 150
	// queries per minute per project.
	DefaultMinInterval = 450 * time.Millisecond

	// MaxHistoryPeriods is the largest collectionPeriodCount the history API accepts.
	MaxHistoryPeriods = 40
)

// ErrNoData is returned when CrUX has no data for the requested origin or URL.
var ErrNoData = errors.New("crux: no data for this origin or URL")

// Form factors accepted by the API. The empty string aggregates all of them.
const (
	FormFactorAll     = ""
	FormFactorPhone   = "PHONE"
	FormFactorDesktop = "DESKTOP"
	FormFactorTablet  = "TABLET"
)

// Target selects what to query: exactly one of Origin or URL, optionally
// narrowed to a form factor.
type Target struct {
	Origin     string
	URL        string
	FormFactor string
}

// Options configures a Client.
type Options struct {
	APIKey string
	// BaseURL overrides DefaultBaseURL (tests).
	BaseURL    string
	HTTPClient *http.Client
	UserAgent  string
	// Logf receives verbose request/response lines. The API key is never
	// included. May be nil.
	Logf func(format string, args ...any)
	// MinInterval is the minimum time between two request starts (zero: no
	// pacing). Production callers pass DefaultMinInterval.
	MinInterval time.Duration
	// MaxRetries bounds retries of a 429/5xx response (default 3).
	MaxRetries int
}

// Client queries the CrUX API.
type Client struct {
	http     *httpclient.Client
	base     string
	key      string
	logf     func(string, ...any)
	interval time.Duration

	mu   sync.Mutex
	next time.Time
}

// New builds a client. A missing key is reported as an auth error before any
// request is made.
func New(o Options) (*Client, error) {
	if strings.TrimSpace(o.APIKey) == "" {
		return nil, &cierrors.APIError{
			Kind:   "auth_failed",
			Detail: "a CrUX API key is required",
			Hint:   "set GSCX_CRUX_API_KEY (or --crux-key, or [crux] api_key in the config file): create an API key in Google Cloud Console (APIs & Services → Credentials) and enable the Chrome UX Report API for its project",
		}
	}
	base := strings.TrimRight(o.BaseURL, "/")
	if base == "" {
		base = DefaultBaseURL
	}
	hopts := []httpclient.Option{}
	if o.HTTPClient != nil {
		hopts = append(hopts, httpclient.WithHTTPClient(o.HTTPClient))
	}
	if o.MaxRetries > 0 {
		hopts = append(hopts, httpclient.WithMaxRetries(o.MaxRetries))
	}
	return &Client{
		http:     httpclient.New(base, uaAuth{ua: o.UserAgent}, hopts...),
		base:     base,
		key:      o.APIKey,
		logf:     o.Logf,
		interval: o.MinInterval,
	}, nil
}

// uaAuth only sets the User-Agent: the key travels in the query string.
type uaAuth struct{ ua string }

func (a uaAuth) Apply(_ context.Context, req *http.Request, _ int) error {
	if a.ua != "" {
		req.Header.Set("User-Agent", a.ua)
	}
	return nil
}

func (uaAuth) OnUnauthorized() bool { return false }

// ---- public result types ----

// Key echoes what a record describes.
type Key struct {
	Origin     string
	URL        string
	FormFactor string
}

// Period is a collection period, dates as YYYY-MM-DD.
type Period struct{ First, Last string }

// Bin is one histogram bucket of a record. End is nil for the open last bin.
type Bin struct {
	Start   float64
	End     *float64
	Density float64
}

// RecordMetric is one metric of a 28-day record.
type RecordMetric struct {
	// P75 is nil when the API returned no percentile.
	P75       *float64
	Histogram []Bin
}

// Densities returns the share of page loads rated good, needs-improvement
// and poor (the first three histogram bins). ok is false if fewer bins came back.
func (m RecordMetric) Densities() (good, needsImprovement, poor float64, ok bool) {
	if len(m.Histogram) < 3 {
		return 0, 0, 0, false
	}
	return m.Histogram[0].Density, m.Histogram[1].Density, m.Histogram[2].Density, true
}

// Record is a 28-day aggregate for one origin or URL.
type Record struct {
	Key           Key
	NormalizedURL string
	Period        Period
	// Metrics are keyed by CrUX metric name (largest_contentful_paint, …).
	// Metrics without enough data are absent.
	Metrics map[string]RecordMetric
}

// HistBin is one histogram bucket of a history series.
type HistBin struct {
	Start float64
	End   *float64
	// Densities has one entry per collection period; nil marks a missing period.
	Densities []*float64
}

// HistoryMetric is one metric's weekly series.
type HistoryMetric struct {
	// P75s has one entry per collection period; nil marks a missing period.
	P75s      []*float64
	Histogram []HistBin
}

// History is a series of weekly aggregates, oldest first.
type History struct {
	Key     Key
	Periods []Period
	Metrics map[string]HistoryMetric
}

// ---- requests ----

type queryBody struct {
	Origin                string   `json:"origin,omitempty"`
	URL                   string   `json:"url,omitempty"`
	FormFactor            string   `json:"formFactor,omitempty"`
	Metrics               []string `json:"metrics,omitempty"`
	CollectionPeriodCount int      `json:"collectionPeriodCount,omitempty"`
}

func newValidation(detail, hint string) *cierrors.APIError {
	return &cierrors.APIError{Status: 400, Kind: "validation", Detail: detail, Hint: hint}
}

func (t Target) body(metrics []string) (queryBody, error) {
	switch {
	case t.Origin == "" && t.URL == "":
		return queryBody{}, newValidation("a CrUX query needs an origin or a url", "e.g. https://www.example.com")
	case t.Origin != "" && t.URL != "":
		return queryBody{}, newValidation("a CrUX query takes an origin or a url, not both", "")
	}
	switch t.FormFactor {
	case FormFactorAll, FormFactorPhone, FormFactorDesktop, FormFactorTablet:
	default:
		return queryBody{}, newValidation(fmt.Sprintf("unknown form factor %q", t.FormFactor), "use PHONE, DESKTOP, TABLET or ALL")
	}
	return queryBody{Origin: t.Origin, URL: t.URL, FormFactor: t.FormFactor, Metrics: metrics}, nil
}

// QueryRecord returns the latest 28-day record for t. metrics are CrUX metric
// names; none means every metric the API has. It returns ErrNoData when
// Google has no data for t.
func (c *Client) QueryRecord(ctx context.Context, t Target, metrics []string) (*Record, error) {
	body, err := t.body(metrics)
	if err != nil {
		return nil, err
	}
	raw, err := c.post(ctx, recordPath, body)
	if err != nil {
		return nil, err
	}
	return parseRecord(raw)
}

// QueryHistory returns up to periods weekly records for t, oldest first
// (periods 0 means the API default, 25; at most MaxHistoryPeriods). It
// returns ErrNoData when Google has no data for t.
func (c *Client) QueryHistory(ctx context.Context, t Target, metrics []string, periods int) (*History, error) {
	body, err := t.body(metrics)
	if err != nil {
		return nil, err
	}
	if periods < 0 || periods > MaxHistoryPeriods {
		return nil, newValidation(fmt.Sprintf("collection period count %d out of range", periods), fmt.Sprintf("use 1-%d weeks", MaxHistoryPeriods))
	}
	body.CollectionPeriodCount = periods
	raw, err := c.post(ctx, historyPath, body)
	if err != nil {
		return nil, err
	}
	return parseHistory(raw)
}

// ---- transport ----

func (c *Client) wait(ctx context.Context) error {
	if c.interval <= 0 {
		return nil
	}
	c.mu.Lock()
	now := time.Now()
	start := c.next
	if start.Before(now) {
		start = now
	}
	c.next = start.Add(c.interval)
	c.mu.Unlock()
	if d := time.Until(start); d > 0 {
		t := time.NewTimer(d)
		defer t.Stop()
		select {
		case <-t.C:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}

func (c *Client) post(ctx context.Context, path string, body queryBody) ([]byte, error) {
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("crux: encode request: %w", err)
	}
	if err := c.wait(ctx); err != nil {
		return nil, err
	}
	endpoint := c.base + path
	if c.logf != nil {
		c.logf("> POST %s %s", endpoint, payload) // no query string: it carries the key
	}
	start := time.Now()
	resp, hresp, err := c.http.Do(ctx, http.MethodPost, endpoint+"?key="+url.QueryEscape(c.key), payload, "application/json")
	status := 0
	if hresp != nil {
		status = hresp.StatusCode
	}
	if c.logf != nil {
		c.logf("< %d (%s)", status, time.Since(start).Round(time.Millisecond))
	}
	if err != nil {
		if status >= 400 {
			return nil, c.apiError(status, nil)
		}
		return nil, c.scrub(err)
	}
	if status < 200 || status >= 300 {
		if status == http.StatusNotFound {
			return nil, ErrNoData
		}
		return nil, c.apiError(status, resp)
	}
	return resp, nil
}

// scrub removes the API key from a transport error. url.Error carries the
// full request URL, query string included.
func (c *Client) scrub(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		if i := strings.IndexByte(ue.URL, '?'); i >= 0 {
			ue.URL = ue.URL[:i]
		}
	}
	msg := err.Error()
	for _, k := range []string{c.key, url.QueryEscape(c.key)} {
		if strings.Contains(msg, k) {
			return errors.New(strings.ReplaceAll(msg, k, "***"))
		}
	}
	return err
}

// apiError maps a failed response to the fleet error type. body may be empty
// (httpclient drops it once 429/5xx retries are exhausted).
func (c *Client) apiError(status int, body []byte) *cierrors.APIError {
	var env struct {
		Error struct {
			Message string `json:"message"`
			Status  string `json:"status"`
			Details []struct {
				Reason string `json:"reason"`
			} `json:"details"`
		} `json:"error"`
	}
	_ = json.Unmarshal(body, &env)
	e := &cierrors.APIError{Status: status, Kind: cierrors.KindForStatus(status), Detail: strings.TrimSpace(env.Error.Message)}
	if e.Detail == "" {
		e.Detail = http.StatusText(status)
	}
	// The message may echo the key ("API key not valid: <key>").
	e.Detail = strings.ReplaceAll(e.Detail, c.key, "***")
	keyInvalid := false
	for _, d := range env.Error.Details {
		keyInvalid = keyInvalid || d.Reason == "API_KEY_INVALID"
	}
	switch {
	case status == http.StatusBadRequest && (keyInvalid || strings.Contains(strings.ToLower(e.Detail), "api key")):
		e.Kind = "auth_failed"
		e.Hint = "the CrUX API key was rejected: check GSCX_CRUX_API_KEY / --crux-key / [crux] api_key"
	case status == http.StatusBadRequest:
		e.Hint = "CrUX rejected the request: the origin or url must be a full http(s) URL, and the form factor PHONE, DESKTOP or TABLET"
	case status == http.StatusUnauthorized, status == http.StatusForbidden:
		e.Hint = "enable the Chrome UX Report API for the API key's Google Cloud project (APIs & Services → Library) and check the key's API restrictions allow it"
	case status == http.StatusTooManyRequests:
		e.Hint = "CrUX allows 150 queries per minute per project and gscx paces itself, so another client sharing the project is probably using the quota; retry shortly or lower --concurrency"
	case status >= 500:
		e.Hint = "the CrUX API returned a server error after retries; retry later"
	}
	return e
}

// ---- parsing ----

// num decodes a JSON number, a numeric string (CrUX encodes cumulative layout
// shift that way, and histogram bounds of CLS), or a missing marker (null,
// "NaN", absent) into a value with an OK flag.
type num struct {
	V  float64
	OK bool
}

func (n *num) UnmarshalJSON(b []byte) error {
	s := strings.Trim(strings.TrimSpace(string(b)), `"`)
	if s == "" || s == "null" || strings.EqualFold(s, "nan") {
		*n = num{}
		return nil
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return fmt.Errorf("crux: unexpected numeric value %s", b)
	}
	*n = num{V: f, OK: true}
	return nil
}

func (n num) ptr() *float64 {
	if !n.OK {
		return nil
	}
	v := n.V
	return &v
}

type apiDate struct {
	Year  int `json:"year"`
	Month int `json:"month"`
	Day   int `json:"day"`
}

func (d apiDate) String() string {
	if d.Year == 0 {
		return ""
	}
	return fmt.Sprintf("%04d-%02d-%02d", d.Year, d.Month, d.Day)
}

type apiPeriod struct {
	FirstDate apiDate `json:"firstDate"`
	LastDate  apiDate `json:"lastDate"`
}

func (p apiPeriod) period() Period {
	return Period{First: p.FirstDate.String(), Last: p.LastDate.String()}
}

type apiKey struct {
	FormFactor string `json:"formFactor"`
	Origin     string `json:"origin"`
	URL        string `json:"url"`
}

func (k apiKey) key() Key { return Key{Origin: k.Origin, URL: k.URL, FormFactor: k.FormFactor} }

func parseRecord(raw []byte) (*Record, error) {
	var r struct {
		Record struct {
			Key     apiKey `json:"key"`
			Metrics map[string]struct {
				Histogram []struct {
					Start   num `json:"start"`
					End     num `json:"end"`
					Density num `json:"density"`
				} `json:"histogram"`
				Percentiles struct {
					P75 num `json:"p75"`
				} `json:"percentiles"`
			} `json:"metrics"`
			CollectionPeriod apiPeriod `json:"collectionPeriod"`
		} `json:"record"`
		URLNormalizationDetails struct {
			NormalizedURL string `json:"normalizedUrl"`
		} `json:"urlNormalizationDetails"`
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		return nil, fmt.Errorf("crux: decode record: %w", err)
	}
	out := &Record{
		Key:           r.Record.Key.key(),
		NormalizedURL: r.URLNormalizationDetails.NormalizedURL,
		Period:        r.Record.CollectionPeriod.period(),
		Metrics:       make(map[string]RecordMetric, len(r.Record.Metrics)),
	}
	for name, m := range r.Record.Metrics {
		rm := RecordMetric{P75: m.Percentiles.P75.ptr()}
		for _, b := range m.Histogram {
			rm.Histogram = append(rm.Histogram, Bin{Start: b.Start.V, End: b.End.ptr(), Density: b.Density.V})
		}
		out.Metrics[name] = rm
	}
	return out, nil
}

func parseHistory(raw []byte) (*History, error) {
	var r struct {
		Record struct {
			Key     apiKey `json:"key"`
			Metrics map[string]struct {
				HistogramTimeseries []struct {
					Start     num   `json:"start"`
					End       num   `json:"end"`
					Densities []num `json:"densities"`
				} `json:"histogramTimeseries"`
				PercentilesTimeseries struct {
					P75s []num `json:"p75s"`
				} `json:"percentilesTimeseries"`
			} `json:"metrics"`
			CollectionPeriods []apiPeriod `json:"collectionPeriods"`
		} `json:"record"`
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		return nil, fmt.Errorf("crux: decode history: %w", err)
	}
	out := &History{Key: r.Record.Key.key(), Metrics: make(map[string]HistoryMetric, len(r.Record.Metrics))}
	for _, p := range r.Record.CollectionPeriods {
		out.Periods = append(out.Periods, p.period())
	}
	for name, m := range r.Record.Metrics {
		hm := HistoryMetric{}
		for _, p := range m.PercentilesTimeseries.P75s {
			hm.P75s = append(hm.P75s, p.ptr())
		}
		for _, b := range m.HistogramTimeseries {
			hb := HistBin{Start: b.Start.V, End: b.End.ptr()}
			for _, d := range b.Densities {
				hb.Densities = append(hb.Densities, d.ptr())
			}
			hm.Histogram = append(hm.Histogram, hb)
		}
		out.Metrics[name] = hm
	}
	return out, nil
}
