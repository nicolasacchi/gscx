// Package pagetype classifies URLs of the site into page types (product,
// category, brand, …). The same ordered rule list drives every command: the
// sitemap audit, the inspection sample strata, the impression trend and the
// Googlebot crawl breakdown, so their numbers are comparable per type.
package pagetype

import (
	"fmt"
	"net/url"
	"regexp"
	"strings"
)

// Other is the type assigned to URLs that match no rule.
const Other = "other"

// Rule is one page type. Rules are evaluated in order and the first match
// wins, so more specific rules (e.g. paginated categories) must come first.
type Rule struct {
	Name string `toml:"name" json:"name"`
	// Path is a regular expression matched against the URL path.
	Path string `toml:"path" json:"path"`
	// Query, if set, must also match the raw query string (without '?').
	Query string `toml:"query,omitempty" json:"query,omitempty"`
	// DDQuery is the equivalent Datadog log-search fragment, used by the crawl
	// command to count requests server-side. Empty means the type is not
	// counted separately in Datadog (it falls into "other").
	DDQuery string `toml:"dd_query,omitempty" json:"dd_query,omitempty"`
}

// Classifier holds compiled rules.
type Classifier struct {
	rules []compiled
}

type compiled struct {
	Rule
	path  *regexp.Regexp
	query *regexp.Regexp
}

// New compiles rules. Names must be unique and non-empty and must not be Other.
func New(rules []Rule) (*Classifier, error) {
	seen := map[string]bool{}
	c := &Classifier{}
	for i, r := range rules {
		if r.Name == "" {
			return nil, fmt.Errorf("page type #%d: empty name", i+1)
		}
		if r.Name == Other {
			return nil, fmt.Errorf("page type %q: name is reserved", r.Name)
		}
		if seen[r.Name] {
			return nil, fmt.Errorf("page type %q: duplicate name", r.Name)
		}
		seen[r.Name] = true
		if r.Path == "" {
			return nil, fmt.Errorf("page type %q: empty path regex", r.Name)
		}
		p, err := regexp.Compile(r.Path)
		if err != nil {
			return nil, fmt.Errorf("page type %q: path regex: %w", r.Name, err)
		}
		cr := compiled{Rule: r, path: p}
		if r.Query != "" {
			q, err := regexp.Compile(r.Query)
			if err != nil {
				return nil, fmt.Errorf("page type %q: query regex: %w", r.Name, err)
			}
			cr.query = q
		}
		c.rules = append(c.rules, cr)
	}
	return c, nil
}

// MustDefault returns the classifier for the built-in rules.
func MustDefault() *Classifier {
	c, err := New(Defaults())
	if err != nil {
		panic(err)
	}
	return c
}

// Classify returns the page type of rawURL, which may be an absolute URL or a
// bare path (optionally with a query string).
func (c *Classifier) Classify(rawURL string) string {
	path, query := split(rawURL)
	for _, r := range c.rules {
		if !r.path.MatchString(path) {
			continue
		}
		if r.query != nil && !r.query.MatchString(query) {
			continue
		}
		return r.Name
	}
	return Other
}

// Names returns the type names in rule order followed by Other.
func (c *Classifier) Names() []string {
	out := make([]string, 0, len(c.rules)+1)
	for _, r := range c.rules {
		out = append(out, r.Name)
	}
	return append(out, Other)
}

// Rules returns a copy of the rules in evaluation order.
func (c *Classifier) Rules() []Rule {
	out := make([]Rule, len(c.rules))
	for i, r := range c.rules {
		out[i] = r.Rule
	}
	return out
}

func split(raw string) (path, query string) {
	if u, err := url.Parse(raw); err == nil {
		path = u.EscapedPath()
		if p, err := url.PathUnescape(path); err == nil {
			path = p
		}
		query = u.RawQuery
	} else if i := strings.IndexByte(raw, '?'); i >= 0 {
		path, query = raw[:i], raw[i+1:]
	} else {
		path = raw
	}
	if path == "" {
		path = "/"
	}
	return path, query
}

// Defaults are the 1000farmacie.it page types. Order matters: first match wins.
func Defaults() []Rule {
	return []Rule{
		{Name: "home", Path: `^/$`, DDQuery: `@http.url_details.path:\/`},
		{Name: "category_paged", Path: `^/[^/]+\.htm/\d+/?$`, DDQuery: `@http.url_details.path:*.htm\/*`},
		{Name: "category", Path: `^/[^/]+\.htm$`, DDQuery: `@http.url_details.path:*.htm`},
		{Name: "product", Path: `^/[^/]+\.html$`, DDQuery: `@http.url_details.path:*.html`},
		{Name: "brand_paged", Path: `^/brands/[^/]+/?$`, Query: `(^|&)page=`, DDQuery: `@http.url_details.path:\/brands\/* @http.url:*page=*`},
		{Name: "brand", Path: `^/brands(/|$)`, DDQuery: `@http.url_details.path:\/brands*`},
		{Name: "blog", Path: `^/blog(/|$)`, DDQuery: `@http.url_details.path:\/blog*`},
		{Name: "rimedi", Path: `^/rimedi(/|$)`, DDQuery: `@http.url_details.path:\/rimedi*`},
		{Name: "sitemap", Path: `^/sitemaps/`, DDQuery: `@http.url_details.path:\/sitemaps\/*`},
		// Sub-requests issued while Google renders a page (Turbo streams,
		// header fragment, bundles). They are not crawl targets.
		{Name: "render_subrequest", Path: `^/(products/track_product_list_view|header$|bundles/)`, DDQuery: `@http.url_details.path:(\/products\/track_product_list_view* OR \/products\/*\/dynamic_details OR \/products\/*\/related_products* OR \/header OR \/bundles\/*)`},
	}
}
