package datadog

import (
	"testing"

	"github.com/nicolasacchi/gscx/internal/pagetype"
)

func TestOtherQuery(t *testing.T) {
	rules := []pagetype.Rule{
		{Name: "a", Path: "^/a", DDQuery: `@p:\/a*`},
		{Name: "b", Path: "^/b"},
		{Name: "c", Path: "^/c", DDQuery: `@p:\/c*`},
	}
	got := OtherQuery("service:web", rules)
	want := `service:web -(@p:\/a*) -(@p:\/c*)`
	if got != want {
		t.Fatalf("OtherQuery = %q, want %q", got, want)
	}
}

func TestStatusFilter(t *testing.T) {
	cases := map[string]string{
		"301": "@http.status_code:301",
		"3xx": "@http.status_code:[300 TO 399]",
		"5XX": "@http.status_code:[500 TO 599]",
		"404": "@http.status_code:404",
	}
	for in, want := range cases {
		if got, ok := StatusFilter(in); !ok || got != want {
			t.Errorf("StatusFilter(%q) = %q, %v; want %q", in, got, ok, want)
		}
	}
	for _, bad := range []string{"", "30", "3011", "x01", "6xx", "3x1", "abc"} {
		if _, ok := StatusFilter(bad); ok {
			t.Errorf("StatusFilter(%q) should fail", bad)
		}
	}
}
