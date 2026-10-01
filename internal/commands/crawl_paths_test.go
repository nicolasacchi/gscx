package commands

import (
	"strings"
	"testing"

	"github.com/nicolasacchi/gscx/internal/config"
	"github.com/nicolasacchi/gscx/internal/pagetype"
)

func TestCrawlPathsQuery(t *testing.T) {
	old := cfg
	t.Cleanup(func() { cfg = old })
	cfg = config.Defaults()
	rules := pagetype.Defaults()

	q, err := crawlPathsQuery(rules, "blog", "301")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"service:web-1000farmacie", "@http.useragent:*Googlebot*", `(@http.url_details.path:(\/blog OR \/blog\/*))`, "-(@http.url_details.path:*.html)", "@http.status_code:301"} {
		if !strings.Contains(q, want) {
			t.Errorf("blog query missing %q:\n%s", want, q)
		}
	}
	if strings.Contains(q, `-(@http.url_details.path:(\/blog`) {
		t.Errorf("blog query must not exclude itself:\n%s", q)
	}

	q, err = crawlPathsQuery(rules, pagetype.Other, "4xx")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(q, `-(@http.url_details.path:(\/blog OR \/blog\/*))`) || !strings.HasSuffix(q, "@http.status_code:[400 TO 499]") {
		t.Errorf("other query:\n%s", q)
	}

	if _, err := crawlPathsQuery(rules, "nope", ""); err == nil {
		t.Error("unknown type should fail")
	}
	if _, err := crawlPathsQuery(rules, "blog", "30x"); err == nil {
		t.Error("bad status should fail")
	}
}
