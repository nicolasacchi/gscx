package gsc

import (
	"context"
	"fmt"
	"math"
	"strings"

	"github.com/nicolasacchi/clicore/cierrors"
	"github.com/nicolasacchi/gscx/internal/gscdate"
	searchconsole "google.golang.org/api/searchconsole/v1"
)

// MaxRowLimit is the largest page the Search Analytics API serves.
const MaxRowLimit = 25000

// Canonical wire spellings accepted by the Search Analytics API.
var (
	// QueryDimensions can be grouped by.
	QueryDimensions = []string{"query", "page", "device", "country", "date", "searchAppearance", "hour"}
	// FilterDimensions can be filtered on (date and hour cannot).
	FilterDimensions = []string{"query", "page", "country", "device", "searchAppearance"}
	// FilterOperators are the dimension-filter operators.
	FilterOperators = []string{"equals", "notEquals", "contains", "notContains", "includingRegex", "excludingRegex"}
	// SearchTypes are the values of the request "type" field.
	SearchTypes = []string{"web", "image", "video", "news", "discover", "googleNews"}
	// DataStates are the values of the request "dataState" field.
	DataStates = []string{"final", "all", "hourly_all"}
	// AggregationTypes are the values of the request "aggregationType" field.
	AggregationTypes = []string{"auto", "byPage", "byProperty", "byNewsShowcasePanel"}
)

// NewValidationError builds a client-side argument error. It carries status
// 400 / kind "validation" so main maps it to the fleet's exit code 3.
func NewValidationError(detail, hint string) *cierrors.APIError {
	return &cierrors.APIError{Status: 400, Kind: "validation", Detail: detail, Hint: hint}
}

// Canonical returns the canonical spelling of v among choices, matching
// case-insensitively.
func Canonical(v string, choices []string) (string, bool) {
	for _, c := range choices {
		if strings.EqualFold(v, c) {
			return c, true
		}
	}
	return "", false
}

// ParseDimensions normalizes a dimension list: each entry may itself be a
// comma-separated list; names are matched case-insensitively and duplicates
// are rejected.
func ParseDimensions(in []string) ([]string, error) {
	var out []string
	seen := map[string]bool{}
	for _, item := range in {
		for _, raw := range strings.Split(item, ",") {
			raw = strings.TrimSpace(raw)
			if raw == "" {
				continue
			}
			d, ok := Canonical(raw, QueryDimensions)
			if !ok {
				return nil, NewValidationError(
					fmt.Sprintf("unknown dimension %q", raw),
					"allowed: "+strings.Join(QueryDimensions, ", "))
			}
			if seen[d] {
				return nil, NewValidationError(fmt.Sprintf("duplicate dimension %q", d), "")
			}
			seen[d] = true
			out = append(out, d)
		}
	}
	return out, nil
}

// ParseFilter parses "DIM:OP:EXPR". It splits on the first two colons only, so
// EXPR may contain colons (URLs, regexes).
func ParseFilter(s string) (*searchconsole.ApiDimensionFilter, error) {
	parts := strings.SplitN(s, ":", 3)
	if len(parts) != 3 {
		return nil, NewValidationError(
			fmt.Sprintf("invalid filter %q: want DIM:OP:EXPR", s),
			"example: page:includingRegex:/blog/ (DIM: "+strings.Join(FilterDimensions, ", ")+")")
	}
	dim, ok := Canonical(strings.TrimSpace(parts[0]), FilterDimensions)
	if !ok {
		return nil, NewValidationError(
			fmt.Sprintf("invalid filter %q: unknown dimension %q", s, parts[0]),
			"allowed: "+strings.Join(FilterDimensions, ", "))
	}
	op, ok := Canonical(strings.TrimSpace(parts[1]), FilterOperators)
	if !ok {
		return nil, NewValidationError(
			fmt.Sprintf("invalid filter %q: unknown operator %q", s, parts[1]),
			"allowed: "+strings.Join(FilterOperators, ", "))
	}
	if parts[2] == "" {
		return nil, NewValidationError(fmt.Sprintf("invalid filter %q: empty expression", s), "")
	}
	return &searchconsole.ApiDimensionFilter{Dimension: dim, Operator: op, Expression: parts[2]}, nil
}

// ParseFilters parses several DIM:OP:EXPR strings.
func ParseFilters(in []string) ([]*searchconsole.ApiDimensionFilter, error) {
	out := make([]*searchconsole.ApiDimensionFilter, 0, len(in))
	for _, s := range in {
		f, err := ParseFilter(s)
		if err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, nil
}

// FormatFilter is the inverse of ParseFilter.
func FormatFilter(f *searchconsole.ApiDimensionFilter) string {
	return f.Dimension + ":" + f.Operator + ":" + f.Expression
}

// FilterGroups ANDs all filters in one group (nil when there are none).
func FilterGroups(filters []*searchconsole.ApiDimensionFilter) []*searchconsole.ApiDimensionFilterGroup {
	if len(filters) == 0 {
		return nil
	}
	return []*searchconsole.ApiDimensionFilterGroup{{GroupType: "and", Filters: filters}}
}

// ValidateRequest rejects requests the API would reject (or silently mangle)
// before spending a call on them.
func ValidateRequest(req *searchconsole.SearchAnalyticsQueryRequest) error {
	start, err := gscdate.Parse(req.StartDate)
	if err != nil {
		return NewValidationError("start date: "+err.Error(), "")
	}
	end, err := gscdate.Parse(req.EndDate)
	if err != nil {
		return NewValidationError("end date: "+err.Error(), "")
	}
	if start.After(end) {
		return NewValidationError(
			fmt.Sprintf("start date %s is after end date %s", req.StartDate, req.EndDate), "")
	}
	if req.RowLimit < 0 || req.RowLimit > MaxRowLimit {
		return NewValidationError(
			fmt.Sprintf("row limit %d out of range", req.RowLimit),
			fmt.Sprintf("must be 1..%d; use --all to paginate", MaxRowLimit))
	}
	if req.StartRow < 0 {
		return NewValidationError(fmt.Sprintf("start row %d is negative", req.StartRow), "")
	}
	byPage := false
	for _, d := range req.Dimensions {
		if _, ok := Canonical(d, QueryDimensions); !ok {
			return NewValidationError(fmt.Sprintf("unknown dimension %q", d), "allowed: "+strings.Join(QueryDimensions, ", "))
		}
		if strings.EqualFold(d, "page") {
			byPage = true
		}
		if strings.EqualFold(d, "hour") && !strings.EqualFold(req.DataState, "hourly_all") {
			return NewValidationError(
				`dimension "hour" requires data state hourly_all`,
				"pass --data-state hourly_all (the API only serves hourly data for the last ~10 days)")
		}
	}
	for _, g := range req.DimensionFilterGroups {
		for _, f := range g.Filters {
			if strings.EqualFold(f.Dimension, "page") {
				byPage = true
			}
		}
	}
	if byPage && strings.EqualFold(req.AggregationType, "byProperty") {
		return NewValidationError(
			"aggregation type byProperty cannot be combined with the page dimension or a page filter",
			"use auto or byPage")
	}
	return nil
}

// PageFetcher fetches one page of a Search Analytics query.
type PageFetcher func(ctx context.Context, req *searchconsole.SearchAnalyticsQueryRequest) (*searchconsole.SearchAnalyticsQueryResponse, error)

// QueryAll runs a Search Analytics query. With all=false it issues one call
// honoring req.RowLimit/StartRow. With all=true it pages through every row
// (RowLimit 25,000, StartRow advancing) until a page comes back short.
// progress, if non-nil, is called after each page with the rows fetched so far.
// It returns the rows and the response aggregation type.
func QueryAll(ctx context.Context, svc *searchconsole.Service, site string, req *searchconsole.SearchAnalyticsQueryRequest, all bool, progress func(int)) ([]*searchconsole.ApiDataRow, string, error) {
	fetch := func(ctx context.Context, r *searchconsole.SearchAnalyticsQueryRequest) (*searchconsole.SearchAnalyticsQueryResponse, error) {
		resp, err := svc.Searchanalytics.Query(site, r).Context(ctx).Do()
		return resp, Wrap(err)
	}
	return Paginate(ctx, fetch, req, all, progress)
}

// Paginate is QueryAll with an injectable page fetcher. The caller's request
// is never mutated.
func Paginate(ctx context.Context, fetch PageFetcher, req *searchconsole.SearchAnalyticsQueryRequest, all bool, progress func(int)) ([]*searchconsole.ApiDataRow, string, error) {
	if err := ValidateRequest(req); err != nil {
		return nil, "", err
	}
	pageSize := req.RowLimit
	if all {
		pageSize = MaxRowLimit
	}
	start := req.StartRow
	var rows []*searchconsole.ApiDataRow
	aggType := ""
	for {
		if err := ctx.Err(); err != nil {
			return nil, "", err
		}
		page := *req
		page.RowLimit = pageSize
		page.StartRow = start
		resp, err := fetch(ctx, &page)
		if err != nil {
			return nil, "", err
		}
		if resp == nil {
			resp = &searchconsole.SearchAnalyticsQueryResponse{}
		}
		if aggType == "" {
			aggType = resp.ResponseAggregationType
		}
		rows = append(rows, resp.Rows...)
		if progress != nil {
			progress(len(rows))
		}
		n := int64(len(resp.Rows))
		if !all || n < pageSize || n == 0 {
			break
		}
		start += n
	}
	return rows, aggType, nil
}

// RowToMap turns an API row into a map keyed by dimension name plus clicks,
// impressions, ctr (fraction) and position. ctr is rounded to 6 decimals and
// position to 2.
func RowToMap(dims []string, row *searchconsole.ApiDataRow) map[string]any {
	m := make(map[string]any, len(dims)+4)
	for i, d := range dims {
		if i < len(row.Keys) {
			m[d] = row.Keys[i]
		}
	}
	m["clicks"] = row.Clicks
	m["impressions"] = row.Impressions
	m["ctr"] = roundPlaces(row.Ctr, 6)
	m["position"] = roundPlaces(row.Position, 2)
	return m
}

func roundPlaces(v float64, places int) float64 {
	p := math.Pow10(places)
	return math.Round(v*p) / p
}
