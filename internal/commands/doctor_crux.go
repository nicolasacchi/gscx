package commands

import (
	"context"
	"errors"
	"fmt"

	"github.com/nicolasacchi/gscx/internal/crux"
)

// checkCrux verifies the CrUX API key with one origin query. CrUX is
// optional: a missing key is reported but does not fail the doctor.
func checkCrux(ctx context.Context) check {
	if cruxKey() == "" {
		return check{Name: "crux", OK: true, Detail: "not configured: 'gscx cwv' is unavailable",
			Hint: "set GSCX_CRUX_API_KEY (an API key with the Chrome UX Report API enabled)"}
	}
	origin, err := normalizeOrigin(cruxOriginRaw())
	if err != nil {
		return check{Name: "crux", Detail: err.Error(), Hint: "fix [crux] origin in the config file"}
	}
	client, err := cruxClient()
	if err != nil {
		return check{Name: "crux", Detail: err.Error()}
	}
	lcp := crux.Metrics[0]
	rec, err := client.QueryRecord(ctx, crux.Target{Origin: origin, FormFactor: crux.FormFactorPhone}, []string{lcp.API})
	switch {
	case errors.Is(err, crux.ErrNoData):
		return check{Name: "crux", OK: true, Detail: fmt.Sprintf("key accepted; CrUX has no data for %s (PHONE)", origin)}
	case err != nil:
		return check{Name: "crux", Detail: err.Error()}
	}
	m := rec.Metrics[lcp.API]
	if m.P75 == nil {
		return check{Name: "crux", OK: true, Detail: fmt.Sprintf("key accepted; %s (PHONE) has no LCP p75", origin)}
	}
	return check{Name: "crux", OK: true, Detail: fmt.Sprintf("%s (PHONE): LCP p75 %.0f ms (%s)", origin, *m.P75, lcp.Rate(*m.P75))}
}

func init() {
	extraChecks = append(extraChecks, checkCrux)
}
