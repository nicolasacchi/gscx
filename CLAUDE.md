# CLAUDE.md — gscx

Go CLI for Google Search Console (read-only) that reconstructs what the API does
not expose. The Search Console API v1 has four resources (searchanalytics,
sitemaps, sites, urlInspection) and **no endpoint for the Page indexing
report**. gscx estimates index coverage per page type from sampled URL
inspections, tracks pages with impressions over time, audits sitemaps, and
joins Googlebot crawl counts from Datadog access logs, summarizes rich-result
issues from the stored inspections and reads Core Web Vitals from the Chrome UX
Report (CrUX) API.

## Layout

| Path | Purpose |
|------|---------|
| `cmd/gscx/main.go` | entrypoint; maps `*cierrors.APIError` to the fleet exit-code table and prints a JSON error envelope on stderr |
| `internal/commands/` | cobra commands; one file per group, each registers itself on `rootCmd` in `init()` |
| `internal/commands/root.go` | global flags + helpers: `siteURL()`, `newGSC()`, `openStore()`, `classifier()`, `emit()`, `logf()` |
| `internal/config/` | `~/.config/gscx/config.toml`; every key optional (defaults in `Defaults()`) |
| `internal/pagetype/` | ordered regex rules → page type; shared by every command so numbers are comparable per type |
| `internal/gsc/` | Search Console client (`webmasters.readonly` scope) + `Wrap(err)` → `*cierrors.APIError` with hints |
| `internal/sitemap/` | sitemap fetch/parse (gzip by magic bytes), index walk |
| `internal/store/` | SQLite cache (`modernc.org/sqlite`, pure Go): `inspections`, `crawl_daily` |
| `internal/output/` | JSON emit with `--jq` (gjson, no HTML escaping) + agent-mode row cap |
| `internal/gscdate/` | Search Console dates are Pacific time; data lags ~3 days (`DefaultEnd()`); Monday weeks |
| `internal/quota/` | Google quota day (America/Los_Angeles) + per-minute limiter for URL Inspection |
| `internal/sampler/` | stratified, seeded, round-robin sampling under a budget |
| `internal/crux/` | Chrome UX Report client (`QueryRecord`, `QueryHistory`), `ErrNoData` for a 404, metric thresholds/ratings (`metrics.go`); API key in `?key=`, scrubbed from errors and logs |
| `internal/stats/` | Wilson interval, percentiles |
| `internal/datadog/` | logs aggregate client (`Count`, `CountBy`, `CountDaily`, `CountDailyBy`) + pacer for the 2 req/10 s limit; disjoint per-type queries |

## Commands

| Command | Data source | Notes |
|---------|-------------|-------|
| `doctor` | all | config, page types, credentials, Search Console permission, store, Datadog, CrUX key |
| `config path/show/init` | — | `show` masks secrets unless `--reveal` |
| `sites list/get` | API | |
| `analytics query` | Search Analytics | `--filter DIM:OP:EXPR` (regex server-side), `--all`, `--classify`, `--data-state` |
| `analytics quick-wins` | Search Analytics | position 4–10, low CTR |
| `pages trend` | Search Analytics | distinct pages with impressions per week and page type (index proxy) |
| `sitemaps list/get/audit` | API + HTTP | `audit --expand` walks every sitemap: URL counts, overlap, redundant |
| `inspect <url>...` | URL Inspection | stored in SQLite; budget-guarded |
| `inspect batch` | URL Inspection | stratified sample of the sitemap frame; daily budget, per-minute rate, freshness skip |
| `coverage` / `coverage urls` | store | indexed % per type with Wilson CI; `--frame`, `--vs`; drill-down |
| `crawl` / `crawl history` | Datadog | Googlebot requests per page type (verified network), `--by-status`, `--daily --save`, `--with-coverage` |
| `crawl paths` | Datadog | top requested paths of one page type, optional `--status 301` / `4xx` |
| `rich` / `rich urls` | store | rich result types and structured-data issues per page type from the stored `richResultsResult`; no API calls; `rich urls --rich-type X [--issue S] [--type T]` drills down |
| `cwv` | CrUX API | origin p75 + good/NI/poor densities per form factor (default PHONE, DESKTOP); `--urls a,b` / `--sample N [--type T] [--from-sitemaps]` for URL-level records (counts `no_data`); needs `GSCX_CRUX_API_KEY` |
| `cwv history` | CrUX History API | weekly origin p75 per metric (`--weeks` 1-40, one `--form-factor`), rating changes flagged |

## Gotchas

- URL Inspection quota is 2,000/day and 600/min **per property**, shared with humans and other tools. `inspect.daily_budget` (1,800) leaves headroom. Each call takes ~2 s; raise `--concurrency` for big batches.
- The Datadog logs aggregate API allows **2 requests per 10 s**: a full `crawl` (≈12 queries) takes ~50 s regardless of `--concurrency`. Logs are kept ~7 days, hence `crawl --daily --save`.
- `group_by` needs `sort.type=measure` for a real top-N and `missing` to keep lines without the facet (both handled in `internal/datadog`).
- Render sub-requests (`/products/*/dynamic_details`, `related_products`, `track_product_list_view`, `/header`, `/bundles/`) are ~26–29% of Googlebot requests; they have their own page type so they don't inflate `other`.
- The Search Analytics `page` dimension only lists pages with impressions; `pages trend` is a proxy, never a count of indexed pages.
- The Search Console Enhancements reports (rich results) have no API. `rich` mirrors them from the inspections we stored: a sample, one count per URL (issues count URLs, not items), and Google reports rich results only for indexed pages (`indexed` = verdict PASS), so pcts over `n` understate eligibility.
- CrUX: 150 queries/min per Cloud project (the client paces at 450 ms). A 404 NOT_FOUND is `crux.ErrNoData` and is reported as `no_data`, never as a failure; most product URLs have no URL-level data. CLS comes as a string ("0.04"), also the CLS histogram bounds, and history gaps are `null` / `"NaN"`. A rejected key (400 API_KEY_INVALID) or disabled API (403) aborts a `--urls`/`--sample` run instead of being recorded per URL.
- Never print the CrUX key: it travels in the query string, so `crux` strips the query from logs and scrubs `url.Error`s. `config show` masks `crux.api_key`.

## Conventions

- Output is always JSON. Lists are capped via `output.Cap(rows, flagLimit)`; include the returned `*output.Truncation` as `"truncated"` (omitempty) in the result. An explicit `--limit 0` becomes `output.Unlimited` (no cap even under CLAUDECODE). The cap runs before `--jq`; `Emit` warns on stderr when that happened.
- Marshal API structs with `output.MarshalNoEscape` when the bytes are printed or stored: googleapi types escape `&` as `\u0026` inside their own `MarshalJSON`.
- Every Search Console API error goes through `gsc.Wrap`; the CrUX client maps its own errors (`internal/crux`, hints specific to the CrUX API).
- Precedence: flag > env > config file > default.
- Never write to Search Console. The only state gscx writes is its own SQLite cache and config file.
- Tests: table-driven, no network (use `httptest`).

## Build

```
make build      # bin/gscx
make install    # version-stamped from git describe (plain `go install` = "dev")
make test       # go test -race -cover ./...
GOTOOLCHAIN=go1.26.3 golangci-lint run ./...   # lint: v2.12.2 can't analyze the Go 1.27 stdlib
```
