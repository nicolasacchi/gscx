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
	cfg.Datadog.AppKey = "appkey-0987654321"
	cfg.Crux.APIKey = "AIzaSyCruxKey-1234567890"
	secrets := []string{cfg.Datadog.APIKey, cfg.Datadog.AppKey, cfg.Crux.APIKey}
	b, err := Encode(cfg, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range secrets {
		if strings.Contains(string(b), s) {
			t.Fatalf("secret %q leaked in masked encode:\n%s", s, b)
		}
	}
	if !strings.Contains(string(b), "api_key") {
		t.Fatalf("masked encode should still show that the keys are set:\n%s", b)
	}
	b, err = Encode(cfg, true)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range secrets {
		if !strings.Contains(string(b), s) {
			t.Fatalf("reveal should include secret %q", s)
		}
	}
}

func TestCruxDefaultsAndMerge(t *testing.T) {
	if got := Defaults().Crux; got.Origin != "https://www.1000farmacie.it" || got.APIKey != "" {
		t.Fatalf("defaults = %+v", got)
	}
	load := func(body string) Config {
		t.Helper()
		p := filepath.Join(t.TempDir(), "c.toml")
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		cfg, err := Load(p)
		if err != nil {
			t.Fatal(err)
		}
		return cfg
	}
	// Only the keys present in the file override the defaults.
	if got := load("[crux]\napi_key = \"k\"\n").Crux; got.APIKey != "k" || got.Origin != DefaultCruxOrigin {
		t.Errorf("api_key only: %+v", got)
	}
	if got := load("[crux]\norigin = \"https://shop.example.com\"\n").Crux; got.APIKey != "" || got.Origin != "https://shop.example.com" {
		t.Errorf("origin only: %+v", got)
	}
	if got := load("site = \"x\"\n").Crux; got != Defaults().Crux {
		t.Errorf("no [crux] section: %+v", got)
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
