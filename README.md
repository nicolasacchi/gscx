# gscx

Google Search Console CLI that reconstructs what the API does not expose.

The Search Console API v1 has four resources: Search Analytics, Sitemaps, Sites
and URL Inspection. There is **no endpoint for the Page indexing report**
(indexed / not indexed, reasons, trend). gscx rebuilds most of that picture from
what is available:

- **Index coverage by page type**: a stratified, seeded sample of sitemap URLs
  is inspected every day within the URL Inspection quota. Results accumulate in
  a local SQLite cache, and `coverage` estimates the indexed share per page type
  with a 95% Wilson interval.
- **Pages with impressions over time**: the weekly count of distinct pages with
  at least one impression, per page type. It is a proxy for indexing, not a
  count.
- **Sitemap audit**: reachability, staleness, errors, and with `--expand` URL
  counts, page types and overlap between sitemaps.
- **Googlebot crawl**: requests per page type and status from Datadog access
  logs, restricted to Google's network, with daily history. With
  `--with-coverage` it shows next to each type how much of it is indexed.
- **Rich results**: the Enhancements reports (Product snippets, Merchant
  listings, Breadcrumbs, Review snippets) have no API either. `rich` reads the
  `richResultsResult` of the stored inspections: per page type, the share of URLs
  carrying each rich result type and which structured-data issues Google flagged,
  with no extra API calls.
- **Core Web Vitals**: `cwv` reads real-user LCP, INP and CLS (plus FCP and TTFB)
  from the Chrome UX Report API, for the origin, for given URLs or for a sample
  of URLs per page type, and `cwv history` shows the weekly trend.

gscx is read-only towards Search Console (`webmasters.readonly` scope). The only
state it writes is its own config file and SQLite cache.

## Install

```sh
make install   # → $GOBIN/gscx, version stamped from git describe
gscx doctor    # checks config, credentials, property access, store, Datadog, CrUX
```

Credentials:

| What | How |
|------|-----|
| Search Console | service-account key via `--credentials`, `GOOGLE_APPLICATION_CREDENTIALS` or `credentials_file`; the account must be a user of the property |
| Datadog (only `crawl`) | `DD_API_KEY`, `DD_APP_KEY`, `DD_SITE` (app key scope `logs_read_data`) |
| CrUX (only `cwv`) | `--crux-key`, `GSCX_CRUX_API_KEY` or `[crux] api_key`: a Google Cloud API key with the Chrome UX Report API enabled (150 queries/min per project, free) |

Configuration lives in `~/.config/gscx/config.toml` and is optional:
`gscx config init` writes the defaults, `gscx config show` prints the effective
values. Precedence is flag > environment > config file > default.

## Examples

```sh
# Daily: inspect 20 new URLs per page type, then read the estimate
gscx inspect batch --per-type 20
gscx coverage --frame

# What is behind a number
gscx coverage urls --type product --state "Crawled - currently not indexed"

# Pages with impressions, last 16 weeks, by type
gscx pages trend --weeks 16 --jq summary

# Server-side regex filter, all rows
gscx analytics query --dimensions page --filter 'page:includingRegex:^https://www\.example\.com/blog/' --all

# Ranking 4-10 with low CTR
gscx analytics quick-wins --limit 20

# Sitemaps: what is broken, stale or redundant
gscx sitemaps audit --expand

# Googlebot over the last 7 days, by page type and status, with coverage
gscx crawl --by-status --with-coverage

# Which blog URLs answer 301 to Googlebot
gscx crawl paths --type blog --status 301

# Keep history beyond Datadog's ~7-day retention
gscx crawl --from 3d --daily --save
gscx crawl history --since 30d

# Rich results from the stored inspections: coverage and issues per page type
gscx rich
gscx rich urls --rich-type "Merchant listings" --issue availability --type product

# Core Web Vitals (CrUX): origin p75 for phone and desktop, then the weekly trend
gscx cwv
gscx cwv history --weeks 25 --form-factor PHONE

# URL-level CrUX: given URLs, or 10 per page type (most product URLs have no data)
gscx cwv --urls https://www.example.com/,https://www.example.com/brands/acme
gscx cwv --sample 10 --type product --type category
```

Output is JSON. `--jq` applies a [gjson](https://github.com/tidwall/gjson)
path. Under Claude Code (`CLAUDECODE=1`) lists are capped at 100 rows unless
`--limit` is given; `--limit 0` returns every row. A capped list carries a
`truncated` object, and since the cap is applied before `--jq`, a warning is
printed on stderr when a filter ran over a capped list.

## Page types

Every command classifies URLs with the same ordered rules (first match wins), so
numbers are comparable across commands. Each rule has a path regex, an optional
query-string regex and the equivalent Datadog search fragment. The defaults are
for 1000farmacie.it; replace them with `[[page_types]]` in the config.

## Limits

- URL Inspection: 2,000 calls/day and 600/min per property (`inspect.daily_budget` defaults to 1,800). The quota day is Pacific time.
- Search Analytics data lags ~2–3 days; default end dates are Pacific today minus 3.
- Datadog logs aggregate: 2 requests per 10 seconds; logs kept ~7 days.
- Coverage estimates the URLs gscx knows about (the sitemaps), not every URL Google has discovered.
- `rich` is a sample (the stored inspections) and Google reports rich results only for indexed pages, so `n` counts every sampled URL but only the `indexed` ones can carry any.
- CrUX: 150 queries/min per Cloud project (gscx paces itself). URL-level data exists only for URLs with enough traffic, so most product URLs are `no_data`; a 404 is reported as `no_data`, not as an error.

## Development

```sh
make test                                       # go test -race -cover ./...
GOTOOLCHAIN=go1.26.3 golangci-lint run ./...    # see CLAUDE.md
```
