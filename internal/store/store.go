// Package store is gscx's local SQLite cache: URL Inspection results (so the
// daily quota accumulates into a growing sample) and daily Googlebot crawl
// aggregates (Datadog keeps access logs only ~7 days).
//
// Methods for each area live in their own file (inspections.go, crawl.go).
package store

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"

	_ "modernc.org/sqlite" // pure-Go SQLite driver, registered as "sqlite"
)

// SchemaVersion is bumped whenever schema changes need a migration.
const SchemaVersion = 1

const schema = `
CREATE TABLE IF NOT EXISTS meta (
	key   TEXT PRIMARY KEY,
	value TEXT NOT NULL
);

-- One row per URL Inspection API call (successful or failed: failures still
-- consume quota and are counted against the daily budget).
CREATE TABLE IF NOT EXISTS inspections (
	id                INTEGER PRIMARY KEY AUTOINCREMENT,
	site              TEXT NOT NULL,
	url               TEXT NOT NULL,
	page_type         TEXT NOT NULL,
	inspected_at      TEXT NOT NULL, -- RFC3339, UTC
	quota_day         TEXT NOT NULL, -- YYYY-MM-DD in America/Los_Angeles (Google quota day)
	error             TEXT NOT NULL DEFAULT '', -- non-empty when the call failed
	verdict           TEXT NOT NULL DEFAULT '',
	coverage_state    TEXT NOT NULL DEFAULT '',
	indexing_state    TEXT NOT NULL DEFAULT '',
	robots_txt_state  TEXT NOT NULL DEFAULT '',
	page_fetch_state  TEXT NOT NULL DEFAULT '',
	google_canonical  TEXT NOT NULL DEFAULT '',
	user_canonical    TEXT NOT NULL DEFAULT '',
	last_crawl_time   TEXT NOT NULL DEFAULT '', -- RFC3339 as returned by the API
	crawled_as        TEXT NOT NULL DEFAULT '',
	raw_json          TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS inspections_site_url ON inspections (site, url, inspected_at);
CREATE INDEX IF NOT EXISTS inspections_site_day ON inspections (site, quota_day);
CREATE INDEX IF NOT EXISTS inspections_site_at  ON inspections (site, inspected_at);

-- Daily Googlebot request counts per page type, captured from access logs.
CREATE TABLE IF NOT EXISTS crawl_daily (
	source      TEXT NOT NULL,    -- e.g. "datadog:service:web-1000farmacie"
	day         TEXT NOT NULL,    -- YYYY-MM-DD, UTC
	bot         TEXT NOT NULL,    -- bot query, e.g. "@http.useragent:*Googlebot*"
	verified    INTEGER NOT NULL, -- 1 when restricted to the crawler's network
	page_type   TEXT NOT NULL,
	status      TEXT NOT NULL,    -- HTTP status code, or "all"
	requests    INTEGER NOT NULL,
	captured_at TEXT NOT NULL,    -- RFC3339, UTC
	PRIMARY KEY (source, day, bot, verified, page_type, status)
);
`

// Store wraps the SQLite database.
type Store struct {
	db   *sql.DB
	path string
}

// Open opens (creating if needed) the database at path and applies the schema.
func Open(ctx context.Context, path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create store dir: %w", err)
	}
	dsn := "file:" + path + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open store %s: %w", path, err)
	}
	// SQLite allows one writer; a single connection avoids SQLITE_BUSY
	// between goroutines of the same process.
	db.SetMaxOpenConns(1)
	if _, err := db.ExecContext(ctx, schema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("init store schema: %w", err)
	}
	if _, err := db.ExecContext(ctx,
		`INSERT INTO meta(key, value) VALUES('schema_version', ?)
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
		fmt.Sprint(SchemaVersion)); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("init store meta: %w", err)
	}
	return &Store{db: db, path: path}, nil
}

// DB exposes the handle for the per-area method files.
func (s *Store) DB() *sql.DB { return s.db }

// Path is the database file path.
func (s *Store) Path() string { return s.path }

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }
