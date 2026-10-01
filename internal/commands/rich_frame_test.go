package commands

import "testing"

func TestApplyRichFrame(t *testing.T) {
	by := map[string]*richRow{
		"product": {N: 100, ByRichType: map[string]*richTypeRow{
			"Product snippets": {N: 98, Pct: 98, CI95: ci95{Lo: 93, Hi: 99.45}},
		}},
		"brand": {N: 20, ByRichType: map[string]*richTypeRow{
			"Breadcrumbs": {N: 20, Pct: 100, CI95: ci95{Lo: 83.89, Hi: 100}},
		}},
		"category": {N: 0, ByRichType: map[string]*richTypeRow{}},
	}
	overall := &richRow{N: 120, ByRichType: map[string]*richTypeRow{
		"Product snippets": {N: 98},
		"Breadcrumbs":      {N: 20},
	}}
	applyRichFrame(overall, by, map[string]int{"product": 114750, "brand": 4317, "category": 2346})

	ps := by["product"].ByRichType["Product snippets"].EstimatedURLs
	if ps == nil || ps.Point != 112455 || ps.Lo != 106718 || ps.Hi != 114119 {
		t.Fatalf("product estimate = %+v", ps)
	}
	if by["product"].FrameURLs == nil || *by["product"].FrameURLs != 114750 {
		t.Fatalf("product frame = %v", by["product"].FrameURLs)
	}
	if by["category"].FrameURLs != nil {
		t.Fatal("unsampled type must not get a frame/estimate")
	}
	if e := overall.ByRichType["Breadcrumbs"].EstimatedURLs; e == nil || e.Point != 4317 || e.Lo != 3622 {
		t.Fatalf("overall breadcrumbs = %+v", e)
	}
	if overall.FrameURLs == nil || *overall.FrameURLs != 121413 {
		t.Fatalf("overall frame = %v", overall.FrameURLs)
	}
}
