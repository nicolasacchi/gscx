package commands

import (
	"context"
	"fmt"
	"time"

	"github.com/nicolasacchi/gscx/internal/datadog"
)

// checkDatadog verifies the credentials used by `crawl` with one cheap count
// of the last hour of crawler requests. Datadog is optional: missing keys are
// reported but do not fail the doctor.
func checkDatadog(ctx context.Context) check {
	apiKey, appKey, site := ddCredentials(cfg.Datadog)
	if apiKey == "" || appKey == "" {
		return check{Name: "datadog", OK: true, Detail: "not configured: 'gscx crawl' is unavailable", Hint: "set DD_API_KEY and DD_APP_KEY (app key scope logs_read_data)"}
	}
	client, err := newDatadogClient(datadog.Options{APIKey: apiKey, AppKey: appKey, Site: site, UserAgent: userAgent(), Logf: logf})
	if err != nil {
		return check{Name: "datadog", Detail: err.Error()}
	}
	to := time.Now().UTC()
	q := datadog.BaseQuery(cfg.Datadog.Query, cfg.Datadog.BotQuery, cfg.Datadog.VerifiedQuery, false)
	n, _, err := client.Count(ctx, q, to.Add(-time.Hour), to)
	if err != nil {
		return check{Name: "datadog", Detail: err.Error()}
	}
	return check{Name: "datadog", OK: true, Detail: fmt.Sprintf("%s: %d verified crawler requests in the last hour", site, n)}
}

func init() {
	extraChecks = append(extraChecks, checkDatadog)
}
