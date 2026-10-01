package gscdate

import (
	"testing"
	"time"
)

func mustDate(t *testing.T, s string) time.Time {
	t.Helper()
	d, err := Parse(s)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func TestTodayAtUsesPacific(t *testing.T) {
	cases := []struct {
		name string
		now  time.Time
		want string
	}{
		// 03:00 UTC on Oct 1 is still Sep 30 evening in Pacific (PDT, UTC-7).
		{"utc early morning is previous pacific day", time.Date(2026, 10, 1, 3, 0, 0, 0, time.UTC), "2026-09-30"},
		{"utc afternoon is same pacific day", time.Date(2026, 10, 1, 20, 0, 0, 0, time.UTC), "2026-10-01"},
		// Winter: PST (UTC-8); 07:59 UTC is still the previous day.
		{"winter before 08:00 UTC", time.Date(2026, 1, 15, 7, 59, 0, 0, time.UTC), "2026-01-14"},
		{"winter after 08:00 UTC", time.Date(2026, 1, 15, 8, 0, 0, 0, time.UTC), "2026-01-15"},
	}
	for _, tc := range cases {
		got := TodayAt(tc.now)
		if Format(got) != tc.want {
			t.Errorf("%s: TodayAt = %s, want %s", tc.name, Format(got), tc.want)
		}
		if got.Hour() != 0 || got.Location() != Location() {
			t.Errorf("%s: not Pacific midnight: %v", tc.name, got)
		}
	}
}

func TestDefaultEndAt(t *testing.T) {
	now := time.Date(2026, 10, 1, 3, 0, 0, 0, time.UTC) // Pacific: Sep 30
	if got := Format(DefaultEndAt(now)); got != "2026-09-27" {
		t.Fatalf("DefaultEndAt = %s, want 2026-09-27", got)
	}
}

func TestParseFormat(t *testing.T) {
	d, err := Parse("2026-03-08")
	if err != nil {
		t.Fatal(err)
	}
	if Format(d) != "2026-03-08" {
		t.Fatalf("round trip = %s", Format(d))
	}
	for _, bad := range []string{"", "2026-3-8x", "08/03/2026", "2026-13-01", "2026-02-30"} {
		if _, err := Parse(bad); err == nil {
			t.Errorf("Parse(%q) should fail", bad)
		}
	}
}

func TestAddDaysAcrossDST(t *testing.T) {
	// US DST starts Sunday 2026-03-08: the day is 23 hours long.
	d := mustDate(t, "2026-03-07")
	if got := Format(AddDays(d, 3)); got != "2026-03-10" {
		t.Fatalf("AddDays across DST = %s, want 2026-03-10", got)
	}
	if got := DaysBetween(d, mustDate(t, "2026-03-10")); got != 3 {
		t.Fatalf("DaysBetween across DST = %d, want 3", got)
	}
}

func TestWeekStart(t *testing.T) {
	cases := map[string]string{
		"2026-09-21": "2026-09-21", // Monday
		"2026-09-23": "2026-09-21", // Wednesday
		"2026-09-27": "2026-09-21", // Sunday belongs to the week that started Monday 21st
		"2026-09-28": "2026-09-28", // next Monday
		"2026-03-08": "2026-03-02", // DST Sunday
	}
	for in, want := range cases {
		if got := Format(WeekStart(mustDate(t, in))); got != want {
			t.Errorf("WeekStart(%s) = %s, want %s", in, got, want)
		}
	}
}

func TestLastCompleteWeeks(t *testing.T) {
	// DefaultEnd on 2026-09-30 is 2026-09-27 (a Sunday): that week is complete.
	weeks := LastCompleteWeeks(3, mustDate(t, "2026-09-27"))
	if len(weeks) != 3 {
		t.Fatalf("len = %d", len(weeks))
	}
	want := [][2]string{{"2026-09-07", "2026-09-13"}, {"2026-09-14", "2026-09-20"}, {"2026-09-21", "2026-09-27"}}
	for i, w := range weeks {
		if w.StartString() != want[i][0] || w.EndString() != want[i][1] {
			t.Errorf("week %d = %s..%s, want %s..%s", i, w.StartString(), w.EndString(), want[i][0], want[i][1])
		}
	}

	// A midweek end (Wednesday 2026-09-23) excludes the incomplete current week.
	weeks = LastCompleteWeeks(2, mustDate(t, "2026-09-23"))
	if weeks[1].EndString() != "2026-09-20" || weeks[0].StartString() != "2026-09-07" {
		t.Errorf("midweek end: got %s..%s", weeks[0].StartString(), weeks[1].EndString())
	}

	if LastCompleteWeeks(0, mustDate(t, "2026-09-27")) != nil {
		t.Error("n=0 should return nil")
	}
}

func TestWeeksBetween(t *testing.T) {
	cases := []struct {
		name       string
		start, end string
		wantFirst  string
		wantLast   string
		wantN      int
	}{
		{"aligned", "2026-09-07", "2026-09-27", "2026-09-07", "2026-09-27", 3},
		{"start midweek snaps forward", "2026-09-09", "2026-09-27", "2026-09-14", "2026-09-27", 2},
		{"end midweek snaps back", "2026-09-07", "2026-09-24", "2026-09-07", "2026-09-20", 2},
		{"single week", "2026-09-21", "2026-09-27", "2026-09-21", "2026-09-27", 1},
		{"no complete week", "2026-09-22", "2026-09-27", "", "", 0},
		{"end before start", "2026-09-27", "2026-09-07", "", "", 0},
	}
	for _, tc := range cases {
		weeks := WeeksBetween(mustDate(t, tc.start), mustDate(t, tc.end))
		if len(weeks) != tc.wantN {
			t.Errorf("%s: %d weeks, want %d", tc.name, len(weeks), tc.wantN)
			continue
		}
		if tc.wantN == 0 {
			continue
		}
		if weeks[0].StartString() != tc.wantFirst || weeks[len(weeks)-1].EndString() != tc.wantLast {
			t.Errorf("%s: %s..%s, want %s..%s", tc.name, weeks[0].StartString(), weeks[len(weeks)-1].EndString(), tc.wantFirst, tc.wantLast)
		}
	}
}
