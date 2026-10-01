package commands

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nicolasacchi/gscx/internal/config"
	"github.com/nicolasacchi/gscx/internal/datadog"
	"github.com/nicolasacchi/gscx/internal/pagetype"
	"github.com/nicolasacchi/gscx/internal/store"
)

// ---- fake aggregator ----

type fakeAnswer struct {
	count   int64
	by      []datadog.Bucket
	daily   map[string]int64
	dailyBy []datadog.DailyBucket
	meta    datadog.Meta
	err     error
}

type fakeAgg struct {
	mu      sync.Mutex
	answers map[string]fakeAnswer
	methods map[string]int
	queries []string

	inflight, maxInflight int32
	hold                  time.Duration
}

func newFakeAgg(answers map[string]fakeAnswer) *fakeAgg {
	return &fakeAgg{answers: answers, methods: map[string]int{}}
}

func (f *fakeAgg) answer(method, query string) fakeAnswer {
	n := atomic.AddInt32(&f.inflight, 1)
	defer atomic.AddInt32(&f.inflight, -1)
	for {
		m := atomic.LoadInt32(&f.maxInflight)
		if n <= m || atomic.CompareAndSwapInt32(&f.maxInflight, m, n) {
			break
		}
	}
	if f.hold > 0 {
		time.Sleep(f.hold)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.methods[method]++
	f.queries = append(f.queries, query)
	return f.answers[query] // zero answer for unknown queries
}

func (f *fakeAgg) Count(_ context.Context, q string, _, _ time.Time) (int64, datadog.Meta, error) {
	a := f.answer("Count", q)
	return a.count, a.meta, a.err
}

func (f *fakeAgg) CountBy(_ context.Context, q string, _, _ time.Time, _ string, _ int) ([]datadog.Bucket, datadog.Meta, error) {
	a := f.answer("CountBy", q)
	return a.by, a.meta, a.err
}

func (f *fakeAgg) CountDaily(_ context.Context, q string, _, _ time.Time) (map[string]int64, datadog.Meta, error) {
	a := f.answer("CountDaily", q)
	return a.daily, a.meta, a.err
}

func (f *fakeAgg) CountDailyBy(_ context.Context, q string, _, _ time.Time, _ string, _ int) ([]datadog.DailyBucket, datadog.Meta, error) {
	a := f.answer("CountDailyBy", q)
	return a.dailyBy, a.meta, a.err
}

// ---- fixtures ----

var (
	crawlT0  = time.Date(2026, 9, 28, 0, 0, 0, 0, time.UTC)
	crawlT1  = time.Date(2026, 9, 30, 0, 0, 0, 0, time.UTC)
	crawlNow = time.Date(2026, 10, 1, 8, 0, 0, 0, time.UTC)

	crawlTestDD = config.Datadog{Query: "svc", BotQuery: "bot:g", VerifiedQuery: "net:g"}

	crawlTestRules = []pagetype.Rule{
		{Name: "a", Path: `^/a`, DDQuery: "x:a"},
		{Name: "b", Path: `^/b`, DDQuery: "x:b"},
		{Name: "c", Path: `^/c`, DDQuery: "x:c"},
	}
)

const (
	crawlBaseVerified = "svc bot:g net:g"
	crawlBaseLoose    = "svc bot:g"
)

func crawlQueries(base string) map[string]string {
	qs, _ := datadog.TypeQueries(base, crawlTestRules)
	m := map[string]string{"total": base}
	for _, q := range qs {
		m[q.Type] = q.Query
	}
	return m
}

func testParams() crawlParams {
	return crawlParams{Datadog: crawlTestDD, Rules: crawlTestRules, From: crawlT0, To: crawlT1, Now: crawlNow, Concurrency: 4}
}

func byType(t *testing.T, res *crawlResult, name string) crawlTypeOut {
	t.Helper()
	for _, ty := range res.ByType {
		if ty.Type == name {
			return ty
		}
	}
	t.Fatalf("no type %q in %+v", name, res.ByType)
	return crawlTypeOut{}
}

func typeNames(res *crawlResult) []string {
	var out []string
	for _, ty := range res.ByType {
		out = append(out, ty.Type)
	}
	return out
}

// ---- collectCrawl ----

func TestCollectCrawlPlain(t *testing.T) {
	q := crawlQueries(crawlBaseVerified)
	agg := newFakeAgg(map[string]fakeAnswer{
		q["total"]:           {count: 1000},
		"svc bot:g -(net:g)": {count: 7},
		q["a"]:               {count: 600},
		q["b"]:               {count: 250},
		q["c"]:               {count: 0},
	})
	res, err := collectCrawl(context.Background(), agg, testParams())
	if err != nil {
		t.Fatal(err)
	}
	if got, want := typeNames(res), []string{"a", "b", "c", "other"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("types = %v, want %v (rule order, then other)", got, want)
	}
	if res.Total != 1000 || res.Other != 150 || byType(t, res, "other").Requests != 150 {
		t.Errorf("total=%d other=%d", res.Total, res.Other)
	}
	if byType(t, res, "a").Share != 0.6 || byType(t, res, "b").Share != 0.25 || byType(t, res, "other").Share != 0.15 {
		t.Errorf("shares = %+v", res.ByType)
	}
	if !res.Verified || res.Spoofed == nil || *res.Spoofed != 7 {
		t.Errorf("verified=%v spoofed=%v", res.Verified, res.Spoofed)
	}
	if res.BaseQuery != crawlBaseVerified || res.Source != "datadog:svc" {
		t.Errorf("base=%q source=%q", res.BaseQuery, res.Source)
	}
	if res.Window.From != "2026-09-28T00:00:00Z" || res.Window.To != "2026-09-30T00:00:00Z" {
		t.Errorf("window = %+v", res.Window)
	}
	if len(res.Warnings) != 0 || res.Partial {
		t.Errorf("warnings=%v partial=%v", res.Warnings, res.Partial)
	}
	if res.Note == "" || !strings.Contains(res.Note, "edge-cache") || !strings.Contains(res.Note, "7 days") {
		t.Errorf("note = %q", res.Note)
	}
	// total + spoofed + one query per type, all plain Count.
	if agg.methods["Count"] != 5 || len(agg.methods) != 1 {
		t.Errorf("calls = %v", agg.methods)
	}
	if byType(t, res, "a").Statuses != nil || byType(t, res, "a").Daily != nil {
		t.Error("statuses/daily must be absent without --by-status/--daily")
	}
}

func TestCollectCrawlUnverified(t *testing.T) {
	q := crawlQueries(crawlBaseLoose)
	agg := newFakeAgg(map[string]fakeAnswer{q["total"]: {count: 100}, q["a"]: {count: 40}})
	p := testParams()
	p.Unverified = true
	res, err := collectCrawl(context.Background(), agg, p)
	if err != nil {
		t.Fatal(err)
	}
	if res.Verified || res.Spoofed != nil || res.BaseQuery != crawlBaseLoose {
		t.Errorf("verified=%v spoofed=%v base=%q", res.Verified, res.Spoofed, res.BaseQuery)
	}
	if res.Total != 100 || res.Other != 60 {
		t.Errorf("total=%d other=%d", res.Total, res.Other)
	}
	if agg.methods["Count"] != 4 { // total + 3 types, no spoofed query
		t.Errorf("calls = %v", agg.methods)
	}
	for _, s := range agg.queries {
		if strings.Contains(s, "net:g") {
			t.Errorf("unverified mode must not use the verified filter: %q", s)
		}
	}
}

func TestCollectCrawlTypeQueriesAreDisjoint(t *testing.T) {
	agg := newFakeAgg(nil)
	if _, err := collectCrawl(context.Background(), agg, testParams()); err != nil {
		t.Fatal(err)
	}
	have := map[string]bool{}
	for _, s := range agg.queries {
		have[s] = true
	}
	q := crawlQueries(crawlBaseVerified)
	if !have[q["c"]] || !strings.Contains(q["c"], "-(x:a) -(x:b)") || strings.Contains(q["a"], "-(") {
		t.Errorf("queries sent: %v", agg.queries)
	}
}

func TestCollectCrawlOtherClampedWhenTypesExceedTotal(t *testing.T) {
	q := crawlQueries(crawlBaseVerified)
	agg := newFakeAgg(map[string]fakeAnswer{q["total"]: {count: 100}, q["a"]: {count: 80}, q["b"]: {count: 40}})
	res, err := collectCrawl(context.Background(), agg, testParams())
	if err != nil {
		t.Fatal(err)
	}
	if res.Other != 0 || byType(t, res, "other").Requests != 0 {
		t.Errorf("other = %d, want clamped 0", res.Other)
	}
	if len(res.Warnings) != 1 || !strings.Contains(res.Warnings[0], "exceed the total by 20") || !strings.Contains(res.Warnings[0], "overlap") {
		t.Errorf("warnings = %v", res.Warnings)
	}
}

func TestCollectCrawlRuleWithoutDDQueryWarns(t *testing.T) {
	p := testParams()
	p.Rules = []pagetype.Rule{{Name: "a", Path: `^/a`, DDQuery: "x:a"}, {Name: "nodd", Path: `^/n`}}
	agg := newFakeAgg(nil)
	res, err := collectCrawl(context.Background(), agg, p)
	if err != nil {
		t.Fatal(err)
	}
	if got := typeNames(res); !reflect.DeepEqual(got, []string{"a", "other"}) {
		t.Errorf("types = %v", got)
	}
	if len(res.Warnings) != 1 || !strings.Contains(res.Warnings[0], `"nodd" has no dd_query`) {
		t.Errorf("warnings = %v", res.Warnings)
	}
}

func TestCollectCrawlPartialMeta(t *testing.T) {
	q := crawlQueries(crawlBaseVerified)
	agg := newFakeAgg(map[string]fakeAnswer{
		q["total"]: {count: 100},
		q["a"]:     {count: 10, meta: datadog.Meta{Status: "timeout"}},
	})
	res, err := collectCrawl(context.Background(), agg, testParams())
	if err != nil {
		t.Fatal(err)
	}
	if !res.Partial || len(res.Warnings) != 1 || !strings.Contains(res.Warnings[0], `"timeout"`) || !strings.Contains(res.Warnings[0], `"a"`) {
		t.Errorf("partial=%v warnings=%v", res.Partial, res.Warnings)
	}
}

func TestCollectCrawlByStatus(t *testing.T) {
	q := crawlQueries(crawlBaseVerified)
	agg := newFakeAgg(map[string]fakeAnswer{
		q["total"]:           {by: []datadog.Bucket{{Value: "200", Count: 90}, {Value: "304", Count: 8}, {Value: "404", Count: 2}}},
		q["a"]:               {by: []datadog.Bucket{{Value: "304", Count: 5}, {Value: "200", Count: 70}}},
		q["b"]:               {by: []datadog.Bucket{{Value: "200", Count: 10}, {Value: "404", Count: 1}}},
		"svc bot:g -(net:g)": {count: 3},
	})
	p := testParams()
	p.ByStatus = true
	res, err := collectCrawl(context.Background(), agg, p)
	if err != nil {
		t.Fatal(err)
	}
	if res.Total != 100 || byType(t, res, "a").Requests != 75 || byType(t, res, "b").Requests != 11 || res.Other != 14 {
		t.Fatalf("total=%d a=%d b=%d other=%d", res.Total, byType(t, res, "a").Requests, byType(t, res, "b").Requests, res.Other)
	}
	a := *byType(t, res, "a").Statuses
	if len(a) != 2 || a[0].Status != "200" || a[0].Requests != 70 || a[1].Status != "304" || a[0].Share != 0.9333 {
		t.Errorf("a statuses (largest first, share of the type) = %+v", a)
	}
	// other: 200 -> 90-70-10, 304 -> 8-5, 404 -> 2-1
	o := *byType(t, res, "other").Statuses
	got := map[string]int64{}
	for _, s := range o {
		got[s.Status] = s.Requests
	}
	if !reflect.DeepEqual(got, map[string]int64{"200": 10, "304": 3, "404": 1}) {
		t.Errorf("other statuses = %v", got)
	}
	if st := byType(t, res, "c").Statuses; st == nil || len(*st) != 0 {
		t.Errorf("a type with no requests still carries an empty statuses list under --by-status, got %v", st)
	}
	if agg.methods["CountBy"] != 4 || agg.methods["Count"] != 1 { // total + 3 types by status; spoofed is a plain count
		t.Errorf("calls = %v", agg.methods)
	}
}

func TestCollectCrawlDaily(t *testing.T) {
	q := crawlQueries(crawlBaseVerified)
	agg := newFakeAgg(map[string]fakeAnswer{
		q["total"]: {daily: map[string]int64{"2026-09-28": 60, "2026-09-29": 40}},
		q["a"]:     {daily: map[string]int64{"2026-09-28": 30, "2026-09-29": 30}},
		q["b"]:     {daily: map[string]int64{"2026-09-29": 5}}, // 09-28 bucket missing -> zero
	})
	p := testParams()
	p.Daily = true
	res, err := collectCrawl(context.Background(), agg, p)
	if err != nil {
		t.Fatal(err)
	}
	if res.Total != 100 || byType(t, res, "a").Requests != 60 {
		t.Fatalf("total=%d a=%d (requests must be the sum of the daily buckets)", res.Total, byType(t, res, "a").Requests)
	}
	if got := byType(t, res, "b").Daily; !reflect.DeepEqual(got, map[string]int64{"2026-09-28": 0, "2026-09-29": 5}) {
		t.Errorf("b daily = %v (every day of the window must be present)", got)
	}
	if got := byType(t, res, "other").Daily; !reflect.DeepEqual(got, map[string]int64{"2026-09-28": 30, "2026-09-29": 5}) {
		t.Errorf("other daily = %v", got)
	}
	if !reflect.DeepEqual(res.Days, []crawlDayOut{{"2026-09-28", true}, {"2026-09-29", true}}) {
		t.Errorf("days = %+v", res.Days)
	}
	if agg.methods["CountDaily"] != 4 {
		t.Errorf("calls = %v", agg.methods)
	}
}

func TestCollectCrawlDailyMarksPartialEdgeDays(t *testing.T) {
	p := testParams()
	p.Daily = true
	p.From = time.Date(2026, 9, 28, 7, 18, 0, 0, time.UTC)
	p.To = time.Date(2026, 9, 30, 7, 18, 0, 0, time.UTC)
	res, err := collectCrawl(context.Background(), newFakeAgg(nil), p)
	if err != nil {
		t.Fatal(err)
	}
	want := []crawlDayOut{{"2026-09-28", false}, {"2026-09-29", true}, {"2026-09-30", false}}
	if !reflect.DeepEqual(res.Days, want) {
		t.Errorf("days = %+v, want %+v", res.Days, want)
	}
	found := false
	for _, w := range res.Warnings {
		found = found || (strings.Contains(w, "2026-09-28") && strings.Contains(w, "2026-09-30") && strings.Contains(w, "complete days only"))
	}
	if !found {
		t.Errorf("warnings = %v", res.Warnings)
	}
}

func TestCollectCrawlDailyByStatus(t *testing.T) {
	q := crawlQueries(crawlBaseVerified)
	agg := newFakeAgg(map[string]fakeAnswer{
		q["total"]: {dailyBy: []datadog.DailyBucket{
			{Value: "200", Days: map[string]int64{"2026-09-28": 50, "2026-09-29": 40}},
			{Value: "404", Days: map[string]int64{"2026-09-28": 10}},
		}},
		q["a"]: {dailyBy: []datadog.DailyBucket{
			{Value: "200", Days: map[string]int64{"2026-09-28": 20, "2026-09-29": 25}},
			{Value: "404", Days: map[string]int64{"2026-09-28": 4}},
		}},
	})
	p := testParams()
	p.Daily, p.ByStatus = true, true
	res, err := collectCrawl(context.Background(), agg, p)
	if err != nil {
		t.Fatal(err)
	}
	a := byType(t, res, "a")
	if a.Requests != 49 || !reflect.DeepEqual(a.Daily, map[string]int64{"2026-09-28": 24, "2026-09-29": 25}) {
		t.Errorf("a = %+v", a)
	}
	if st := *a.Statuses; len(st) != 2 || st[0].Status != "200" || st[0].Requests != 45 || st[1].Requests != 4 {
		t.Errorf("a statuses = %+v", st)
	}
	o := byType(t, res, "other")
	if o.Requests != 51 || o.dailyStatus["200"]["2026-09-28"] != 30 || o.dailyStatus["404"]["2026-09-28"] != 6 || o.dailyStatus["200"]["2026-09-29"] != 15 {
		t.Errorf("other = %+v", o)
	}
	if agg.methods["CountDailyBy"] != 4 {
		t.Errorf("calls = %v (one combined query per page type)", agg.methods)
	}
}

func TestCollectCrawlPropagatesErrorAndStops(t *testing.T) {
	q := crawlQueries(crawlBaseVerified)
	boom := errors.New("boom")
	agg := newFakeAgg(map[string]fakeAnswer{q["b"]: {err: boom}})
	if _, err := collectCrawl(context.Background(), agg, testParams()); !errors.Is(err, boom) {
		t.Fatalf("err = %v", err)
	}
}

func TestCollectCrawlRespectsConcurrency(t *testing.T) {
	agg := newFakeAgg(nil)
	agg.hold = 15 * time.Millisecond
	p := testParams()
	p.Rules = nil
	for i := 0; i < 10; i++ {
		p.Rules = append(p.Rules, pagetype.Rule{Name: string(rune('a' + i)), Path: "x", DDQuery: "x:" + string(rune('a'+i))})
	}
	p.Concurrency = 2
	if _, err := collectCrawl(context.Background(), agg, p); err != nil {
		t.Fatal(err)
	}
	if m := atomic.LoadInt32(&agg.maxInflight); m != 2 {
		t.Errorf("max in flight = %d, want exactly the --concurrency bound 2", m)
	}
}

func TestCollectCrawlHonoursCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := collectCrawl(ctx, newFakeAgg(nil), testParams()); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
}

// ---- time parsing ----

func TestCrawlParseTime(t *testing.T) {
	now := time.Date(2026, 10, 1, 8, 30, 0, 0, time.UTC)
	cases := []struct {
		in      string
		want    time.Time
		wantErr bool
	}{
		{"", now, false},
		{"now", now, false},
		{"24h", now.Add(-24 * time.Hour), false},
		{"7d", now.Add(-7 * 24 * time.Hour), false},
		{"90m", now.Add(-90 * time.Minute), false},
		{"2w", now.Add(-14 * 24 * time.Hour), false},
		{"2026-09-24", time.Date(2026, 9, 24, 0, 0, 0, 0, time.UTC), false},
		{"2026-09-24T10:00:00Z", time.Date(2026, 9, 24, 10, 0, 0, 0, time.UTC), false},
		{"2026-09-24T12:00:00+02:00", time.Date(2026, 9, 24, 10, 0, 0, 0, time.UTC), false},
		{"yesterday", time.Time{}, true},
		{"7x", time.Time{}, true},
		{"-3d", time.Time{}, true},
	}
	for _, tc := range cases {
		got, err := crawlParseTime(tc.in, now)
		if (err != nil) != tc.wantErr {
			t.Errorf("%q: err = %v", tc.in, err)
			continue
		}
		if !tc.wantErr && !got.Equal(tc.want) {
			t.Errorf("%q = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestCrawlWindow(t *testing.T) {
	now := time.Date(2026, 10, 1, 8, 30, 0, 0, time.UTC)
	from, to, err := crawlWindow("7d", "", now)
	if err != nil || !to.Equal(now) || !from.Equal(now.Add(-7*24*time.Hour)) {
		t.Fatalf("from=%v to=%v err=%v", from, to, err)
	}
	if _, _, err := crawlWindow("1h", "3h", now); err == nil || !strings.Contains(err.Error(), "must be before") {
		t.Errorf("inverted window: %v", err)
	}
	if _, _, err := crawlWindow("bogus", "", now); err == nil || !strings.Contains(err.Error(), "--from") {
		t.Errorf("bad from: %v", err)
	}
	if _, _, err := crawlWindow("1d", "bogus", now); err == nil || !strings.Contains(err.Error(), "--to") {
		t.Errorf("bad to: %v", err)
	}
}

func TestCrawlDaysAndCompleteness(t *testing.T) {
	from := time.Date(2026, 9, 28, 7, 0, 0, 0, time.UTC)
	to := time.Date(2026, 9, 30, 0, 0, 0, 0, time.UTC)
	if got := crawlDays(from, to); !reflect.DeepEqual(got, []string{"2026-09-28", "2026-09-29"}) {
		t.Errorf("days = %v (a window ending at midnight does not touch that day)", got)
	}
	if crawlDayComplete("2026-09-28", from, to) || !crawlDayComplete("2026-09-29", from, to) {
		t.Error("only 09-29 lies fully inside the window")
	}
	if crawlDayComplete("garbage", from, to) {
		t.Error("unparseable day must not be complete")
	}
}

func TestCrawlCredentialPrecedence(t *testing.T) {
	t.Setenv("DD_API_KEY", "env-api")
	t.Setenv("DD_APP_KEY", "env-app")
	t.Setenv("DD_SITE", "datadoghq.com")
	d := config.Datadog{APIKey: "cfg-api", AppKey: "cfg-app", Site: "datadoghq.eu"}
	t.Cleanup(func() { flagDDAPIKey, flagDDAppKey, flagDDSite = "", "", "" })

	flagDDAPIKey, flagDDAppKey, flagDDSite = "", "", ""
	if a, b, s := ddCredentials(d); a != "env-api" || b != "env-app" || s != "datadoghq.com" {
		t.Errorf("env must beat config: %q %q %q", a, b, s)
	}
	flagDDAPIKey, flagDDAppKey, flagDDSite = "flag-api", "flag-app", "us3.datadoghq.com"
	if a, b, s := ddCredentials(d); a != "flag-api" || b != "flag-app" || s != "us3.datadoghq.com" {
		t.Errorf("flag must beat env: %q %q %q", a, b, s)
	}
	t.Setenv("DD_API_KEY", "")
	t.Setenv("DD_APP_KEY", "")
	t.Setenv("DD_SITE", "")
	flagDDAPIKey, flagDDAppKey, flagDDSite = "", "", ""
	if a, b, s := ddCredentials(d); a != "cfg-api" || b != "cfg-app" || s != "datadoghq.eu" {
		t.Errorf("config is the fallback: %q %q %q", a, b, s)
	}
}

// ---- saveCrawl ----

func saveFixture(t *testing.T) (*store.Store, *crawlResult) {
	t.Helper()
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "gscx.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	res := &crawlResult{
		Days: []crawlDayOut{{"2026-09-28", false}, {"2026-09-29", true}, {"2026-09-30", true}, {"2026-10-01", false}},
		ByType: []crawlTypeOut{
			{Type: "a", Daily: map[string]int64{"2026-09-28": 5, "2026-09-29": 60, "2026-09-30": 0, "2026-10-01": 1},
				dailyStatus: map[string]map[string]int64{"200": {"2026-09-29": 55}, "404": {"2026-09-29": 5}}},
			{Type: "other", Daily: map[string]int64{"2026-09-28": 1, "2026-09-29": 10, "2026-09-30": 0, "2026-10-01": 0}},
		},
	}
	return st, res
}

func TestSaveCrawlStoresCompleteNonEmptyDays(t *testing.T) {
	ctx := context.Background()
	st, res := saveFixture(t)
	key := crawlKey(crawlTestDD, false)
	saved, err := saveCrawl(ctx, st, res, key, crawlNow)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(saved.Days, []string{"2026-09-29"}) || saved.Rows != 4 { // a, a/200, a/404, other
		t.Fatalf("saved = %+v", saved)
	}
	reasons := map[string]string{}
	for _, s := range saved.Skipped {
		reasons[s.Day] = s.Reason
	}
	if !strings.Contains(reasons["2026-09-28"], "partial") || !strings.Contains(reasons["2026-10-01"], "partial") || !strings.Contains(reasons["2026-09-30"], "no logs") {
		t.Errorf("skipped = %+v", saved.Skipped)
	}

	rows, _ := st.CrawlRows(ctx, store.CrawlFilter{Source: key.Source, Bot: key.Bot, Verified: true})
	if len(rows) != 2 || rows[0].PageType != "a" || rows[0].Requests != 60 || rows[1].PageType != "other" {
		t.Errorf("all-status rows = %+v", rows)
	}
	rows, _ = st.CrawlRows(ctx, store.CrawlFilter{Source: key.Source, Bot: key.Bot, Verified: true, Status: "404"})
	if len(rows) != 1 || rows[0].Requests != 5 {
		t.Errorf("404 rows = %+v", rows)
	}

	// Saving again is a no-op on the data.
	again, err := saveCrawl(ctx, st, res, key, crawlNow.Add(time.Hour))
	if err != nil || again.Rows != saved.Rows {
		t.Fatalf("again = %+v err = %v", again, err)
	}
	var n int
	if err := st.DB().QueryRow(`SELECT COUNT(*) FROM crawl_daily`).Scan(&n); err != nil || n != 4 {
		t.Errorf("rows after re-save = %d err=%v", n, err)
	}
}

func TestSaveCrawlReportsKeptLargerDay(t *testing.T) {
	ctx := context.Background()
	st, res := saveFixture(t)
	key := crawlKey(crawlTestDD, false)
	if _, err := saveCrawl(ctx, st, res, key, crawlNow); err != nil {
		t.Fatal(err)
	}
	res.ByType[0].Daily["2026-09-29"] = 20 // logs expired: smaller snapshot of the same day
	res.ByType[0].dailyStatus = nil
	saved, err := saveCrawl(ctx, st, res, key, crawlNow)
	if err != nil {
		t.Fatal(err)
	}
	if saved.Rows != 0 || len(saved.Days) != 0 {
		t.Fatalf("saved = %+v", saved)
	}
	found := false
	for _, s := range saved.Skipped {
		found = found || (s.Day == "2026-09-29" && strings.Contains(s.Reason, "larger"))
	}
	if !found {
		t.Errorf("skipped = %+v", saved.Skipped)
	}
}

// ---- end to end through cobra, against a fake Datadog ----

func fakeDatadog(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Filter  struct{ Query string }
			Compute []struct{ Type string }
			GroupBy []struct{ Facet string } `json:"group_by"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("bad request: %v", err)
		}
		if r.Header.Get("DD-API-KEY") != "k1" || r.Header.Get("DD-APPLICATION-KEY") != "k2" {
			t.Errorf("credentials from flags not sent: %v", r.Header)
		}
		// total = 2000, spoofed = 5, every typed query = 100 (so other = 2000 - n*100).
		n := int64(100)
		switch {
		case !strings.Contains(req.Filter.Query, " ("):
			n = 2000
			if strings.Contains(req.Filter.Query, "-(") {
				n = 5
			}
		}
		w.Header().Set("Content-Type", "application/json")
		if len(req.Compute) > 0 && req.Compute[0].Type == "timeseries" {
			_, _ = w.Write([]byte(`{"meta":{"status":"done"},"data":{"buckets":[{"by":{},"computes":{"c0":[` +
				`{"time":"2026-09-28T00:00:00.000Z","value":` + itoa(n/2) + `},{"time":"2026-09-29T00:00:00.000Z","value":` + itoa(n-n/2) + `}]}}]}}`))
			return
		}
		_, _ = w.Write([]byte(`{"meta":{"status":"done"},"data":{"buckets":[{"by":{},"computes":{"c0":` + itoa(n) + `}}]}}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func itoa(n int64) string {
	b, _ := json.Marshal(n)
	return string(b)
}

func runRoot(t *testing.T, args ...string) []byte {
	t.Helper()
	var out, errOut bytes.Buffer
	rootCmd.SetOut(&out)
	rootCmd.SetErr(&errOut)
	rootCmd.SetArgs(args)
	if err := rootCmd.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("gscx %s: %v\nstderr: %s", strings.Join(args, " "), err, errOut.String())
	}
	return out.Bytes()
}

func TestCrawlEndToEndSaveAndHistory(t *testing.T) {
	srv := fakeDatadog(t)
	orig := newDatadogClient
	newDatadogClient = func(o datadog.Options) (*datadog.Client, error) {
		o.RateLimit, o.RatePeriod = 1000, time.Millisecond
		o.RetryDelay = func(int, int) time.Duration { return 0 }
		return orig(o)
	}
	t.Cleanup(func() {
		newDatadogClient = orig
		flagCrawlFrom, flagCrawlTo, flagCrawlDaily, flagCrawlSave, flagCrawlByStatus, flagCrawlUnverified = "7d", "", false, false, false, false
		flagCrawlSince, flagDDAPIKey, flagDDAppKey, flagDDSite, flagDB, flagConfig, flagJQ = "30d", "", "", "", "", "", ""
		flagLimit = 0
	})
	dir := t.TempDir()
	common := []string{"--config", filepath.Join(dir, "none.toml"), "--db", filepath.Join(dir, "gscx.db")}

	args := append([]string{"crawl", "--from", "2026-09-28", "--to", "2026-09-30", "--daily", "--save",
		"--dd-api-key", "k1", "--dd-app-key", "k2", "--dd-site", srv.URL, "--concurrency", "3"}, common...)
	var res crawlResult
	if err := json.Unmarshal(runRoot(t, args...), &res); err != nil {
		t.Fatal(err)
	}
	nTypes := len(pagetype.Defaults())
	if res.Total != 2000 || res.Spoofed == nil || *res.Spoofed != 5 || len(res.ByType) != nTypes+1 {
		t.Fatalf("total=%d spoofed=%v types=%d", res.Total, res.Spoofed, len(res.ByType))
	}
	wantOther := int64(2000 - 100*nTypes)
	if res.Other != wantOther {
		t.Errorf("other = %d, want %d", res.Other, wantOther)
	}
	if res.Saved == nil || res.Saved.Rows != 2*(nTypes+1) || !reflect.DeepEqual(res.Saved.Days, []string{"2026-09-28", "2026-09-29"}) {
		t.Fatalf("saved = %+v", res.Saved)
	}

	var hist struct {
		Source   string
		Verified bool
		Types    []string
		Days     int
		Rows     []store.CrawlDay
	}
	hargs := append([]string{"crawl", "history", "--since", "2026-09-01"}, common...)
	if err := json.Unmarshal(runRoot(t, hargs...), &hist); err != nil {
		t.Fatal(err)
	}
	if hist.Source != "datadog:service:web-1000farmacie" || !hist.Verified || hist.Days != 2 || len(hist.Types) != nTypes+1 {
		t.Fatalf("history = %+v", hist)
	}
	if hist.Rows[0].Day != "2026-09-29" || hist.Rows[1].Day != "2026-09-28" {
		t.Errorf("rows must be newest first: %+v", hist.Rows)
	}
	if hist.Rows[0].Total != 2000/2 || hist.Rows[0].ByType["product"] != 50 || hist.Rows[0].ByType["other"] != wantOther/2 {
		t.Errorf("pivot = %+v", hist.Rows[0])
	}
	if hist.Types[len(hist.Types)-1] != "other" || hist.Types[0] != "home" {
		t.Errorf("types = %v (rule order, other last)", hist.Types)
	}

	// --limit keeps the newest days and flags truncation.
	var capped struct {
		Rows      []store.CrawlDay
		Truncated struct{ Shown, Total int }
	}
	largs := append([]string{"crawl", "history", "--since", "2026-09-01", "--limit", "1"}, common...)
	if err := json.Unmarshal(runRoot(t, largs...), &capped); err != nil {
		t.Fatal(err)
	}
	if len(capped.Rows) != 1 || capped.Rows[0].Day != "2026-09-29" || capped.Truncated.Total != 2 {
		t.Errorf("capped = %+v", capped)
	}
}

func TestCrawlSaveRequiresDaily(t *testing.T) {
	t.Cleanup(func() { flagCrawlSave, flagCrawlDaily = false, false })
	rootCmd.SetOut(&bytes.Buffer{})
	rootCmd.SetErr(&bytes.Buffer{})
	rootCmd.SetArgs([]string{"crawl", "--save", "--config", filepath.Join(t.TempDir(), "none.toml")})
	err := rootCmd.ExecuteContext(context.Background())
	if err == nil || !strings.Contains(err.Error(), "--save needs --daily") {
		t.Fatalf("err = %v", err)
	}
}

func TestCrawlHistoryEmptyStore(t *testing.T) {
	dir := t.TempDir()
	t.Cleanup(func() { flagDB, flagConfig, flagCrawlSince = "", "", "30d" })
	var out struct {
		Rows  []store.CrawlDay
		Types []string
		Note  string
	}
	raw := runRoot(t, "crawl", "history", "--config", filepath.Join(dir, "none.toml"), "--db", filepath.Join(dir, "gscx.db"))
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	if out.Rows == nil || len(out.Rows) != 0 || out.Types == nil || !strings.Contains(out.Note, "--daily --save") {
		t.Errorf("empty history output = %s", raw)
	}
}
