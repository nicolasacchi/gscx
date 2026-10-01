# CLAUDE.md — gscx

Go CLI for Google Search Console (read-only) that reconstructs what the API does
not expose. The Search Console API v1 has four resources (searchanalytics,
sitemaps, sites, urlInspection) and **no endpoint for the Page indexing
report**. gscx estimates index coverage per page type from sampled URL
inspections, tracks pages with impressions over time, audits sitemaps, and
joins Googlebot crawl counts from Datadog access logs.

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
| `internal/output/` | JSON emit with `--jq` (gjson) + agent-mode row cap |

## Conventions

- Output is always JSON. Lists are capped via `output.Cap(rows, flagLimit)`; include the returned `*output.Truncation` as `"truncated"` (omitempty) in the result.
- Every Google API error goes through `gsc.Wrap`.
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
