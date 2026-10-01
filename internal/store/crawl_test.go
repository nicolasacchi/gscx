package store

import (
	"context"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

const (
	crawlTestSource = "datadog:service:web-1000farmacie"
	crawlTestBot    = "@http.useragent:*Googlebot*"
)

var crawlCaptured = time.Date(2026, 10, 1, 8, 0, 0, 0, time.UTC)

func openCrawlStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(context.Background(), filepath.Join(t.TempDir(), "gscx.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func crawlRow(day, typ, status string, n int64) CrawlRow {
	return CrawlRow{Source: crawlTestSource, Day: day, Bot: crawlTestBot, Verified: true, PageType: typ, Status: status, Requests: n, CapturedAt: crawlCaptured}
}

func crawlFilter() CrawlFilter {
	return CrawlFilter{Source: crawlTestSource, Bot: crawlTestBot, Verified: true}
}

func countCrawlRows(t *testing.T, s *Store) int {
	t.Helper()
	var n int
	if err := s.DB().QueryRow(`SELECT COUNT(*) FROM crawl_daily`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestUpsertCrawlDailyIsIdempotent(t *testing.T) {
	ctx := context.Background()
	s := openCrawlStore(t)
	rows := []CrawlRow{
		crawlRow("2026-09-30", "product", "all", 1000),
		crawlRow("2026-09-30", "category", "all", 200),
		crawlRow("2026-09-30", "other", "all", 5),
		crawlRow("2026-09-29", "product", "all", 900),
		crawlRow("2026-09-29", "category", "all", 150),
	}
	first, err := s.UpsertCrawlDaily(ctx, rows)
	if err != nil {
		t.Fatal(err)
	}
	if first.Rows != 5 || !reflect.DeepEqual(first.Days, []string{"2026-09-29", "2026-09-30"}) || len(first.Kept) != 0 {
		t.Fatalf("first = %+v", first)
	}
	before, _ := s.CrawlRows(ctx, crawlFilter())
	second, err := s.UpsertCrawlDaily(ctx, rows)
	if err != nil {
		t.Fatal(err)
	}
	if second.Rows != 5 || len(second.Kept) != 0 {
		t.Fatalf("second = %+v", second)
	}
	after, _ := s.CrawlRows(ctx, crawlFilter())
	if countCrawlRows(t, s) != 5 || !reflect.DeepEqual(before, after) {
		t.Fatalf("re-saving changed the table: %d rows\nbefore %+v\nafter  %+v", countCrawlRows(t, s), before, after)
	}
}

func TestUpsertCrawlDailyReplacesSnapshotAndDropsStaleTypes(t *testing.T) {
	ctx := context.Background()
	s := openCrawlStore(t)
	if _, err := s.UpsertCrawlDaily(ctx, []CrawlRow{
		crawlRow("2026-09-30", "product", "all", 1000),
		crawlRow("2026-09-30", "retired_type", "all", 10),
	}); err != nil {
		t.Fatal(err)
	}
	// Same total (1010) re-saved under the new type set.
	if _, err := s.UpsertCrawlDaily(ctx, []CrawlRow{
		crawlRow("2026-09-30", "product", "all", 1005),
		crawlRow("2026-09-30", "other", "all", 5),
	}); err != nil {
		t.Fatal(err)
	}
	got, _ := s.CrawlRows(ctx, crawlFilter())
	if len(got) != 2 || got[0].PageType != "other" || got[1].PageType != "product" || got[1].Requests != 1005 {
		t.Fatalf("rows = %+v (retired_type must not linger)", got)
	}
}

func TestUpsertCrawlDailyKeepsLargerStoredDay(t *testing.T) {
	ctx := context.Background()
	s := openCrawlStore(t)
	if _, err := s.UpsertCrawlDaily(ctx, []CrawlRow{crawlRow("2026-09-24", "product", "all", 1000), crawlRow("2026-09-24", "other", "all", 50)}); err != nil {
		t.Fatal(err)
	}
	// Re-run after the first hours of that day expired from Datadog.
	res, err := s.UpsertCrawlDaily(ctx, []CrawlRow{crawlRow("2026-09-24", "product", "all", 300), crawlRow("2026-09-24", "other", "all", 10), crawlRow("2026-09-25", "product", "all", 700)})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(res.Kept, []string{"2026-09-24"}) || !reflect.DeepEqual(res.Days, []string{"2026-09-25"}) || res.Rows != 1 {
		t.Fatalf("res = %+v", res)
	}
	got, _ := s.CrawlRows(ctx, crawlFilter())
	if len(got) != 3 || got[1].Requests != 1000 {
		t.Fatalf("complete snapshot was overwritten: %+v", got)
	}
}

func TestUpsertCrawlDailyPerStatusRows(t *testing.T) {
	ctx := context.Background()
	s := openCrawlStore(t)
	withStatus := []CrawlRow{
		crawlRow("2026-09-30", "product", "all", 100),
		crawlRow("2026-09-30", "product", "200", 90),
		crawlRow("2026-09-30", "product", "404", 10),
	}
	if _, err := s.UpsertCrawlDaily(ctx, withStatus); err != nil {
		t.Fatal(err)
	}
	// Re-saving the "all" rows alone (no --by-status) must keep status detail.
	if _, err := s.UpsertCrawlDaily(ctx, []CrawlRow{crawlRow("2026-09-30", "product", "all", 100)}); err != nil {
		t.Fatal(err)
	}
	if n := countCrawlRows(t, s); n != 3 {
		t.Fatalf("rows = %d, want 3 (status rows must survive an all-only re-save)", n)
	}
	// A status-carrying re-save replaces the old status rows.
	if _, err := s.UpsertCrawlDaily(ctx, []CrawlRow{
		crawlRow("2026-09-30", "product", "all", 100),
		crawlRow("2026-09-30", "product", "200", 100),
	}); err != nil {
		t.Fatal(err)
	}
	if n := countCrawlRows(t, s); n != 2 {
		t.Fatalf("rows = %d, want 2 (old 404 row replaced)", n)
	}
	f := crawlFilter()
	f.Status = "200"
	got, _ := s.CrawlRows(ctx, f)
	if len(got) != 1 || got[0].Requests != 100 {
		t.Fatalf("status rows = %+v", got)
	}
}

func TestCrawlRowsFilters(t *testing.T) {
	ctx := context.Background()
	s := openCrawlStore(t)
	other := crawlRow("2026-09-30", "product", "all", 7)
	other.Verified = false
	otherSrc := crawlRow("2026-09-30", "product", "all", 8)
	otherSrc.Source = "datadog:service:other"
	if _, err := s.UpsertCrawlDaily(ctx, []CrawlRow{
		crawlRow("2026-09-20", "product", "all", 1),
		crawlRow("2026-09-30", "product", "all", 2),
		other, otherSrc,
	}); err != nil {
		t.Fatal(err)
	}
	f := crawlFilter()
	f.Since = "2026-09-25"
	got, err := s.CrawlRows(ctx, f)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Requests != 2 || !got[0].Verified || !got[0].CapturedAt.Equal(crawlCaptured) {
		t.Fatalf("rows = %+v", got)
	}
	f.Verified = false
	got, _ = s.CrawlRows(ctx, f)
	if len(got) != 1 || got[0].Requests != 7 {
		t.Fatalf("unverified rows = %+v", got)
	}
}

func TestPivotCrawl(t *testing.T) {
	rows := []CrawlRow{
		crawlRow("2026-09-30", "product", "all", 100),
		crawlRow("2026-09-29", "category", "all", 20),
		crawlRow("2026-09-30", "category", "all", 30),
		crawlRow("2026-09-30", "other", "all", 5),
		crawlRow("2026-09-29", "zzz_retired", "all", 1),
		crawlRow("2026-09-29", "product", "all", 90),
	}
	types, days := PivotCrawl(rows, []string{"home", "category", "product", "other"})
	if want := []string{"category", "product", "other", "zzz_retired"}; !reflect.DeepEqual(types, want) {
		t.Fatalf("types = %v, want %v", types, want)
	}
	if len(days) != 2 || days[0].Day != "2026-09-29" || days[1].Day != "2026-09-30" {
		t.Fatalf("days = %+v", days)
	}
	if d := days[0]; d.Total != 111 || d.ByType["product"] != 90 || d.ByType["other"] != 0 || d.ByType["zzz_retired"] != 1 {
		t.Errorf("day 0 = %+v", d)
	}
	if d := days[1]; d.Total != 135 || len(d.ByType) != 4 || d.ByType["zzz_retired"] != 0 {
		t.Errorf("day 1 = %+v (columns must be rectangular)", d)
	}
}

func TestPivotCrawlEmpty(t *testing.T) {
	types, days := PivotCrawl(nil, []string{"home"})
	if len(types) != 0 || len(days) != 0 {
		t.Errorf("types=%v days=%v", types, days)
	}
}
