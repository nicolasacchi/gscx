package pagetype

import "testing"

func TestClassifyDefaults(t *testing.T) {
	c := MustDefault()
	cases := map[string]string{
		"https://www.1000farmacie.it/":                                 "home",
		"https://www.1000farmacie.it":                                  "home",
		"https://www.1000farmacie.it/geanove-60cpr.html":               "product",
		"https://www.1000farmacie.it/geanove-60cpr.html?iss=abc":       "product",
		"/sub-eau-thermale.htm":                                        "category",
		"/sub-deodorante-cruelty-free.htm/179":                         "category_paged",
		"/sub-deodorante-cruelty-free.htm/3/":                          "category_paged",
		"https://www.1000farmacie.it/brands/alpiflor":                  "brand",
		"https://www.1000farmacie.it/brands/phytoitalia?page=13":       "brand_paged",
		"https://www.1000farmacie.it/brands/phytoitalia?sort=x&page=2": "brand_paged",
		"https://www.1000farmacie.it/brands":                           "brand",
		"/blog/guida-prodotti-cbd-come-scegliere-formato-giusto/":      "blog",
		"/rimedi/mal-di-gola":                                          "rimedi",
		"/sitemaps/sitemap.xml.gz":                                     "sitemap",
		"/products/track_product_list_view.turbo_stream":               "render_subrequest",
		"/products/geanove-60cpr/dynamic_details":                      "render_subrequest",
		"/products/geanove-60cpr/related_products.turbo_stream":        "render_subrequest",
		"/products/geanove-60cpr/reviews":                              Other,
		"/blogger":                                                     Other,
		"/rimedi-naturali":                                             Other,
		"/brandsx":                                                     Other,
		"/header":                                                      "render_subrequest",
		"/bundles/123":                                                 "render_subrequest",
		"/cart":                                                        Other,
		"/headers":                                                     Other,
		"https://www.1000farmacie.it/categoria%20con%20spazio.htm":     "category",
	}
	for in, want := range cases {
		if got := c.Classify(in); got != want {
			t.Errorf("Classify(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestNewValidation(t *testing.T) {
	bad := [][]Rule{
		{{Name: "", Path: "^/"}},
		{{Name: Other, Path: "^/"}},
		{{Name: "a", Path: "^/"}, {Name: "a", Path: "^/x"}},
		{{Name: "a", Path: ""}},
		{{Name: "a", Path: "("}},
		{{Name: "a", Path: "^/", Query: "("}},
	}
	for i, rules := range bad {
		if _, err := New(rules); err == nil {
			t.Errorf("case %d: expected error", i)
		}
	}
}

func TestNames(t *testing.T) {
	c, err := New([]Rule{{Name: "a", Path: "^/a"}, {Name: "b", Path: "^/b"}})
	if err != nil {
		t.Fatal(err)
	}
	got := c.Names()
	want := []string{"a", "b", Other}
	if len(got) != len(want) {
		t.Fatalf("Names() = %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("Names() = %v, want %v", got, want)
		}
	}
}
