package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadMissingFileGivesDefaults(t *testing.T) {
	cfg, err := Load(filepath.Join(t.TempDir(), "nope.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Site != DefaultSite || cfg.Inspect.DailyBudget != 1800 || len(cfg.PageTypes) == 0 {
		t.Fatalf("unexpected defaults: %+v", cfg)
	}
}

func TestLoadMergesOnlyDefinedKeys(t *testing.T) {
	p := filepath.Join(t.TempDir(), "c.toml")
	body := `
site = "https://www.example.com/"
[inspect]
per_type = 5
[datadog]
query = "service:web"
`
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Site != "https://www.example.com/" {
		t.Errorf("site = %q", cfg.Site)
	}
	if cfg.Inspect.PerType != 5 || cfg.Inspect.DailyBudget != 1800 {
		t.Errorf("inspect = %+v", cfg.Inspect)
	}
	if cfg.Datadog.Query != "service:web" || cfg.Datadog.Site != "datadoghq.eu" {
		t.Errorf("datadog = %+v", cfg.Datadog)
	}
	if len(cfg.PageTypes) != len(Defaults().PageTypes) {
		t.Errorf("page types replaced unexpectedly")
	}
}

func TestPageTypesReplaceDefaults(t *testing.T) {
	p := filepath.Join(t.TempDir(), "c.toml")
	body := `
[[page_types]]
name = "post"
path = '^/p/'
`
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.PageTypes) != 1 || cfg.PageTypes[0].Name != "post" {
		t.Fatalf("page types = %+v", cfg.PageTypes)
	}
}

func TestEncodeMasksSecrets(t *testing.T) {
	cfg := Defaults()
	cfg.Datadog.APIKey = "abcdef1234567890"
	b, err := Encode(cfg, false)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "abcdef1234567890") {
		t.Fatal("secret leaked in masked encode")
	}
	b, err = Encode(cfg, true)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "abcdef1234567890") {
		t.Fatal("reveal should include secret")
	}
}

func TestSaveRoundTrip(t *testing.T) {
	p := filepath.Join(t.TempDir(), "sub", "config.toml")
	cfg := Defaults()
	cfg.Inspect.PerType = 7
	if err := Save(p, cfg); err != nil {
		t.Fatal(err)
	}
	got, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if got.Inspect.PerType != 7 || len(got.PageTypes) != len(cfg.PageTypes) {
		t.Fatalf("round trip mismatch: %+v", got.Inspect)
	}
	if got.PageTypes[0].DDQuery != cfg.PageTypes[0].DDQuery {
		t.Fatalf("dd_query not preserved: %q", got.PageTypes[0].DDQuery)
	}
}
