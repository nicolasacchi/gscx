package store

import (
	"context"
	"path/filepath"
	"testing"
)

func TestOpenIsIdempotent(t *testing.T) {
	ctx := context.Background()
	p := filepath.Join(t.TempDir(), "nested", "gscx.db")
	s, err := Open(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(ctx, p)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s.Close()
	var v string
	if err := s.DB().QueryRowContext(ctx, `SELECT value FROM meta WHERE key='schema_version'`).Scan(&v); err != nil {
		t.Fatal(err)
	}
	if v != "1" {
		t.Fatalf("schema_version = %q", v)
	}
	for _, table := range []string{"inspections", "crawl_daily"} {
		var n int
		if err := s.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM `+table).Scan(&n); err != nil {
			t.Fatalf("%s: %v", table, err)
		}
	}
}
