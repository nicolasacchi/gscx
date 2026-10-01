// Package config loads ~/.config/gscx/config.toml. Every value has a built-in
// default, so the file is optional; flags and environment variables override it
// (flag > env > file > default).
package config

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"

	"github.com/BurntSushi/toml"
	"github.com/nicolasacchi/clicore/config"
	"github.com/nicolasacchi/gscx/internal/pagetype"
)

// DefaultSite is the Search Console property used when none is configured.
const DefaultSite = "sc-domain:1000farmacie.it"

// DefaultCruxOrigin is the origin `cwv` queries when none is configured.
const DefaultCruxOrigin = "https://www.1000farmacie.it"

// Config is the on-disk shape of config.toml.
type Config struct {
	// Site is the Search Console property, e.g. "sc-domain:example.com" or
	// "https://www.example.com/".
	Site string `toml:"site"`
	// CredentialsFile is a service-account key JSON. Empty falls back to
	// GOOGLE_APPLICATION_CREDENTIALS, then Application Default Credentials.
	CredentialsFile string `toml:"credentials_file,omitempty"`
	// DB is the SQLite cache path. Empty means $XDG_DATA_HOME/gscx/gscx.db.
	DB string `toml:"db,omitempty"`
	// Sitemaps is the sampling frame for `inspect batch` and the URL set
	// counted by `sitemaps audit --expand`.
	Sitemaps []string `toml:"sitemaps"`

	Inspect   Inspect         `toml:"inspect"`
	Datadog   Datadog         `toml:"datadog"`
	Crux      Crux            `toml:"crux"`
	PageTypes []pagetype.Rule `toml:"page_types"`
}

// Inspect tunes the URL Inspection sampler.
type Inspect struct {
	// DailyBudget caps inspections per Google quota day (the API allows
	// 2,000/day per property; keep headroom for ad-hoc use).
	DailyBudget int `toml:"daily_budget"`
	// PerMinute caps the request rate (API limit: 600/min per property).
	PerMinute int `toml:"per_minute"`
	// PerType is the default sample size per page type for `inspect batch`.
	PerType int `toml:"per_type"`
	// FreshDays: a URL inspected less than this many days ago is not
	// re-inspected by `inspect batch`.
	FreshDays int `toml:"fresh_days"`
}

// Datadog configures the crawl command (Googlebot requests from access logs).
type Datadog struct {
	APIKey string `toml:"api_key,omitempty"`
	AppKey string `toml:"app_key,omitempty"`
	// Site is the Datadog site, e.g. "datadoghq.eu".
	Site string `toml:"site"`
	// Query is the base log query selecting the access-log stream.
	Query string `toml:"query"`
	// BotQuery selects the crawler by user agent.
	BotQuery string `toml:"bot_query"`
	// VerifiedQuery additionally keeps only requests from the crawler's
	// network (user agents are trivially spoofed).
	VerifiedQuery string `toml:"verified_query"`
}

// Crux configures the cwv command (Core Web Vitals from the Chrome UX Report
// API).
type Crux struct {
	// APIKey is a Google Cloud API key with the Chrome UX Report API enabled.
	APIKey string `toml:"api_key,omitempty"`
	// Origin is the origin queried by default, e.g. "https://www.example.com".
	Origin string `toml:"origin"`
}

// Defaults returns the built-in configuration.
func Defaults() Config {
	return Config{
		Site:     DefaultSite,
		Sitemaps: []string{"https://www.1000farmacie.it/sitemaps/sitemap.xml.gz"},
		Inspect: Inspect{
			DailyBudget: 1800,
			PerMinute:   300,
			PerType:     20,
			FreshDays:   7,
		},
		Datadog: Datadog{
			Site:          "datadoghq.eu",
			Query:         "service:web-1000farmacie",
			BotQuery:      "@http.useragent:*Googlebot*",
			VerifiedQuery: `@network.client.geoip.as.name:"Google LLC"`,
		},
		Crux:      Crux{Origin: DefaultCruxOrigin},
		PageTypes: pagetype.Defaults(),
	}
}

// Path returns the config path: $GSCX_CONFIG, else
// $XDG_CONFIG_HOME/gscx/config.toml, else ~/.config/gscx/config.toml.
func Path() string {
	if p := os.Getenv("GSCX_CONFIG"); p != "" {
		return p
	}
	dir, err := os.UserConfigDir()
	if err != nil {
		return filepath.Join(".", "gscx-config.toml")
	}
	return filepath.Join(dir, "gscx", "config.toml")
}

// DefaultDBPath returns $XDG_DATA_HOME/gscx/gscx.db (~/.local/share/gscx/gscx.db).
func DefaultDBPath() string {
	if d := os.Getenv("XDG_DATA_HOME"); d != "" {
		return filepath.Join(d, "gscx", "gscx.db")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "gscx.db"
	}
	return filepath.Join(home, ".local", "share", "gscx", "gscx.db")
}

// Load reads path over the defaults. A missing file yields the defaults.
// Fields absent from the file keep their default values; a page_types array in
// the file replaces the default rules entirely.
func Load(path string) (Config, error) {
	cfg := Defaults()
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return cfg, nil
		}
		return cfg, err
	}
	var fileCfg Config
	md, err := toml.Decode(string(b), &fileCfg)
	if err != nil {
		return cfg, fmt.Errorf("parse %s: %w", path, err)
	}
	merge(&cfg, fileCfg, md)
	return cfg, nil
}

func merge(dst *Config, src Config, md toml.MetaData) {
	set := func(key ...string) bool { return md.IsDefined(key...) }
	if set("site") {
		dst.Site = src.Site
	}
	if set("credentials_file") {
		dst.CredentialsFile = src.CredentialsFile
	}
	if set("db") {
		dst.DB = src.DB
	}
	if set("sitemaps") {
		dst.Sitemaps = src.Sitemaps
	}
	if set("inspect", "daily_budget") {
		dst.Inspect.DailyBudget = src.Inspect.DailyBudget
	}
	if set("inspect", "per_minute") {
		dst.Inspect.PerMinute = src.Inspect.PerMinute
	}
	if set("inspect", "per_type") {
		dst.Inspect.PerType = src.Inspect.PerType
	}
	if set("inspect", "fresh_days") {
		dst.Inspect.FreshDays = src.Inspect.FreshDays
	}
	if set("datadog", "api_key") {
		dst.Datadog.APIKey = src.Datadog.APIKey
	}
	if set("datadog", "app_key") {
		dst.Datadog.AppKey = src.Datadog.AppKey
	}
	if set("datadog", "site") {
		dst.Datadog.Site = src.Datadog.Site
	}
	if set("datadog", "query") {
		dst.Datadog.Query = src.Datadog.Query
	}
	if set("datadog", "bot_query") {
		dst.Datadog.BotQuery = src.Datadog.BotQuery
	}
	if set("datadog", "verified_query") {
		dst.Datadog.VerifiedQuery = src.Datadog.VerifiedQuery
	}
	if set("crux", "api_key") {
		dst.Crux.APIKey = src.Crux.APIKey
	}
	if set("crux", "origin") {
		dst.Crux.Origin = src.Crux.Origin
	}
	if set("page_types") {
		dst.PageTypes = src.PageTypes
	}
}

// Encode renders cfg as TOML (used by `config show` and `config init`).
// Secrets are masked unless reveal is true.
func Encode(cfg Config, reveal bool) ([]byte, error) {
	if !reveal {
		cfg.Datadog.APIKey = config.MaskSecret(cfg.Datadog.APIKey)
		cfg.Datadog.AppKey = config.MaskSecret(cfg.Datadog.AppKey)
		cfg.Crux.APIKey = config.MaskSecret(cfg.Crux.APIKey)
	}
	var buf bytes.Buffer
	if err := toml.NewEncoder(&buf).Encode(cfg); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// Save writes cfg to path atomically with 0600 permissions.
func Save(path string, cfg Config) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return config.SaveTOML(path, cfg)
}
