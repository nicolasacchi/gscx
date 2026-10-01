package datadog

import (
	"reflect"
	"strings"
	"testing"

	"github.com/nicolasacchi/gscx/internal/config"
	"github.com/nicolasacchi/gscx/internal/pagetype"
)

func TestBaseQuery(t *testing.T) {
	d := config.Defaults().Datadog
	got := BaseQuery(d.Query, d.BotQuery, d.VerifiedQuery, false)
	want := `service:web-1000farmacie @http.useragent:*Googlebot* @network.client.geoip.as.name:"Google LLC"`
	if got != want {
		t.Errorf("verified base = %q, want %q", got, want)
	}
	got = BaseQuery(d.Query, d.BotQuery, d.VerifiedQuery, true)
	if want := `service:web-1000farmacie @http.useragent:*Googlebot*`; got != want {
		t.Errorf("unverified base = %q, want %q", got, want)
	}
	if got := BaseQuery("  svc ", "", "", false); got != "svc" {
		t.Errorf("empty parts must be skipped, got %q", got)
	}
}

func TestSpoofedQuery(t *testing.T) {
	d := config.Defaults().Datadog
	got := SpoofedQuery(d.Query, d.BotQuery, d.VerifiedQuery)
	want := `service:web-1000farmacie @http.useragent:*Googlebot* -(@network.client.geoip.as.name:"Google LLC")`
	if got != want {
		t.Errorf("spoofed = %q, want %q", got, want)
	}
	if SpoofedQuery(d.Query, d.BotQuery, "") != "" {
		t.Error("no verified query means nothing to compare against")
	}
}

func TestTypeQueriesMirrorFirstMatch(t *testing.T) {
	rules := []pagetype.Rule{
		{Name: "home", Path: `^/$`, DDQuery: `@http.url_details.path:\/`},
		{Name: "paged", Path: `x`, DDQuery: `@http.url_details.path:*.htm\/*`},
		{Name: "nodd", Path: `y`},
		{Name: "brand", Path: `z`, DDQuery: `@http.url_details.path:\/brands* @http.url:*page=*`},
		{Name: "render", Path: `w`, DDQuery: `@http.url_details.path:(\/a* OR \/header)`},
	}
	qs, uncounted := TypeQueries("BASE", rules)
	if !reflect.DeepEqual(uncounted, []string{"nodd"}) {
		t.Errorf("uncounted = %v", uncounted)
	}
	want := map[string]string{
		"home":   `BASE (@http.url_details.path:\/)`,
		"paged":  `BASE (@http.url_details.path:*.htm\/*) -(@http.url_details.path:\/)`,
		"brand":  `BASE (@http.url_details.path:\/brands* @http.url:*page=*) -(@http.url_details.path:\/) -(@http.url_details.path:*.htm\/*)`,
		"render": `BASE (@http.url_details.path:(\/a* OR \/header)) -(@http.url_details.path:\/) -(@http.url_details.path:*.htm\/*) -(@http.url_details.path:\/brands* @http.url:*page=*)`,
	}
	if len(qs) != len(want) {
		t.Fatalf("got %d queries, want %d: %+v", len(qs), len(want), qs)
	}
	order := []string{"home", "paged", "brand", "render"}
	for i, q := range qs {
		if q.Type != order[i] {
			t.Errorf("query %d type = %q, want %q (rule order)", i, q.Type, order[i])
		}
		if q.Query != want[q.Type] {
			t.Errorf("%s:\n got  %s\n want %s", q.Type, q.Query, want[q.Type])
		}
	}
}

// Every query must contain its own fragment positively and every earlier
// fragment negated — that is what makes the counts disjoint — with fragments
// kept byte-for-byte (backslash escapes and wildcards untouched).
func TestTypeQueriesDisjointOnDefaults(t *testing.T) {
	rules := pagetype.Defaults()
	qs, uncounted := TypeQueries("BASE", rules)
	if len(uncounted) != 0 {
		t.Fatalf("default rules must all carry a dd_query, uncounted = %v", uncounted)
	}
	if len(qs) != len(rules) {
		t.Fatalf("got %d queries for %d rules", len(qs), len(rules))
	}
	for i, q := range qs {
		if !strings.HasPrefix(q.Query, "BASE ("+rules[i].DDQuery+")") {
			t.Errorf("%s: own fragment not first: %s", q.Type, q.Query)
		}
		if got, want := strings.Count(q.Query, " -("), i; got != want {
			t.Errorf("%s: %d negations, want %d (one per earlier rule)", q.Type, got, want)
		}
		for j := 0; j < i; j++ {
			if !strings.Contains(q.Query, " -("+rules[j].DDQuery+")") {
				t.Errorf("%s: earlier rule %s not negated verbatim", q.Type, rules[j].Name)
			}
		}
		for j := i + 1; j < len(rules); j++ {
			if strings.Contains(q.Query, rules[j].DDQuery) && !strings.Contains(rules[i].DDQuery, rules[j].DDQuery) {
				t.Errorf("%s: later rule %s leaked into the query", q.Type, rules[j].Name)
			}
		}
	}
}

func TestTypeQueriesNoRules(t *testing.T) {
	qs, unc := TypeQueries("BASE", nil)
	if len(qs) != 0 || len(unc) != 0 {
		t.Errorf("qs=%v unc=%v", qs, unc)
	}
}
