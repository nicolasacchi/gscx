package datadog

import (
	"strings"

	"github.com/nicolasacchi/gscx/internal/pagetype"
)

// BaseQuery is the log filter every crawl count starts from: the access-log
// stream, the crawler user agent and, unless unverified, the crawler's network
// (user agents are trivially spoofed). Empty parts are skipped.
func BaseQuery(service, bot, verified string, unverified bool) string {
	parts := []string{strings.TrimSpace(service), strings.TrimSpace(bot)}
	if !unverified {
		parts = append(parts, strings.TrimSpace(verified))
	}
	return joinNonEmpty(parts)
}

// SpoofedQuery selects requests whose user agent claims to be the crawler but
// whose network is not the crawler's. Logs with no network attribution at all
// count as spoofed: they cannot be verified.
func SpoofedQuery(service, bot, verified string) string {
	verified = strings.TrimSpace(verified)
	if verified == "" {
		return ""
	}
	return joinNonEmpty([]string{strings.TrimSpace(service), strings.TrimSpace(bot), "-(" + verified + ")"})
}

// TypeQuery is the Datadog query counting one page type.
type TypeQuery struct {
	Type  string
	Query string
}

// TypeQueries builds one query per rule that has a DDQuery, mirroring the
// classifier's first-match-wins semantics so the counts are disjoint:
//
//	base (dd_i) -(dd_j) for every earlier rule j with a DDQuery
//
// Fragments are used verbatim (wrapped in parentheses, never rewritten), so
// escaping such as `\/` and `*` wildcards is kept exactly as configured.
//
// Rules without a DDQuery cannot be counted server-side. They are returned in
// uncounted: their requests land in "other" (or in a later type whose
// fragment also matches them), so the caller should warn.
func TypeQueries(base string, rules []pagetype.Rule) (queries []TypeQuery, uncounted []string) {
	var earlier []string
	for _, r := range rules {
		frag := strings.TrimSpace(r.DDQuery)
		if frag == "" {
			uncounted = append(uncounted, r.Name)
			continue
		}
		var b strings.Builder
		b.WriteString(base)
		b.WriteString(" (")
		b.WriteString(frag)
		b.WriteString(")")
		for _, prev := range earlier {
			b.WriteString(" -(")
			b.WriteString(prev)
			b.WriteString(")")
		}
		queries = append(queries, TypeQuery{Type: r.Name, Query: strings.TrimSpace(b.String())})
		earlier = append(earlier, frag)
	}
	return queries, uncounted
}

func joinNonEmpty(parts []string) string {
	out := parts[:0:0]
	for _, p := range parts {
		if p != "" {
			out = append(out, p)
		}
	}
	return strings.Join(out, " ")
}

// OtherQuery selects what no counted page type matches: base minus every
// rule's DDQuery fragment. It mirrors the "other" bucket of TypeQueries.
func OtherQuery(base string, rules []pagetype.Rule) string {
	var b strings.Builder
	b.WriteString(base)
	for _, r := range rules {
		if frag := strings.TrimSpace(r.DDQuery); frag != "" {
			b.WriteString(" -(")
			b.WriteString(frag)
			b.WriteString(")")
		}
	}
	return strings.TrimSpace(b.String())
}

// StatusFilter turns "301" or "3xx" into a Datadog status-code filter.
// ok is false for anything else.
func StatusFilter(s string) (filter string, ok bool) {
	s = strings.TrimSpace(strings.ToLower(s))
	if len(s) != 3 || s[0] < '1' || s[0] > '5' {
		return "", false
	}
	if s[1:] == "xx" {
		return "@http.status_code:[" + s[:1] + "00 TO " + s[:1] + "99]", true
	}
	for _, c := range s[1:] {
		if c < '0' || c > '9' {
			return "", false
		}
	}
	return "@http.status_code:" + s, true
}
