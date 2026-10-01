package store

import (
	"context"
	"time"
)

// RichInspection is an Inspection plus the richResultsResult fragment of its
// stored API result.
type RichInspection struct {
	Inspection
	// RichResults is the JSON of the result's richResultsResult object
	// ({"verdict":…,"detectedItems":[…]}), or "" when the result carries none
	// (the API omits it for pages without structured data and for pages Google
	// has not indexed) or the stored raw result is not valid JSON.
	RichResults string
}

// LatestPerURLRich is LatestPerURL plus each row's richResultsResult
// fragment, extracted inside SQLite so the rest of the (large) raw result is
// never read. Rows are ordered by URL.
func (s *Store) LatestPerURLRich(ctx context.Context, site string, from, to time.Time) ([]RichInspection, error) {
	// json_valid guards rows whose raw_json is empty or truncated: json_extract
	// would fail the whole query on them.
	rows, extras, err := s.latestPerURL(ctx, site, from, to,
		`CASE WHEN json_valid(i.raw_json) THEN json_extract(i.raw_json, '$.richResultsResult') END`)
	if err != nil {
		return nil, err
	}
	out := make([]RichInspection, len(rows))
	for i, r := range rows {
		out[i] = RichInspection{Inspection: r, RichResults: extras[i]}
	}
	return out, nil
}
