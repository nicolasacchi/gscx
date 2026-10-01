package store

import (
	"context"
	"encoding/json"
	"testing"
)

func TestLatestPerURLRich(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)

	const richOld = `{"inspectionResultLink":"l","richResultsResult":{"verdict":"PASS","detectedItems":[{"richResultType":"Breadcrumbs","items":[{"name":"x"}]}]}}`
	const richNew = `{"inspectionResultLink":"l","richResultsResult":{"verdict":"PASS","detectedItems":[{"richResultType":"Product snippets","items":[{"name":"Lift & Repair","issues":[{"issueMessage":"Missing field \"availability\"","severity":"WARNING"}]}]}]}}`
	insert(t, s, Inspection{URL: "a", InspectedAt: at("2026-09-10T10:00:00Z"), Verdict: "PASS", RawJSON: richOld})
	insert(t, s, Inspection{URL: "a", InspectedAt: at("2026-09-20T10:00:00Z"), Verdict: "PASS", RawJSON: richNew})
	insert(t, s, Inspection{URL: "a", InspectedAt: at("2026-09-25T10:00:00Z"), Error: "later failure"})
	insert(t, s, Inspection{URL: "b", InspectedAt: at("2026-09-15T10:00:00Z"), Verdict: "NEUTRAL", RawJSON: `{"indexStatusResult":{}}`})
	insert(t, s, Inspection{URL: "c", InspectedAt: at("2026-09-15T10:00:00Z"), Verdict: "PASS"})                       // empty raw_json
	insert(t, s, Inspection{URL: "d", InspectedAt: at("2026-09-15T10:00:00Z"), Verdict: "PASS", RawJSON: `{"richRes`}) // truncated
	insert(t, s, Inspection{URL: "e", InspectedAt: at("2026-08-01T10:00:00Z"), Verdict: "PASS", RawJSON: richOld})     // outside window
	insert(t, s, Inspection{URL: "x", Site: "sc-domain:other.com", InspectedAt: at("2026-09-15T10:00:00Z"), RawJSON: richOld})

	got, err := s.LatestPerURLRich(ctx, site, at("2026-09-01T00:00:00Z"), at("2026-10-01T00:00:00Z"))
	if err != nil {
		t.Fatal(err)
	}
	var urls []string
	rich := map[string]string{}
	for _, r := range got {
		urls = append(urls, r.URL)
		rich[r.URL] = r.RichResults
		if r.RawJSON != "" {
			t.Errorf("%s: the full raw result must not be loaded", r.URL)
		}
	}
	if want := []string{"a", "b", "c", "d"}; len(urls) != len(want) || urls[0] != "a" || urls[3] != "d" {
		t.Fatalf("urls = %v, want %v (ordered by url, latest successful, in window, this site)", urls, want)
	}

	// The latest successful inspection of "a" wins, and the fragment is the
	// richResultsResult object only, with JSON escapes intact.
	var frag struct {
		Verdict       string
		DetectedItems []struct {
			RichResultType string
			Items          []struct {
				Name   string
				Issues []struct{ IssueMessage, Severity string }
			}
		}
	}
	if err := json.Unmarshal([]byte(rich["a"]), &frag); err != nil {
		t.Fatalf("fragment %q: %v", rich["a"], err)
	}
	if frag.Verdict != "PASS" || len(frag.DetectedItems) != 1 || frag.DetectedItems[0].RichResultType != "Product snippets" ||
		frag.DetectedItems[0].Items[0].Name != "Lift & Repair" ||
		frag.DetectedItems[0].Items[0].Issues[0].IssueMessage != `Missing field "availability"` {
		t.Errorf("fragment = %+v", frag)
	}
	for _, u := range []string{"b", "c", "d"} {
		if rich[u] != "" {
			t.Errorf("%s: fragment = %q, want empty (no richResultsResult / unusable raw_json)", u, rich[u])
		}
	}
}

func TestLatestPerURLUnchangedByRefactor(t *testing.T) {
	// LatestPerURL and LatestPerURLRich must agree on which rows they return.
	ctx := context.Background()
	s := openTemp(t)
	insert(t, s, Inspection{URL: "u", InspectedAt: at("2026-09-10T10:00:00Z"), Verdict: "NEUTRAL"})
	insert(t, s, Inspection{URL: "u", InspectedAt: at("2026-09-12T10:00:00Z"), Verdict: "PASS"})
	insert(t, s, Inspection{URL: "v", InspectedAt: at("2026-09-11T10:00:00Z"), Verdict: "FAIL"})
	from, to := at("2026-09-01T00:00:00Z"), at("2026-10-01T00:00:00Z")
	plain, err := s.LatestPerURL(ctx, site, from, to)
	if err != nil {
		t.Fatal(err)
	}
	rich, err := s.LatestPerURLRich(ctx, site, from, to)
	if err != nil {
		t.Fatal(err)
	}
	if len(plain) != 2 || len(rich) != 2 {
		t.Fatalf("plain=%d rich=%d", len(plain), len(rich))
	}
	for i := range plain {
		if plain[i].ID != rich[i].ID || plain[i].Verdict != rich[i].Verdict {
			t.Errorf("row %d differs: %+v vs %+v", i, plain[i], rich[i].Inspection)
		}
	}
}
