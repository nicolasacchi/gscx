package commands

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"github.com/nicolasacchi/gscx/internal/gsc"
	"github.com/spf13/cobra"
)

// check is one doctor result.
type check struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail,omitempty"`
	Hint   string `json:"hint,omitempty"`
}

// extraChecks lets other command files contribute doctor checks (e.g. the
// Datadog credentials used by `crawl`) without editing this file.
var extraChecks []func(ctx context.Context) check

var doctorCmd = &cobra.Command{
	Use:   "doctor",
	Short: "Check configuration, credentials, Search Console access and the local cache",
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx := cmd.Context()
		checks := []check{
			checkConfig(),
			checkPageTypes(),
			checkCredentials(),
			checkSearchConsole(ctx),
			checkStore(ctx),
		}
		for _, f := range extraChecks {
			checks = append(checks, f(ctx))
		}
		failed := 0
		for _, c := range checks {
			if !c.OK {
				failed++
			}
		}
		if err := emit(cmd, map[string]any{"ok": failed == 0, "site": siteURL(), "checks": checks}); err != nil {
			return err
		}
		if failed > 0 {
			return fmt.Errorf("doctor: %d of %d checks failed", failed, len(checks))
		}
		return nil
	},
}

func checkConfig() check {
	path := configPath()
	if _, err := os.Stat(path); err != nil {
		return check{Name: "config", OK: true, Detail: path + " not found; using built-in defaults", Hint: "gscx config init writes an editable copy"}
	}
	return check{Name: "config", OK: true, Detail: path}
}

func checkPageTypes() check {
	c, err := classifier()
	if err != nil {
		return check{Name: "page_types", Detail: err.Error(), Hint: "fix [[page_types]] in the config file"}
	}
	return check{Name: "page_types", OK: true, Detail: fmt.Sprintf("%d rules: %v", len(c.Rules()), c.Names())}
}

func checkCredentials() check {
	path := credentialsFile()
	if path == "" {
		return check{Name: "credentials", OK: true, Detail: "no key file configured; using Application Default Credentials"}
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return check{Name: "credentials", Detail: err.Error(), Hint: "set GOOGLE_APPLICATION_CREDENTIALS or --credentials to a readable service-account key"}
	}
	var key struct {
		Type        string `json:"type"`
		ClientEmail string `json:"client_email"`
	}
	if err := json.Unmarshal(b, &key); err != nil {
		return check{Name: "credentials", Detail: "not valid JSON: " + err.Error()}
	}
	if key.Type != "service_account" {
		return check{Name: "credentials", Detail: fmt.Sprintf("key type %q, want service_account", key.Type)}
	}
	return check{Name: "credentials", OK: true, Detail: key.ClientEmail}
}

func checkSearchConsole(ctx context.Context) check {
	svc, err := newGSC(ctx)
	if err != nil {
		return check{Name: "search_console", Detail: err.Error()}
	}
	site := siteURL()
	s, err := svc.Sites.Get(site).Context(ctx).Do()
	if err != nil {
		return check{
			Name:   "search_console",
			Detail: gsc.Wrap(err).Error(),
			Hint:   "the service account must be added as a user of " + site + " in Search Console",
		}
	}
	return check{Name: "search_console", OK: true, Detail: fmt.Sprintf("%s: %s", s.SiteUrl, s.PermissionLevel)}
}

func checkStore(ctx context.Context) check {
	st, err := openStore(ctx)
	if err != nil {
		return check{Name: "store", Detail: err.Error(), Hint: "check --db / GSCX_DB"}
	}
	defer func() { _ = st.Close() }()
	var inspections, crawlRows int
	if err := st.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM inspections WHERE site = ?`, siteURL()).Scan(&inspections); err != nil {
		return check{Name: "store", Detail: err.Error()}
	}
	if err := st.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM crawl_daily`).Scan(&crawlRows); err != nil {
		return check{Name: "store", Detail: err.Error()}
	}
	return check{Name: "store", OK: true, Detail: fmt.Sprintf("%s (inspections for site: %d, crawl_daily rows: %d)", st.Path(), inspections, crawlRows)}
}

func init() {
	rootCmd.AddCommand(doctorCmd)
}
