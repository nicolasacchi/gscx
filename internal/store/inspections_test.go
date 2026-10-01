package store

import (
	"context"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

const site = "sc-domain:example.com"

func openTemp(t *testing.T) *Store {
	t.Helper()
	s, err := Open(context.Background(), filepath.Join(t.TempDir(), "gscx.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func at(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return t
}

func insert(t *testing.T, s *Store, rec Inspection) {
	t.Helper()
	if rec.Site == "" {
		rec.Site = site
	}
	if err := s.InsertInspection(context.Background(), rec); err != nil {
		t.Fatal(err)
	}
}

func TestInsertAndRawRoundTrip(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	insert(t, s, Inspection{
		URL: "https://example.com/a.html", PageType: "product",
		InspectedAt: at("2026-10-01T10:30:15Z"),
		Verdict:     "PASS", CoverageState: "Submitted and indexed", IndexingState: "INDEXING_ALLOWED",
		RobotsTxtState: "ALLOWED", PageFetchState: "SUCCESSFUL",
		GoogleCanonical: "https://example.com/a.html", UserCanonical: "https://example.com/a.html",
		LastCrawlTime: "2026-09-29T04:00:00Z", CrawledAs: "MOBILE",
		RawJSON: `{"inspectionResultLink":"https://search.google.com/x"}`,
	})
	got, err := s.LatestPerURL(ctx, site, at("2026-10-01T00:00:00Z"), at("2026-10-02T00:00:00Z"))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("rows = %d", len(got))
	}
	g := got[0]
	want := Inspection{
		ID: g.ID, Site: site, URL: "https://example.com/a.html", PageType: "product",
		InspectedAt: at("2026-10-01T10:30:15Z"), QuotaDay: "2026-10-01",
		Verdict: "PASS", CoverageState: "Submitted and indexed", IndexingState: "INDEXING_ALLOWED",
		RobotsTxtState: "ALLOWED", PageFetchState: "SUCCESSFUL",
		GoogleCanonical: "https://example.com/a.html", UserCanonical: "https://example.com/a.html",
		LastCrawlTime: "2026-09-29T04:00:00Z", CrawledAs: "MOBILE",
	}
	if !reflect.DeepEqual(g, want) {
		t.Errorf("row = %+v\nwant  %+v", g, want)
	}
	raw, err := s.RawJSON(ctx, g.ID)
	if err != nil || raw != `{"inspectionResultLink":"https://search.google.com/x"}` {
		t.Errorf("raw = %q, %v", raw, err)
	}
}

func TestQuotaDayIsLosAngelesDay(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	// 02:00 UTC on Oct 1 is still Sep 30 in Los Angeles (PDT).
	insert(t, s, Inspection{URL: "u1", InspectedAt: at("2026-10-01T02:00:00Z")})
	insert(t, s, Inspection{URL: "u2", InspectedAt: at("2026-10-01T06:59:59Z")})
	insert(t, s, Inspection{URL: "u3", InspectedAt: at("2026-10-01T07:00:00Z")})
	for day, want := range map[string]int{"2026-09-30": 2, "2026-10-01": 1, "2026-10-02": 0} {
		got, err := s.UsedOnDay(ctx, site, day)
		if err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Errorf("UsedOnDay(%s) = %d, want %d", day, got, want)
		}
	}
}

func TestUsedOnDayCountsErrorsAndIsPerSite(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	ts := at("2026-10-01T12:00:00Z")
	insert(t, s, Inspection{URL: "ok", InspectedAt: ts, Verdict: "PASS"})
	insert(t, s, Inspection{URL: "bad", InspectedAt: ts, Error: "403: forbidden"})
	insert(t, s, Inspection{URL: "other-site", Site: "sc-domain:other.com", InspectedAt: ts})
	got, err := s.UsedOnDay(ctx, site, "2026-10-01")
	if err != nil {
		t.Fatal(err)
	}
	if got != 2 {
		t.Errorf("UsedOnDay = %d, want 2 (error rows count, other sites do not)", got)
	}
}

func TestRecentlyInspected(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	insert(t, s, Inspection{URL: "old", InspectedAt: at("2026-09-01T12:00:00Z"), Verdict: "PASS"})
	insert(t, s, Inspection{URL: "new", InspectedAt: at("2026-09-28T12:00:00Z"), Verdict: "PASS"})
	insert(t, s, Inspection{URL: "new", InspectedAt: at("2026-09-29T12:00:00Z"), Verdict: "PASS"})
	insert(t, s, Inspection{URL: "failed", InspectedAt: at("2026-09-29T12:00:00Z"), Error: "boom"})
	insert(t, s, Inspection{URL: "other", Site: "sc-domain:other.com", InspectedAt: at("2026-09-29T12:00:00Z"), Verdict: "PASS"})

	cases := []struct {
		name  string
		since string
		want  map[string]bool
	}{
		{"week", "2026-09-24T00:00:00Z", map[string]bool{"new": true}},
		{"everything", "2026-01-01T00:00:00Z", map[string]bool{"old": true, "new": true}},
		{"boundary is inclusive", "2026-09-28T12:00:00Z", map[string]bool{"new": true}},
		{"future", "2026-10-30T00:00:00Z", map[string]bool{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := s.RecentlyInspected(ctx, site, at(tc.since))
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestLatestPerURL(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	insert(t, s, Inspection{URL: "b", InspectedAt: at("2026-09-10T10:00:00Z"), Verdict: "NEUTRAL", CoverageState: "Crawled - currently not indexed"})
	insert(t, s, Inspection{URL: "b", InspectedAt: at("2026-09-20T10:00:00Z"), Verdict: "PASS", CoverageState: "Submitted and indexed"})
	insert(t, s, Inspection{URL: "b", InspectedAt: at("2026-09-25T10:00:00Z"), Error: "later failure"})
	insert(t, s, Inspection{URL: "a", InspectedAt: at("2026-09-15T10:00:00Z"), Verdict: "PASS"})
	insert(t, s, Inspection{URL: "c", InspectedAt: at("2026-10-05T10:00:00Z"), Verdict: "PASS"}) // outside window
	insert(t, s, Inspection{URL: "d", InspectedAt: at("2026-08-01T10:00:00Z"), Verdict: "PASS"}) // outside window
	// Same instant: the later insert wins.
	insert(t, s, Inspection{URL: "e", InspectedAt: at("2026-09-15T10:00:00Z"), Verdict: "NEUTRAL"})
	insert(t, s, Inspection{URL: "e", InspectedAt: at("2026-09-15T10:00:00Z"), Verdict: "PASS"})
	insert(t, s, Inspection{URL: "x", Site: "sc-domain:other.com", InspectedAt: at("2026-09-15T10:00:00Z"), Verdict: "PASS"})

	cases := []struct {
		name     string
		from, to string
		want     map[string]string // url -> verdict
	}{
		{"september", "2026-09-01T00:00:00Z", "2026-10-01T00:00:00Z",
			map[string]string{"a": "PASS", "b": "PASS", "e": "PASS"}},
		{"window ends before the PASS: older NEUTRAL is latest in-window", "2026-09-01T00:00:00Z", "2026-09-15T00:00:00Z",
			map[string]string{"b": "NEUTRAL"}},
		{"to is exclusive", "2026-09-01T00:00:00Z", "2026-09-20T10:00:00Z",
			map[string]string{"a": "PASS", "b": "NEUTRAL", "e": "PASS"}},
		{"from is inclusive", "2026-09-20T10:00:00Z", "2026-10-01T00:00:00Z",
			map[string]string{"b": "PASS"}},
		{"empty window", "2027-01-01T00:00:00Z", "2027-02-01T00:00:00Z", map[string]string{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rows, err := s.LatestPerURL(ctx, site, at(tc.from), at(tc.to))
			if err != nil {
				t.Fatal(err)
			}
			got := map[string]string{}
			prev := ""
			for _, r := range rows {
				got[r.URL] = r.Verdict
				if r.URL <= prev {
					t.Errorf("rows not ordered by url: %q after %q", r.URL, prev)
				}
				prev = r.URL
				if r.RawJSON != "" {
					t.Errorf("LatestPerURL must not load raw_json")
				}
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestLatestPerURLSubSecondOrdering(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	base := at("2026-09-15T10:00:00Z")
	insert(t, s, Inspection{URL: "u", InspectedAt: base.Add(900 * time.Millisecond), Verdict: "PASS"})
	insert(t, s, Inspection{URL: "u", InspectedAt: base.Add(100 * time.Millisecond), Verdict: "FAIL"})
	rows, err := s.LatestPerURL(ctx, site, base, base.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Verdict != "PASS" {
		t.Errorf("rows = %+v, want latest-by-time PASS", rows)
	}
}
