package store

import (
	"context"
	"fmt"
	"time"

	"github.com/nicolasacchi/gscx/internal/quota"
)

// tsLayout is the fixed-width UTC timestamp layout stored in inspected_at. It
// is valid RFC3339 (millisecond precision) and sorts lexicographically, so
// range queries can compare the stored text directly.
const tsLayout = "2006-01-02T15:04:05.000Z07:00"

func fmtTS(t time.Time) string { return t.UTC().Format(tsLayout) }

// Inspection is one URL Inspection API call (successful or failed).
type Inspection struct {
	ID          int64
	Site        string
	URL         string
	PageType    string
	InspectedAt time.Time
	QuotaDay    string
	// Error is non-empty when the call failed; failed calls still count
	// against the daily quota.
	Error string

	Verdict         string
	CoverageState   string
	IndexingState   string
	RobotsTxtState  string
	PageFetchState  string
	GoogleCanonical string
	UserCanonical   string
	LastCrawlTime   string
	CrawledAs       string
	// RawJSON is the full API result. It is only populated by RawJSON(id).
	RawJSON string
}

// InsertInspection writes one row. InspectedAt defaults to now; QuotaDay is
// always derived from it (America/Los_Angeles calendar day).
func (s *Store) InsertInspection(ctx context.Context, rec Inspection) error {
	if rec.InspectedAt.IsZero() {
		rec.InspectedAt = time.Now()
	}
	_, err := s.db.ExecContext(ctx, `
INSERT INTO inspections (
	site, url, page_type, inspected_at, quota_day, error,
	verdict, coverage_state, indexing_state, robots_txt_state, page_fetch_state,
	google_canonical, user_canonical, last_crawl_time, crawled_as, raw_json
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		rec.Site, rec.URL, rec.PageType, fmtTS(rec.InspectedAt), quota.Day(rec.InspectedAt), rec.Error,
		rec.Verdict, rec.CoverageState, rec.IndexingState, rec.RobotsTxtState, rec.PageFetchState,
		rec.GoogleCanonical, rec.UserCanonical, rec.LastCrawlTime, rec.CrawledAs, rec.RawJSON)
	if err != nil {
		return fmt.Errorf("insert inspection %s: %w", rec.URL, err)
	}
	return nil
}

// UsedOnDay counts every call recorded for site on the given quota day
// (YYYY-MM-DD, America/Los_Angeles), failed calls included.
func (s *Store) UsedOnDay(ctx context.Context, site, day string) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM inspections WHERE site = ? AND quota_day = ?`, site, day).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("count inspections for %s: %w", day, err)
	}
	return n, nil
}

// RecentlyInspected returns the URLs of site with a successful inspection at
// or after since.
func (s *Store) RecentlyInspected(ctx context.Context, site string, since time.Time) (map[string]bool, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT DISTINCT url FROM inspections WHERE site = ? AND error = '' AND inspected_at >= ?`,
		site, fmtTS(since))
	if err != nil {
		return nil, fmt.Errorf("query recent inspections: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := map[string]bool{}
	for rows.Next() {
		var u string
		if err := rows.Scan(&u); err != nil {
			return nil, fmt.Errorf("scan recent inspection: %w", err)
		}
		out[u] = true
	}
	return out, rows.Err()
}

// LatestPerURL returns, for each URL of site, its most recent successful
// inspection with inspected_at in [from, to), ordered by URL. RawJSON is left
// empty (use RawJSON(id)) so large windows stay cheap.
func (s *Store) LatestPerURL(ctx context.Context, site string, from, to time.Time) ([]Inspection, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT id, url, page_type, inspected_at, quota_day, verdict, coverage_state,
       indexing_state, robots_txt_state, page_fetch_state, google_canonical,
       user_canonical, last_crawl_time, crawled_as
FROM (
	SELECT id, url, page_type, inspected_at, quota_day, verdict, coverage_state,
	       indexing_state, robots_txt_state, page_fetch_state, google_canonical,
	       user_canonical, last_crawl_time, crawled_as,
	       ROW_NUMBER() OVER (PARTITION BY url ORDER BY inspected_at DESC, id DESC) AS rn
	FROM inspections
	WHERE site = ? AND error = '' AND inspected_at >= ? AND inspected_at < ?
)
WHERE rn = 1
ORDER BY url`, site, fmtTS(from), fmtTS(to))
	if err != nil {
		return nil, fmt.Errorf("query latest inspections: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []Inspection
	for rows.Next() {
		r := Inspection{Site: site}
		var at string
		if err := rows.Scan(&r.ID, &r.URL, &r.PageType, &at, &r.QuotaDay, &r.Verdict, &r.CoverageState,
			&r.IndexingState, &r.RobotsTxtState, &r.PageFetchState, &r.GoogleCanonical,
			&r.UserCanonical, &r.LastCrawlTime, &r.CrawledAs); err != nil {
			return nil, fmt.Errorf("scan inspection: %w", err)
		}
		t, err := time.Parse(time.RFC3339, at)
		if err != nil {
			return nil, fmt.Errorf("inspection %d: bad inspected_at %q: %w", r.ID, at, err)
		}
		r.InspectedAt = t
		out = append(out, r)
	}
	return out, rows.Err()
}

// RawJSON returns the stored API result of inspection id.
func (s *Store) RawJSON(ctx context.Context, id int64) (string, error) {
	var raw string
	if err := s.db.QueryRowContext(ctx, `SELECT raw_json FROM inspections WHERE id = ?`, id).Scan(&raw); err != nil {
		return "", fmt.Errorf("read raw inspection %d: %w", id, err)
	}
	return raw, nil
}
