package commands

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nicolasacchi/clicore/cierrors"
	"github.com/nicolasacchi/gscx/internal/gscdate"
	"github.com/nicolasacchi/gscx/internal/pagetype"
	searchconsole "google.golang.org/api/searchconsole/v1"
)

func trendTestDate(t *testing.T, s string) time.Time {
	t.Helper()
	d, err := gscdate.Parse(s)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func TestTrendWeeks(t *testing.T) {
	defEnd := trendTestDate(t, "2026-09-27") // a Sunday
	cases := []struct {
		name      string
		opts      pagesTrendOpts
		weeksSet  bool
		wantFirst string
		wantLast  string
		wantN     int
		wantNotes int
		wantErr   string
	}{
		{"last 4 weeks", pagesTrendOpts{Weeks: 4}, false, "2026-08-31", "2026-09-27", 4, 0, ""},
		{"midweek default end excludes partial week", pagesTrendOpts{Weeks: 2}, false, "2026-09-07", "2026-09-20", 2, 0, ""},
		{"start/end snap to weeks", pagesTrendOpts{Weeks: 16, Start: "2026-08-12", End: "2026-09-24"}, false, "2026-08-17", "2026-09-20", 5, 0, ""},
		{"start only uses default end", pagesTrendOpts{Weeks: 16, Start: "2026-09-01"}, false, "2026-09-07", "2026-09-27", 3, 0, ""},
		{"end only counts weeks back", pagesTrendOpts{Weeks: 2, End: "2026-09-13"}, true, "2026-08-31", "2026-09-13", 2, 0, ""},
		{"end past default end warns", pagesTrendOpts{Weeks: 1, End: "2026-10-04"}, true, "2026-09-28", "2026-10-04", 1, 1, ""},
		{"weeks and start conflict", pagesTrendOpts{Weeks: 4, Start: "2026-08-01"}, true, "", "", 0, 0, "mutually exclusive"},
		{"no complete week", pagesTrendOpts{Weeks: 4, Start: "2026-09-22", End: "2026-09-27"}, false, "", "", 0, 0, "no complete"},
		{"zero weeks", pagesTrendOpts{Weeks: 0}, false, "", "", 0, 0, "--weeks"},
		{"too many weeks", pagesTrendOpts{Weeks: 71}, false, "", "", 0, 0, "max 70"},
		{"bad start", pagesTrendOpts{Weeks: 4, Start: "nope"}, false, "", "", 0, 0, "--start"},
		{"bad end", pagesTrendOpts{Weeks: 4, End: "nope"}, false, "", "", 0, 0, "--end"},
	}
	// "midweek" case uses a Wednesday default end
	midweek := trendTestDate(t, "2026-09-23")
	for _, tc := range cases {
		end := defEnd
		if strings.HasPrefix(tc.name, "midweek") {
			end = midweek
		}
		weeks, notes, err := trendWeeks(tc.opts, tc.weeksSet, end)
		if tc.wantErr != "" {
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("%s: err = %v, want containing %q", tc.name, err, tc.wantErr)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: %v", tc.name, err)
			continue
		}
		if len(weeks) != tc.wantN || weeks[0].StartString() != tc.wantFirst || weeks[len(weeks)-1].EndString() != tc.wantLast {
			t.Errorf("%s: %d weeks %s..%s, want %d %s..%s", tc.name, len(weeks), weeks[0].StartString(), weeks[len(weeks)-1].EndString(), tc.wantN, tc.wantFirst, tc.wantLast)
		}
		if len(notes) != tc.wantNotes {
			t.Errorf("%s: notes = %v", tc.name, notes)
		}
	}
}

func pageRow(url string, clicks, impressions float64) *searchconsole.ApiDataRow {
	return &searchconsole.ApiDataRow{Keys: []string{url}, Clicks: clicks, Impressions: impressions}
}

func TestAggregateTrendWeek(t *testing.T) {
	w := gscdate.NewWeek(trendTestDate(t, "2026-09-21"))
	rows := []*searchconsole.ApiDataRow{
		pageRow("https://www.1000farmacie.it/a.html", 2, 100),
		pageRow("https://www.1000farmacie.it/b.html", 0, 50),
		pageRow("https://www.1000farmacie.it/c.html", 1, 25.4),
		pageRow("https://www.1000farmacie.it/cat.htm", 5, 500),
		pageRow("https://www.1000farmacie.it/", 10, 1000),
		pageRow("https://www.1000farmacie.it/zero.html", 0, 0), // no impressions: not counted
		{Keys: nil, Impressions: 10},                           // malformed row: skipped
		pageRow("https://www.1000farmacie.it/strange/path", 0, 3),
	}
	got := aggregateTrendWeek(w, rows, pagetype.MustDefault().Classify)
	if got.WeekStart != "2026-09-21" || got.WeekEnd != "2026-09-27" {
		t.Errorf("week bounds = %s..%s", got.WeekStart, got.WeekEnd)
	}
	if got.Pages != 6 || got.Clicks != 18 || got.Impressions != 1678 {
		t.Errorf("totals = %d pages %d clicks %d impressions", got.Pages, got.Clicks, got.Impressions)
	}
	want := map[string]trendTypeStats{
		"product":  {Pages: 3, Clicks: 3, Impressions: 175},
		"category": {Pages: 1, Clicks: 5, Impressions: 500},
		"home":     {Pages: 1, Clicks: 10, Impressions: 1000},
		"other":    {Pages: 1, Clicks: 0, Impressions: 3},
	}
	if !reflect.DeepEqual(got.ByType, want) {
		t.Errorf("by_type = %+v\nwant %+v", got.ByType, want)
	}
	// per-type pages add up to the total
	sum := 0
	for _, s := range got.ByType {
		sum += s.Pages
	}
	if sum != got.Pages {
		t.Errorf("type pages sum %d != %d", sum, got.Pages)
	}

	empty := aggregateTrendWeek(w, nil, pagetype.MustDefault().Classify)
	if empty.Pages != 0 || empty.ByType == nil {
		t.Errorf("empty week = %+v", empty)
	}
}

func TestApplyTrend(t *testing.T) {
	weeks := []trendWeek{
		{WeekStart: "2026-08-31", Pages: 16000},
		{WeekStart: "2026-09-07", Pages: 16160}, // +1.00%
		{WeekStart: "2026-09-14", Pages: 14000}, // -13.37%: alert
		{WeekStart: "2026-09-21", Pages: 15400}, // +10.00%: not > 10
		{WeekStart: "2026-09-28", Pages: 17000}, // +10.39%: alert
	}
	sum := applyTrend(weeks, 10)

	if weeks[0].ChangePct != nil || weeks[0].Alert {
		t.Errorf("first week must have no change: %+v", weeks[0])
	}
	wantPct := []float64{1.00, -13.37, 10.00, 10.39}
	for i, want := range wantPct {
		got := weeks[i+1].ChangePct
		if got == nil || *got != want {
			t.Errorf("week %d change_pct = %v, want %v", i+1, got, want)
		}
	}
	wantAlert := []bool{false, false, true, false, true}
	for i, w := range weeks {
		if w.Alert != wantAlert[i] {
			t.Errorf("week %d alert = %v, want %v", i, w.Alert, wantAlert[i])
		}
	}
	wantSum := trendSummary{Weeks: 5, MinPages: 14000, MaxPages: 17000, LastPages: 17000, Alerts: []string{"2026-09-14", "2026-09-28"}}
	if !reflect.DeepEqual(sum, wantSum) {
		t.Errorf("summary = %+v\nwant %+v", sum, wantSum)
	}
}

func TestApplyTrendEdgeCases(t *testing.T) {
	// no alerts: Alerts is an empty slice, not nil (serializes as [])
	sum := applyTrend([]trendWeek{{Pages: 10}, {Pages: 10}}, 10)
	if sum.Alerts == nil || len(sum.Alerts) != 0 {
		t.Errorf("alerts = %#v", sum.Alerts)
	}

	// previous week with zero pages: change is undefined, no alert
	weeks := []trendWeek{{Pages: 0}, {Pages: 500}}
	applyTrend(weeks, 10)
	if weeks[1].ChangePct != nil || weeks[1].Alert {
		t.Errorf("change after empty week = %+v", weeks[1])
	}

	// alert-pct 0 flags every change but not a flat week
	weeks = []trendWeek{{Pages: 100}, {Pages: 101}, {Pages: 101}}
	applyTrend(weeks, 0)
	if !weeks[1].Alert || weeks[2].Alert {
		t.Errorf("alert-pct 0: %+v", weeks)
	}

	if s := applyTrend(nil, 10); s.Weeks != 0 || s.Alerts == nil {
		t.Errorf("no weeks summary = %+v", s)
	}
}

func TestCollectTrend(t *testing.T) {
	weeks := gscdate.LastCompleteWeeks(8, trendTestDate(t, "2026-09-27"))
	var inFlight, peak atomic.Int32
	fetch := func(ctx context.Context, w gscdate.Week) ([]*searchconsole.ApiDataRow, error) {
		n := inFlight.Add(1)
		for {
			p := peak.Load()
			if n <= p || peak.CompareAndSwap(p, n) {
				break
			}
		}
		time.Sleep(5 * time.Millisecond)
		inFlight.Add(-1)
		// pages = day-of-month of the week start, so each week is distinguishable
		var rows []*searchconsole.ApiDataRow
		for i := 0; i < w.Start.Day(); i++ {
			rows = append(rows, pageRow("https://www.1000farmacie.it/"+w.StartString()+"-"+string(rune('a'+i%26))+string(rune('a'+i/26))+".html", 1, 1))
		}
		return rows, nil
	}
	got, err := collectTrend(context.Background(), weeks, 3, fetch, pagetype.MustDefault().Classify)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(weeks) {
		t.Fatalf("got %d weeks", len(got))
	}
	for i, w := range got {
		if w.WeekStart != weeks[i].StartString() || w.Pages != weeks[i].Start.Day() {
			t.Errorf("week %d = %s pages=%d, want %s pages=%d", i, w.WeekStart, w.Pages, weeks[i].StartString(), weeks[i].Start.Day())
		}
	}
	if p := peak.Load(); p > 3 || p < 2 {
		t.Errorf("peak concurrency = %d, want 2..3", p)
	}
}

func TestCollectTrendError(t *testing.T) {
	weeks := gscdate.LastCompleteWeeks(6, trendTestDate(t, "2026-09-27"))
	apiErr := &cierrors.APIError{Status: 403, Kind: "forbidden", Detail: "no access"}
	var calls atomic.Int32
	fetch := func(ctx context.Context, w gscdate.Week) ([]*searchconsole.ApiDataRow, error) {
		calls.Add(1)
		if w.Start.Equal(weeks[2].Start) {
			return nil, apiErr
		}
		return nil, nil
	}
	_, err := collectTrend(context.Background(), weeks, 1, fetch, pagetype.MustDefault().Classify)
	if err == nil {
		t.Fatal("expected error")
	}
	var got *cierrors.APIError
	if !errors.As(err, &got) || got.Status != 403 {
		t.Errorf("structured error must survive wrapping: %v", err)
	}
	if !strings.Contains(err.Error(), weeks[2].StartString()) {
		t.Errorf("error should name the failing week: %v", err)
	}
	if n := calls.Load(); n > 3 {
		t.Errorf("with concurrency 1 the run should stop at the failing week; %d fetches", n)
	}
}

func TestForEachBoundedIdx(t *testing.T) {
	// every index runs exactly once
	var mu sync.Mutex
	seen := map[int]int{}
	err := forEachBoundedIdx(context.Background(), 20, 4, func(_ context.Context, i int) error {
		mu.Lock()
		seen[i]++
		mu.Unlock()
		return nil
	})
	if err != nil || len(seen) != 20 {
		t.Fatalf("err=%v seen=%d", err, len(seen))
	}
	for i, n := range seen {
		if n != 1 {
			t.Errorf("index %d ran %d times", i, n)
		}
	}

	// limit < 1 is treated as 1; n == 0 is a no-op
	if err := forEachBoundedIdx(context.Background(), 0, 0, nil); err != nil {
		t.Errorf("n=0: %v", err)
	}

	// cancelled parent
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := forEachBoundedIdx(ctx, 5, 2, func(context.Context, int) error { return nil }); !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled parent: %v", err)
	}
}
