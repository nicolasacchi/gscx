package commands

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/nicolasacchi/gscx/internal/config"
	"github.com/nicolasacchi/gscx/internal/pagetype"
	"github.com/nicolasacchi/gscx/internal/store"
)

func TestAttachCrawlCoverage(t *testing.T) {
	ctx := context.Background()
	oldDB, oldSite, oldCfg := flagDB, flagSite, cfg
	t.Cleanup(func() { flagDB, flagSite, cfg = oldDB, oldSite, oldCfg })
	flagDB = filepath.Join(t.TempDir(), "gscx.db")
	flagSite = "sc-domain:example.com"
	cfg = config.Defaults()
	flagCrawlCoverageSince = "30d"

	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	st, err := store.Open(ctx, flagDB)
	if err != nil {
		t.Fatal(err)
	}
	recs := []store.Inspection{
		{URL: "https://www.example.com/a.html", Verdict: "PASS"},
		{URL: "https://www.example.com/b.html", Verdict: "PASS"},
		{URL: "https://www.example.com/c.html", Verdict: "NEUTRAL", CoverageState: "Crawled - currently not indexed"},
		{URL: "https://www.example.com/x.htm", Verdict: "PASS"},
	}
	for i, r := range recs {
		r.Site = flagSite
		r.PageType = "ignored-reclassified-on-read"
		r.InspectedAt = now.Add(-time.Duration(i+1) * time.Hour)
		if err := st.InsertInspection(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	res := &crawlResult{ByType: []crawlTypeOut{{Type: "product"}, {Type: "category"}, {Type: "brand"}}}
	if err := attachCrawlCoverage(ctx, res, pagetype.MustDefault(), now); err != nil {
		t.Fatal(err)
	}
	p := res.ByType[0].Coverage
	if p == nil || p.N != 3 || p.Indexed != 2 || p.IndexedPct == nil {
		t.Fatalf("product coverage = %+v", p)
	}
	if c := res.ByType[1].Coverage; c == nil || c.N != 1 || c.Indexed != 1 {
		t.Fatalf("category coverage = %+v", c)
	}
	if res.ByType[2].Coverage != nil {
		t.Fatalf("brand has no samples, got %+v", res.ByType[2].Coverage)
	}
	if len(res.Warnings) != 0 {
		t.Fatalf("unexpected warnings %v", res.Warnings)
	}

	empty := &crawlResult{ByType: []crawlTypeOut{{Type: "product"}}}
	flagSite = "sc-domain:other.com"
	if err := attachCrawlCoverage(ctx, empty, pagetype.MustDefault(), now); err != nil {
		t.Fatal(err)
	}
	if len(empty.Warnings) != 1 || empty.ByType[0].Coverage != nil {
		t.Fatalf("empty store: %+v", empty)
	}
}
