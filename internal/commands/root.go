// Package commands implements the gscx command tree. Each command group lives
// in its own file and registers itself on rootCmd from init().
package commands

import (
	"context"
	"fmt"
	"os"

	"github.com/nicolasacchi/gscx/internal/config"
	"github.com/nicolasacchi/gscx/internal/gsc"
	"github.com/nicolasacchi/gscx/internal/output"
	"github.com/nicolasacchi/gscx/internal/pagetype"
	"github.com/nicolasacchi/gscx/internal/store"
	"github.com/spf13/cobra"
	searchconsole "google.golang.org/api/searchconsole/v1"
)

var (
	version = "dev"

	// Global flags.
	flagSite        string
	flagCredentials string
	flagConfig      string
	flagDB          string
	flagJQ          string
	flagLimit       int
	flagVerbose     bool

	// cfg is the effective configuration, loaded in PersistentPreRunE.
	cfg config.Config
)

var rootCmd = &cobra.Command{
	Use:   "gscx",
	Short: "Google Search Console CLI with index-coverage estimation",
	Long: `gscx reads Google Search Console (read-only) and reconstructs what the API
does not expose: the Page indexing report has no API, so gscx estimates index
coverage per page type from sampled URL inspections, tracks pages with
impressions over time, audits sitemaps, and joins Googlebot crawl counts from
Datadog access logs.

Output is JSON. Use --jq for a gjson path filter. Under Claude Code
(CLAUDECODE=1) row lists are capped at 100 unless --limit is given.

Configuration: ~/.config/gscx/config.toml (see 'gscx config show').
Precedence: flag > environment > config file > built-in default.`,
	SilenceUsage:  true,
	SilenceErrors: true,
	PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
		path := flagConfig
		if path == "" {
			path = config.Path()
		}
		c, err := config.Load(path)
		if err != nil {
			return err
		}
		cfg = c
		return nil
	},
}

// SetVersion stamps the build version.
func SetVersion(v string) {
	version = v
	rootCmd.Version = v
}

// Execute runs the CLI.
func Execute(ctx context.Context) error {
	return rootCmd.ExecuteContext(ctx)
}

func init() {
	pf := rootCmd.PersistentFlags()
	pf.StringVar(&flagSite, "site", "", "Search Console property, e.g. sc-domain:example.com (env GSCX_SITE; default from config)")
	pf.StringVar(&flagCredentials, "credentials", "", "service-account key JSON (env GOOGLE_APPLICATION_CREDENTIALS)")
	pf.StringVar(&flagConfig, "config", "", "config file path (env GSCX_CONFIG; default ~/.config/gscx/config.toml)")
	pf.StringVar(&flagDB, "db", "", "SQLite cache path (env GSCX_DB; default ~/.local/share/gscx/gscx.db)")
	pf.StringVar(&flagJQ, "jq", "", "gjson path filter applied to the JSON output, e.g. 'rows.#.page'")
	pf.IntVar(&flagLimit, "limit", 0, "max rows in list outputs (0 = unbounded; 100 under CLAUDECODE)")
	pf.BoolVar(&flagVerbose, "verbose", false, "log progress and API calls to stderr")
}

// siteURL resolves the Search Console property.
func siteURL() string {
	return firstNonEmpty(flagSite, os.Getenv("GSCX_SITE"), cfg.Site, config.DefaultSite)
}

func credentialsFile() string {
	return firstNonEmpty(flagCredentials, os.Getenv("GOOGLE_APPLICATION_CREDENTIALS"), cfg.CredentialsFile)
}

func dbPath() string {
	return firstNonEmpty(flagDB, os.Getenv("GSCX_DB"), cfg.DB, config.DefaultDBPath())
}

func userAgent() string { return "gscx/" + version }

// newGSC builds the Search Console client.
func newGSC(ctx context.Context) (*searchconsole.Service, error) {
	return gsc.NewService(ctx, credentialsFile(), userAgent())
}

// openStore opens the SQLite cache. Callers must Close it.
func openStore(ctx context.Context) (*store.Store, error) {
	return store.Open(ctx, dbPath())
}

// classifier compiles the configured page-type rules.
func classifier() (*pagetype.Classifier, error) {
	c, err := pagetype.New(cfg.PageTypes)
	if err != nil {
		return nil, fmt.Errorf("config page_types: %w", err)
	}
	return c, nil
}

// emit prints v as JSON honoring --jq.
func emit(cmd *cobra.Command, v any) error {
	return output.Emit(cmd.OutOrStdout(), v, flagJQ)
}

// logf writes progress to stderr when --verbose is set.
func logf(format string, args ...any) {
	if flagVerbose {
		fmt.Fprintf(os.Stderr, format+"\n", args...)
	}
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
