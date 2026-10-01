package gsc

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/nicolasacchi/clicore/cierrors"
	searchconsole "google.golang.org/api/searchconsole/v1"
)

func TestParseFilter(t *testing.T) {
	cases := []struct {
		in      string
		wantDim string
		wantOp  string
		wantExp string
	}{
		{"query:contains:aspirina", "query", "contains", "aspirina"},
		{"page:includingRegex:/blog/", "page", "includingRegex", "/blog/"},
		{"page:equals:https://www.1000farmacie.it/x.html", "page", "equals", "https://www.1000farmacie.it/x.html"}, // colons in EXPR
		{"Page:INCLUDINGREGEX:^/a:b$", "page", "includingRegex", "^/a:b$"},                                         // case-insensitive names
		{"searchappearance:equals:AMP_BLUE_LINK", "searchAppearance", "equals", "AMP_BLUE_LINK"},
		{"country:notEquals:ita", "country", "notEquals", "ita"},
		{"query:contains: spaced ", "query", "contains", " spaced "}, // EXPR kept verbatim
	}
	for _, tc := range cases {
		f, err := ParseFilter(tc.in)
		if err != nil {
			t.Errorf("ParseFilter(%q): %v", tc.in, err)
			continue
		}
		if f.Dimension != tc.wantDim || f.Operator != tc.wantOp || f.Expression != tc.wantExp {
			t.Errorf("ParseFilter(%q) = %+v, want %s/%s/%q", tc.in, f, tc.wantDim, tc.wantOp, tc.wantExp)
		}
		if got := FormatFilter(f); !strings.EqualFold(got, tc.wantDim+":"+tc.wantOp+":"+tc.wantExp) {
			t.Errorf("FormatFilter round trip = %q", got)
		}
	}
}

func TestParseFilterErrors(t *testing.T) {
	cases := map[string]string{
		"query":                  "want DIM:OP:EXPR",
		"query:contains":         "want DIM:OP:EXPR",
		"":                       "want DIM:OP:EXPR",
		"date:equals:2026-01-01": "unknown dimension",
		"hour:equals:3":          "unknown dimension",
		"foo:equals:x":           "unknown dimension",
		"query:like:x":           "unknown operator",
		"query:regex:x":          "unknown operator",
		"query:contains:":        "empty expression",
	}
	for in, wantSub := range cases {
		_, err := ParseFilter(in)
		if err == nil {
			t.Errorf("ParseFilter(%q): expected error", in)
			continue
		}
		if !strings.Contains(err.Error(), wantSub) {
			t.Errorf("ParseFilter(%q) error %q lacks %q", in, err, wantSub)
		}
		var api *cierrors.APIError
		if !errors.As(err, &api) || api.ExitCode() != 3 {
			t.Errorf("ParseFilter(%q): want validation error with exit 3, got %v", in, err)
		}
	}
}

func TestFilterGroups(t *testing.T) {
	if FilterGroups(nil) != nil {
		t.Error("no filters should yield no groups")
	}
	fs, err := ParseFilters([]string{"query:contains:a", "page:contains:/b"})
	if err != nil {
		t.Fatal(err)
	}
	g := FilterGroups(fs)
	if len(g) != 1 || len(g[0].Filters) != 2 || g[0].GroupType != "and" {
		t.Errorf("groups = %+v", g)
	}
	if _, err := ParseFilters([]string{"query:contains:a", "bad"}); err == nil {
		t.Error("ParseFilters should surface the bad entry")
	}
}

func TestParseDimensions(t *testing.T) {
	got, err := ParseDimensions([]string{"query,PAGE", "searchappearance", " date "})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"query", "page", "searchAppearance", "date"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
	if _, err := ParseDimensions([]string{"query,foo"}); err == nil {
		t.Error("unknown dimension should fail")
	}
	if _, err := ParseDimensions([]string{"query", "Query"}); err == nil {
		t.Error("duplicate dimension should fail")
	}
	if got, err := ParseDimensions(nil); err != nil || len(got) != 0 {
		t.Errorf("empty = %v, %v", got, err)
	}
}

func baseRequest() *searchconsole.SearchAnalyticsQueryRequest {
	return &searchconsole.SearchAnalyticsQueryRequest{StartDate: "2026-09-01", EndDate: "2026-09-07", Dimensions: []string{"query"}, RowLimit: 100}
}

func TestValidateRequest(t *testing.T) {
	mutate := func(f func(r *searchconsole.SearchAnalyticsQueryRequest)) *searchconsole.SearchAnalyticsQueryRequest {
		r := baseRequest()
		f(r)
		return r
	}
	cases := []struct {
		name    string
		req     *searchconsole.SearchAnalyticsQueryRequest
		wantErr string
	}{
		{"ok", baseRequest(), ""},
		{"hour needs hourly_all", mutate(func(r *searchconsole.SearchAnalyticsQueryRequest) { r.Dimensions = []string{"hour"} }), "requires data state hourly_all"},
		{"hour with final", mutate(func(r *searchconsole.SearchAnalyticsQueryRequest) {
			r.Dimensions = []string{"hour"}
			r.DataState = "final"
		}), "requires data state hourly_all"},
		{"hour with hourly_all ok", mutate(func(r *searchconsole.SearchAnalyticsQueryRequest) {
			r.Dimensions = []string{"hour"}
			r.DataState = "hourly_all"
		}), ""},
		{"bad dates order", mutate(func(r *searchconsole.SearchAnalyticsQueryRequest) { r.StartDate = "2026-09-09" }), "after end date"},
		{"bad date", mutate(func(r *searchconsole.SearchAnalyticsQueryRequest) { r.EndDate = "yesterday" }), "end date"},
		{"row limit too big", mutate(func(r *searchconsole.SearchAnalyticsQueryRequest) { r.RowLimit = 25001 }), "out of range"},
		{"negative start row", mutate(func(r *searchconsole.SearchAnalyticsQueryRequest) { r.StartRow = -1 }), "negative"},
		{"unknown dimension", mutate(func(r *searchconsole.SearchAnalyticsQueryRequest) { r.Dimensions = []string{"foo"} }), "unknown dimension"},
		{"byProperty with page dim", mutate(func(r *searchconsole.SearchAnalyticsQueryRequest) {
			r.Dimensions = []string{"page"}
			r.AggregationType = "byProperty"
		}), "byProperty"},
		{"byProperty with page filter", mutate(func(r *searchconsole.SearchAnalyticsQueryRequest) {
			r.AggregationType = "byProperty"
			f, _ := ParseFilters([]string{"page:contains:/x"})
			r.DimensionFilterGroups = FilterGroups(f)
		}), "byProperty"},
		{"byProperty with query dim ok", mutate(func(r *searchconsole.SearchAnalyticsQueryRequest) { r.AggregationType = "byProperty" }), ""},
	}
	for _, tc := range cases {
		err := ValidateRequest(tc.req)
		switch {
		case tc.wantErr == "" && err != nil:
			t.Errorf("%s: unexpected error %v", tc.name, err)
		case tc.wantErr != "" && err == nil:
			t.Errorf("%s: expected error containing %q", tc.name, tc.wantErr)
		case tc.wantErr != "" && !strings.Contains(err.Error(), tc.wantErr):
			t.Errorf("%s: error %q lacks %q", tc.name, err, tc.wantErr)
		}
	}
}

// fakeSource serves rows [0,total) in pages and records every request.
type fakeSource struct {
	total int
	calls []searchconsole.SearchAnalyticsQueryRequest
	fail  int // 1-based call number that fails (0 = never)
}

func (f *fakeSource) fetch(_ context.Context, req *searchconsole.SearchAnalyticsQueryRequest) (*searchconsole.SearchAnalyticsQueryResponse, error) {
	f.calls = append(f.calls, *req)
	if f.fail == len(f.calls) {
		return nil, errors.New("boom")
	}
	resp := &searchconsole.SearchAnalyticsQueryResponse{ResponseAggregationType: "byPage"}
	for i := int(req.StartRow); i < f.total && i < int(req.StartRow+req.RowLimit); i++ {
		resp.Rows = append(resp.Rows, &searchconsole.ApiDataRow{Keys: []string{"k"}, Clicks: float64(i)})
	}
	return resp, nil
}

func TestPaginateAll(t *testing.T) {
	cases := []struct {
		name      string
		total     int
		wantCalls int
	}{
		{"empty", 0, 1},
		{"one short page", 100, 1},
		{"exactly one full page needs a confirming call", MaxRowLimit, 2},
		{"two pages", MaxRowLimit + 7, 2},
		{"three pages", 2*MaxRowLimit + 1, 3},
	}
	for _, tc := range cases {
		src := &fakeSource{total: tc.total}
		var progress []int
		req := baseRequest()
		rows, agg, err := Paginate(context.Background(), src.fetch, req, true, func(n int) { progress = append(progress, n) })
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if len(rows) != tc.total {
			t.Errorf("%s: %d rows, want %d", tc.name, len(rows), tc.total)
		}
		if len(src.calls) != tc.wantCalls {
			t.Errorf("%s: %d calls, want %d", tc.name, len(src.calls), tc.wantCalls)
		}
		if len(progress) != len(src.calls) || (len(progress) > 0 && progress[len(progress)-1] != tc.total) {
			t.Errorf("%s: progress = %v", tc.name, progress)
		}
		if agg != "byPage" {
			t.Errorf("%s: aggregation = %q", tc.name, agg)
		}
		for i, c := range src.calls {
			if c.RowLimit != MaxRowLimit {
				t.Errorf("%s: call %d RowLimit = %d", tc.name, i, c.RowLimit)
			}
			if c.StartRow != int64(i)*MaxRowLimit {
				t.Errorf("%s: call %d StartRow = %d", tc.name, i, c.StartRow)
			}
		}
		// rows must be contiguous with no duplicates across pages
		for i, r := range rows {
			if int(r.Clicks) != i {
				t.Fatalf("%s: row %d has clicks %v", tc.name, i, r.Clicks)
			}
		}
		if req.RowLimit != 100 || req.StartRow != 0 {
			t.Errorf("%s: caller request mutated: %+v", tc.name, req)
		}
	}
}

func TestPaginateSinglePage(t *testing.T) {
	src := &fakeSource{total: 5000}
	req := baseRequest()
	req.RowLimit = 1000
	req.StartRow = 200
	rows, _, err := Paginate(context.Background(), src.fetch, req, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1000 || len(src.calls) != 1 {
		t.Fatalf("rows=%d calls=%d", len(rows), len(src.calls))
	}
	if src.calls[0].StartRow != 200 || src.calls[0].RowLimit != 1000 {
		t.Errorf("request = %+v", src.calls[0])
	}
}

func TestPaginateAllHonorsStartRow(t *testing.T) {
	src := &fakeSource{total: MaxRowLimit + 10}
	req := baseRequest()
	req.StartRow = 5
	rows, _, err := Paginate(context.Background(), src.fetch, req, true, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != MaxRowLimit+5 {
		t.Errorf("rows = %d, want %d", len(rows), MaxRowLimit+5)
	}
	if len(src.calls) != 2 || src.calls[1].StartRow != 5+MaxRowLimit {
		t.Errorf("calls = %+v", src.calls)
	}
}

func TestPaginateErrors(t *testing.T) {
	src := &fakeSource{total: 2*MaxRowLimit + 1, fail: 2}
	if _, _, err := Paginate(context.Background(), src.fetch, baseRequest(), true, nil); err == nil || err.Error() != "boom" {
		t.Errorf("fetch error not propagated: %v", err)
	}

	bad := baseRequest()
	bad.Dimensions = []string{"hour"}
	src = &fakeSource{total: 1}
	if _, _, err := Paginate(context.Background(), src.fetch, bad, false, nil); err == nil || len(src.calls) != 0 {
		t.Errorf("validation must fail before any call: err=%v calls=%d", err, len(src.calls))
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	src = &fakeSource{total: 1}
	if _, _, err := Paginate(ctx, src.fetch, baseRequest(), false, nil); !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled context: %v", err)
	}
}

func TestRowToMap(t *testing.T) {
	row := &searchconsole.ApiDataRow{
		Keys: []string{"aspirina", "https://x/y.html"}, Clicks: 12, Impressions: 1234,
		Ctr: 0.009724473257698542, Position: 7.123456789,
	}
	m := RowToMap([]string{"query", "page"}, row)
	want := map[string]any{
		"query": "aspirina", "page": "https://x/y.html",
		"clicks": 12.0, "impressions": 1234.0, "ctr": 0.009724, "position": 7.12,
	}
	if !reflect.DeepEqual(m, want) {
		t.Errorf("got %v\nwant %v", m, want)
	}

	// no dimensions: metrics only; fewer keys than dims: missing dims omitted
	m = RowToMap(nil, &searchconsole.ApiDataRow{Clicks: 3})
	if len(m) != 4 || m["clicks"] != 3.0 {
		t.Errorf("metrics-only row = %v", m)
	}
	m = RowToMap([]string{"query", "page"}, &searchconsole.ApiDataRow{Keys: []string{"q"}})
	if _, ok := m["page"]; ok || m["query"] != "q" {
		t.Errorf("short keys row = %v", m)
	}
}
