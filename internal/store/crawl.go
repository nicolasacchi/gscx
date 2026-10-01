package store

import (
	"context"
	"fmt"
	"sort"
	"time"
)

// CrawlStatusAll is the crawl_daily status of a row that counts every HTTP status.
const CrawlStatusAll = "all"

// CrawlRow is one crawl_daily row: Googlebot requests for one page type on one
// UTC day (optionally for one HTTP status).
type CrawlRow struct {
	Source     string    `json:"source"`
	Day        string    `json:"day"` // YYYY-MM-DD, UTC
	Bot        string    `json:"bot"`
	Verified   bool      `json:"verified"`
	PageType   string    `json:"page_type"`
	Status     string    `json:"status"` // CrawlStatusAll or an HTTP status code
	Requests   int64     `json:"requests"`
	CapturedAt time.Time `json:"captured_at"`
}

type crawlDayKey struct {
	source, day, bot string
	verified         bool
}

func (r CrawlRow) dayKey() crawlDayKey { return crawlDayKey{r.Source, r.Day, r.Bot, r.Verified} }

// CrawlUpsert reports what UpsertCrawlDaily did, per day.
type CrawlUpsert struct {
	Rows int      `json:"rows"`
	Days []string `json:"days"`
	// Kept lists days whose stored total was larger than the new one, so the
	// stored snapshot was left untouched (see UpsertCrawlDaily).
	Kept []string `json:"kept,omitempty"`
}

// UpsertCrawlDaily stores rows in one transaction. It is idempotent: saving the
// same rows twice leaves the table unchanged.
//
// A day is stored as a snapshot: the existing "all" rows of that
// (source, day, bot, verified) are replaced, so a page type that was dropped
// from the configuration does not linger and double-count. Per-status rows are
// replaced only when the new snapshot carries per-status rows itself, so
// re-saving without --by-status never erases earlier status detail.
//
// Datadog only keeps ~7 days of logs and a re-run can see a day whose first
// hours have already expired. Day totals do not depend on the page-type rules
// (only on source/bot/verified), so a new snapshot with a smaller total than the
// stored one is by construction the incomplete one: that day is kept as stored
// and reported in CrawlUpsert.Kept.
func (s *Store) UpsertCrawlDaily(ctx context.Context, rows []CrawlRow) (CrawlUpsert, error) {
	var res CrawlUpsert
	groups := map[crawlDayKey][]CrawlRow{}
	var order []crawlDayKey
	for _, r := range rows {
		k := r.dayKey()
		if _, ok := groups[k]; !ok {
			order = append(order, k)
		}
		groups[k] = append(groups[k], r)
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return res, fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	for _, k := range order {
		grp := groups[k]
		var newTotal int64
		hasStatus := false
		for _, r := range grp {
			if r.Status == CrawlStatusAll {
				newTotal += r.Requests
			} else {
				hasStatus = true
			}
		}
		var oldTotal int64
		if err := tx.QueryRowContext(ctx,
			`SELECT COALESCE(SUM(requests), 0) FROM crawl_daily
			 WHERE source = ? AND day = ? AND bot = ? AND verified = ? AND status = ?`,
			k.source, k.day, k.bot, crawlBoolInt(k.verified), CrawlStatusAll).Scan(&oldTotal); err != nil {
			return res, fmt.Errorf("read stored total: %w", err)
		}
		if oldTotal > newTotal {
			res.Kept = append(res.Kept, k.day)
			continue
		}
		del := `DELETE FROM crawl_daily WHERE source = ? AND day = ? AND bot = ? AND verified = ? AND status = ?`
		if _, err := tx.ExecContext(ctx, del, k.source, k.day, k.bot, crawlBoolInt(k.verified), CrawlStatusAll); err != nil {
			return res, fmt.Errorf("replace day %s: %w", k.day, err)
		}
		if hasStatus {
			if _, err := tx.ExecContext(ctx,
				`DELETE FROM crawl_daily WHERE source = ? AND day = ? AND bot = ? AND verified = ? AND status <> ?`,
				k.source, k.day, k.bot, crawlBoolInt(k.verified), CrawlStatusAll); err != nil {
				return res, fmt.Errorf("replace day %s statuses: %w", k.day, err)
			}
		}
		for _, r := range grp {
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO crawl_daily(source, day, bot, verified, page_type, status, requests, captured_at)
				 VALUES(?, ?, ?, ?, ?, ?, ?, ?)
				 ON CONFLICT(source, day, bot, verified, page_type, status)
				 DO UPDATE SET requests = excluded.requests, captured_at = excluded.captured_at`,
				r.Source, r.Day, r.Bot, crawlBoolInt(r.Verified), r.PageType, r.Status, r.Requests,
				r.CapturedAt.UTC().Format(time.RFC3339)); err != nil {
				return res, fmt.Errorf("upsert crawl row: %w", err)
			}
			res.Rows++
		}
		res.Days = append(res.Days, k.day)
	}
	if err := tx.Commit(); err != nil {
		return res, fmt.Errorf("commit: %w", err)
	}
	sort.Strings(res.Days)
	sort.Strings(res.Kept)
	return res, nil
}

// CrawlFilter selects crawl_daily rows.
type CrawlFilter struct {
	Source   string
	Bot      string
	Verified bool
	// Since is the first day to include (YYYY-MM-DD); empty means all.
	Since string
	// Status defaults to CrawlStatusAll.
	Status string
}

// CrawlRows returns the rows matching f ordered by day then page type.
func (s *Store) CrawlRows(ctx context.Context, f CrawlFilter) ([]CrawlRow, error) {
	status := f.Status
	if status == "" {
		status = CrawlStatusAll
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT source, day, bot, verified, page_type, status, requests, captured_at
		 FROM crawl_daily
		 WHERE source = ? AND bot = ? AND verified = ? AND status = ? AND day >= ?
		 ORDER BY day, page_type`,
		f.Source, f.Bot, crawlBoolInt(f.Verified), status, f.Since)
	if err != nil {
		return nil, fmt.Errorf("query crawl_daily: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []CrawlRow
	for rows.Next() {
		var r CrawlRow
		var verified int
		var captured string
		if err := rows.Scan(&r.Source, &r.Day, &r.Bot, &verified, &r.PageType, &r.Status, &r.Requests, &captured); err != nil {
			return nil, fmt.Errorf("scan crawl_daily: %w", err)
		}
		r.Verified = verified != 0
		r.CapturedAt, _ = time.Parse(time.RFC3339, captured)
		out = append(out, r)
	}
	return out, rows.Err()
}

// CrawlDay is one pivoted day: the per-type counts and their sum.
type CrawlDay struct {
	Day    string           `json:"day"`
	Total  int64            `json:"total"`
	ByType map[string]int64 `json:"by_type"`
}

// PivotCrawl turns day × type rows into one CrawlDay per day (ascending) with
// every column present (0 when a type has no row that day), and returns the
// column order: the types of order that occur in rows, then any other stored
// type alphabetically.
func PivotCrawl(rows []CrawlRow, order []string) (types []string, days []CrawlDay) {
	present := map[string]bool{}
	byDay := map[string]map[string]int64{}
	for _, r := range rows {
		present[r.PageType] = true
		m := byDay[r.Day]
		if m == nil {
			m = map[string]int64{}
			byDay[r.Day] = m
		}
		m[r.PageType] += r.Requests
	}
	seen := map[string]bool{}
	for _, t := range order {
		if present[t] && !seen[t] {
			types = append(types, t)
			seen[t] = true
		}
	}
	var extra []string
	for t := range present {
		if !seen[t] {
			extra = append(extra, t)
		}
	}
	sort.Strings(extra)
	types = append(types, extra...)

	dayNames := make([]string, 0, len(byDay))
	for d := range byDay {
		dayNames = append(dayNames, d)
	}
	sort.Strings(dayNames)
	for _, d := range dayNames {
		cd := CrawlDay{Day: d, ByType: make(map[string]int64, len(types))}
		for _, t := range types {
			n := byDay[d][t]
			cd.ByType[t] = n
			cd.Total += n
		}
		days = append(days, cd)
	}
	return types, days
}

func crawlBoolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
