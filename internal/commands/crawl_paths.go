package commands

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/nicolasacchi/gscx/internal/datadog"
	"github.com/nicolasacchi/gscx/internal/gsc"
	"github.com/nicolasacchi/gscx/internal/output"
	"github.com/nicolasacchi/gscx/internal/pagetype"
	"github.com/spf13/cobra"
)

var (
	flagPathsType   string
	flagPathsStatus string
	flagPathsTop    int
	flagPathsFrom   string
	flagPathsTo     string
)

var crawlPathsCmd = &cobra.Command{
	Use:   "paths",
	Short: "Most requested paths of one page type, optionally for one HTTP status",
	Long: `Lists the paths Googlebot requested most for one page type, e.g. to see
which blog URLs answer 301 or which products answer 404. One Datadog query
(group by @http.url_details.path, sorted by count). The query string is not
part of the path, so URLs differing only in ?iss=… are merged.`,
	Example: `  gscx crawl paths --type blog --status 301
  gscx crawl paths --type product --status 404 --top 50
  gscx crawl paths --type other --status 4xx --from 3d`,
	Args: cobra.NoArgs,
	RunE: runCrawlPaths,
}

type crawlPathRow struct {
	Path     string  `json:"path"`
	Requests int64   `json:"requests"`
	Share    float64 `json:"share"` // of the requests matching type+status
}

type crawlPathsResult struct {
	Type      string             `json:"type"`
	Status    string             `json:"status,omitempty"`
	Window    crawlWindowOut     `json:"window"`
	Query     string             `json:"query"`
	Total     int64              `json:"total"`
	TopShare  float64            `json:"top_share"` // share of Total covered by the listed paths
	Paths     []crawlPathRow     `json:"paths"`
	Partial   bool               `json:"partial,omitempty"`
	Warnings  []string           `json:"warnings"`
	Truncated *output.Truncation `json:"truncated,omitempty"`
}

func runCrawlPaths(cmd *cobra.Command, _ []string) error {
	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	now := time.Now().UTC().Truncate(time.Second)
	from, to, err := crawlWindow(flagPathsFrom, flagPathsTo, now)
	if err != nil {
		return err
	}
	if flagPathsTop < 1 || flagPathsTop > 1000 {
		return gsc.NewValidationError("--top must be 1..1000", "")
	}
	cl, err := classifier()
	if err != nil {
		return err
	}
	q, err := crawlPathsQuery(cl.Rules(), flagPathsType, flagPathsStatus)
	if err != nil {
		return err
	}

	apiKey, appKey, site := ddCredentials(cfg.Datadog)
	client, err := newDatadogClient(datadog.Options{APIKey: apiKey, AppKey: appKey, Site: site, UserAgent: userAgent(), Logf: logf})
	if err != nil {
		return err
	}
	total, meta, err := client.Count(ctx, q, from, to)
	if err != nil {
		return err
	}
	buckets, meta2, err := client.CountBy(ctx, q, from, to, "@http.url_details.path", flagPathsTop)
	if err != nil {
		return err
	}

	res := crawlPathsResult{
		Type:     flagPathsType,
		Status:   strings.ToLower(flagPathsStatus),
		Window:   crawlWindowOut{From: from.Format(time.RFC3339), To: to.Format(time.RFC3339)},
		Query:    q,
		Total:    total,
		Partial:  meta.Partial() || meta2.Partial(),
		Warnings: []string{},
	}
	var listed int64
	for _, b := range buckets {
		res.Paths = append(res.Paths, crawlPathRow{Path: b.Value, Requests: b.Count, Share: crawlShare(b.Count, total)})
		listed += b.Count
	}
	res.TopShare = crawlShare(listed, total)
	if res.Partial {
		res.Warnings = append(res.Warnings, "Datadog returned partial results; re-run for exact counts")
	}
	res.Paths, res.Truncated = output.Cap(res.Paths, flagLimit)
	return emit(cmd, res)
}

// crawlPathsQuery builds the Datadog query for one page type (disjoint, as in
// `crawl`) and an optional status filter.
func crawlPathsQuery(rules []pagetype.Rule, typ, status string) (string, error) {
	base := datadog.BaseQuery(cfg.Datadog.Query, cfg.Datadog.BotQuery, cfg.Datadog.VerifiedQuery, flagCrawlUnverified)
	var q string
	if typ == pagetype.Other {
		q = datadog.OtherQuery(base, rules)
	} else {
		qs, _ := datadog.TypeQueries(base, rules)
		for _, tq := range qs {
			if tq.Type == typ {
				q = tq.Query
			}
		}
		if q == "" {
			names := make([]string, 0, len(rules)+1)
			for _, r := range rules {
				if strings.TrimSpace(r.DDQuery) != "" {
					names = append(names, r.Name)
				}
			}
			names = append(names, pagetype.Other)
			return "", gsc.NewValidationError(fmt.Sprintf("unknown or uncounted page type %q", typ), "one of: "+strings.Join(names, ", "))
		}
	}
	if status != "" {
		f, ok := datadog.StatusFilter(status)
		if !ok {
			return "", gsc.NewValidationError(fmt.Sprintf("invalid --status %q", status), "use a code (301) or a class (4xx)")
		}
		q += " " + f
	}
	return q, nil
}

func init() {
	f := crawlPathsCmd.Flags()
	f.StringVar(&flagPathsType, "type", "", "page type (see 'gscx doctor' for the list), or 'other'")
	f.StringVar(&flagPathsStatus, "status", "", "HTTP status code (301) or class (4xx)")
	f.IntVar(&flagPathsTop, "top", 20, "number of paths to list (1-1000)")
	f.StringVar(&flagPathsFrom, "from", "7d", "window start: 24h, 7d, RFC3339 or YYYY-MM-DD")
	f.StringVar(&flagPathsTo, "to", "", "window end (default: now)")
	_ = crawlPathsCmd.MarkFlagRequired("type")
	crawlCmd.AddCommand(crawlPathsCmd)
}
