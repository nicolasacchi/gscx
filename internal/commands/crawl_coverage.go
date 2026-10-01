package commands

import (
	"context"
	"time"

	"github.com/nicolasacchi/gscx/internal/pagetype"
)

var (
	flagCrawlWithCoverage  bool
	flagCrawlCoverageSince string
)

// crawlCoverage is the sampled index coverage of one page type, attached to
// the crawl breakdown so crawl volume and indexing can be read side by side.
type crawlCoverage struct {
	N          int      `json:"n"`
	Indexed    int      `json:"indexed"`
	IndexedPct *float64 `json:"indexed_pct,omitempty"`
	CI95       *ci95    `json:"ci95,omitempty"`
}

// attachCrawlCoverage reads the latest inspection per URL from the store
// (no API calls) and attaches per-type coverage to res.ByType. Types with no
// sampled URLs get no coverage field.
func attachCrawlCoverage(ctx context.Context, res *crawlResult, cls *pagetype.Classifier, now time.Time) error {
	since, err := parseSince(flagCrawlCoverageSince, now)
	if err != nil {
		return err
	}
	st, err := openStore(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()
	rows, err := st.LatestPerURL(ctx, siteURL(), since, now.Add(time.Second))
	if err != nil {
		return err
	}
	if len(rows) == 0 {
		res.Warnings = append(res.Warnings, "--with-coverage: no inspections in the store for this window; run 'gscx inspect batch' first")
		return nil
	}
	_, byType := computeCoverage(rows, cls)
	for i := range res.ByType {
		cr, ok := byType[res.ByType[i].Type]
		if !ok || cr.N == 0 {
			continue
		}
		res.ByType[i].Coverage = &crawlCoverage{N: cr.N, Indexed: cr.Indexed, IndexedPct: cr.IndexedPct, CI95: cr.CI95}
	}
	return nil
}

func init() {
	f := crawlCmd.Flags()
	f.BoolVar(&flagCrawlWithCoverage, "with-coverage", false, "attach sampled index coverage per page type from the local store (see 'gscx coverage')")
	f.StringVar(&flagCrawlCoverageSince, "coverage-since", "30d", "inspection window for --with-coverage")
}
