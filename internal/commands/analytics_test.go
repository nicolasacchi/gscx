package commands

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/nicolasacchi/gscx/internal/gscdate"
	"github.com/nicolasacchi/gscx/internal/pagetype"
	searchconsole "google.golang.org/api/searchconsole/v1"
)

func analyticsTestEnd(t *testing.T) time.Time {
	t.Helper()
	d, err := gscdate.Parse("2026-09-27")
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func TestAnalyticsDateRange(t *testing.T) {
	end := analyticsTestEnd(t)
	cases := []struct {
		name       string
		start, end string
		span       int
		wantStart  string
		wantEnd    string
		wantErr    string
	}{
		{"defaults 7 days", "", "", 7, "2026-09-21", "2026-09-27", ""},
		{"defaults 28 days", "", "", 28, "2026-08-31", "2026-09-27", ""},
		{"explicit end shifts default start", "", "2026-09-10", 7, "2026-09-04", "2026-09-10", ""},
		{"explicit start keeps default end", "2026-09-01", "", 7, "2026-09-01", "2026-09-27", ""},
		{"both explicit", "2026-01-01", "2026-01-31", 7, "2026-01-01", "2026-01-31", ""},
		{"start after end", "2026-09-30", "", 7, "", "", "after end date"},
		{"bad start", "x", "", 7, "", "", "--start-date"},
		{"bad end", "", "2026-13-01", 7, "", "", "--end-date"},
	}
	for _, tc := range cases {
		s, e, err := analyticsDateRange(tc.start, tc.end, tc.span, end)
		if tc.wantErr != "" {
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("%s: err = %v, want containing %q", tc.name, err, tc.wantErr)
			}
			continue
		}
		if err != nil || s != tc.wantStart || e != tc.wantEnd {
			t.Errorf("%s: got %s..%s (%v), want %s..%s", tc.name, s, e, err, tc.wantStart, tc.wantEnd)
		}
	}
}

func TestBuildAnalyticsRequest(t *testing.T) {
	end := analyticsTestEnd(t)

	req, echo, err := buildAnalyticsRequest(analyticsQueryOpts{
		Dimensions: []string{"Query,page"},
		Filters:    []string{"page:includingRegex:https://x/y:z", "Country:equals:ita"},
		Type:       "googlenews",
		DataState:  "ALL",
		RowLimit:   500,
		StartRow:   10,
	}, end)
	if err != nil {
		t.Fatal(err)
	}
	if req.StartDate != "2026-09-21" || req.EndDate != "2026-09-27" {
		t.Errorf("dates = %s..%s", req.StartDate, req.EndDate)
	}
	if !reflect.DeepEqual(req.Dimensions, []string{"query", "page"}) {
		t.Errorf("dimensions = %v", req.Dimensions)
	}
	if req.Type != "googleNews" || req.DataState != "all" || req.AggregationType != "" {
		t.Errorf("type/state/agg = %q/%q/%q", req.Type, req.DataState, req.AggregationType)
	}
	if req.RowLimit != 500 || req.StartRow != 10 {
		t.Errorf("limit/start = %d/%d", req.RowLimit, req.StartRow)
	}
	if len(req.DimensionFilterGroups) != 1 || len(req.DimensionFilterGroups[0].Filters) != 2 {
		t.Fatalf("filter groups = %+v", req.DimensionFilterGroups)
	}
	wantEcho := analyticsRequestEcho{
		StartDate: "2026-09-21", EndDate: "2026-09-27",
		Dimensions: []string{"query", "page"},
		Filters:    []string{"page:includingRegex:https://x/y:z", "country:equals:ita"},
		Type:       "googleNews", DataState: "all", RowLimit: 500, StartRow: 10,
	}
	if !reflect.DeepEqual(echo, wantEcho) {
		t.Errorf("echo = %+v\nwant %+v", echo, wantEcho)
	}

	// --all reports the page size actually used and never emits null lists
	_, echo, err = buildAnalyticsRequest(analyticsQueryOpts{RowLimit: 10, All: true}, end)
	if err != nil {
		t.Fatal(err)
	}
	if echo.RowLimit != 25000 || !echo.All || echo.Dimensions == nil || echo.Filters == nil {
		t.Errorf("--all echo = %+v", echo)
	}
}

func TestBuildAnalyticsRequestErrors(t *testing.T) {
	end := analyticsTestEnd(t)
	cases := []struct {
		name    string
		opts    analyticsQueryOpts
		wantErr string
	}{
		{"bad dimension", analyticsQueryOpts{Dimensions: []string{"foo"}, RowLimit: 10}, "unknown dimension"},
		{"bad filter", analyticsQueryOpts{Filters: []string{"query:like:x"}, RowLimit: 10}, "unknown operator"},
		{"bad type", analyticsQueryOpts{Type: "audio", RowLimit: 10}, "--type"},
		{"bad data state", analyticsQueryOpts{DataState: "fresh", RowLimit: 10}, "--data-state"},
		{"bad aggregation", analyticsQueryOpts{AggregationType: "byDay", RowLimit: 10}, "--aggregation-type"},
		{"row limit zero", analyticsQueryOpts{RowLimit: 0}, "--row-limit"},
		{"row limit too big", analyticsQueryOpts{RowLimit: 25001}, "--row-limit"},
		{"negative start row", analyticsQueryOpts{RowLimit: 10, StartRow: -1}, "--start-row"},
		{"hour without hourly_all", analyticsQueryOpts{Dimensions: []string{"hour"}, RowLimit: 10}, "hourly_all"},
		{"hour with final", analyticsQueryOpts{Dimensions: []string{"hour"}, DataState: "final", RowLimit: 10}, "hourly_all"},
	}
	for _, tc := range cases {
		_, _, err := buildAnalyticsRequest(tc.opts, end)
		if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
			t.Errorf("%s: err = %v, want containing %q", tc.name, err, tc.wantErr)
		}
	}

	_, _, err := buildAnalyticsRequest(analyticsQueryOpts{Dimensions: []string{"hour"}, DataState: "hourly_all", RowLimit: 10}, end)
	if err != nil {
		t.Errorf("hour + hourly_all should be accepted: %v", err)
	}
}

func TestAddPageTypes(t *testing.T) {
	c := pagetype.MustDefault()
	rows := []map[string]any{
		{"page": "https://www.1000farmacie.it/aspirina-100-mg.html", "clicks": 1.0},
		{"page": "https://www.1000farmacie.it/integratori.htm", "clicks": 2.0},
		{"page": "https://www.1000farmacie.it/blog/post", "clicks": 3.0},
		{"query": "no page key"},
	}
	addPageTypes(rows, c.Classify)
	want := []any{"product", "category", "blog", nil}
	for i, r := range rows {
		if r["page_type"] != want[i] {
			t.Errorf("row %d page_type = %v, want %v", i, r["page_type"], want[i])
		}
	}
}

func qwRow(q string, pos, ctr, impr, clicks float64) *searchconsole.ApiDataRow {
	return &searchconsole.ApiDataRow{Keys: []string{q}, Position: pos, Ctr: ctr, Impressions: impr, Clicks: clicks}
}

func TestFilterQuickWins(t *testing.T) {
	th := quickWinThresholds{MinPosition: 4, MaxPosition: 10, MaxCTR: 0.02, MinImpressions: 50}
	rows := []*searchconsole.ApiDataRow{
		qwRow("a-small", 5, 0.01, 100, 1),
		qwRow("b-big", 5, 0.01, 900, 9),
		qwRow("c-pos-too-good", 3.9, 0.01, 5000, 50),
		qwRow("d-pos-too-low", 10.1, 0.005, 5000, 5),
		qwRow("e-ctr-high", 6, 0.021, 5000, 105),
		qwRow("f-few-impr", 6, 0.0, 49, 0),
		qwRow("g-lower-bound", 4, 0.02, 50, 1),  // all bounds inclusive
		qwRow("h-upper-bound", 10, 0.0, 300, 0), // all bounds inclusive
		qwRow("i-tie-more-clicks", 7, 0.01, 300, 3),
		qwRow("j-tie-a", 7, 0.01, 300, 0),
		qwRow("k-tie-b", 7, 0.01, 300, 0),
	}
	got := filterQuickWins(rows, th)
	var names []string
	for _, r := range got {
		names = append(names, r.Keys[0])
	}
	// impressions desc, ties by clicks desc then key asc
	want := []string{"b-big", "i-tie-more-clicks", "h-upper-bound", "j-tie-a", "k-tie-b", "a-small", "g-lower-bound"}
	if !reflect.DeepEqual(names, want) {
		t.Errorf("got  %v\nwant %v", names, want)
	}
	if len(filterQuickWins(nil, th)) != 0 {
		t.Error("nil rows should yield no matches")
	}
}

func TestValidateQuickWinThresholds(t *testing.T) {
	ok := quickWinThresholds{MinPosition: 4, MaxPosition: 10, MaxCTR: 0.02, MinImpressions: 50}
	if err := validateQuickWinThresholds(ok); err != nil {
		t.Fatal(err)
	}
	bad := map[string]quickWinThresholds{
		"--min-position":    {MinPosition: 11, MaxPosition: 10, MaxCTR: 0.02},
		"--max-ctr":         {MinPosition: 4, MaxPosition: 10, MaxCTR: 2},
		"--min-impressions": {MinPosition: 4, MaxPosition: 10, MaxCTR: 0.02, MinImpressions: -1},
	}
	for wantFlag, th := range bad {
		if err := validateQuickWinThresholds(th); err == nil || !strings.Contains(err.Error(), wantFlag) {
			t.Errorf("%s: err = %v", wantFlag, err)
		}
	}
}
