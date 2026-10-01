// Package gscdate holds the date helpers for Search Console, whose dates are
// calendar days in Pacific time (America/Los_Angeles) and whose data lags the
// present by about three days.
package gscdate

import (
	"fmt"
	"time"

	// Embed the zoneinfo database so Pacific time works without system tzdata.
	_ "time/tzdata"
)

// Layout is the Search Console date format (YYYY-MM-DD).
const Layout = "2006-01-02"

// Zone is the IANA zone Search Console reports dates in.
const Zone = "America/Los_Angeles"

// LagDays is how far the newest complete day trails the Pacific "today".
const LagDays = 3

var pacific = loadPacific()

func loadPacific() *time.Location {
	loc, err := time.LoadLocation(Zone)
	if err != nil {
		// Unreachable with time/tzdata embedded; PST is the safe approximation.
		return time.FixedZone("PST", -8*3600)
	}
	return loc
}

// Location returns the Pacific time zone.
func Location() *time.Location { return pacific }

// Today returns the current Pacific calendar day at 00:00.
func Today() time.Time { return TodayAt(time.Now()) }

// TodayAt returns the Pacific calendar day containing now, at 00:00.
func TodayAt(now time.Time) time.Time {
	p := now.In(pacific)
	return time.Date(p.Year(), p.Month(), p.Day(), 0, 0, 0, 0, pacific)
}

// DefaultEnd is the newest day with reasonably complete data: Pacific today
// minus LagDays.
func DefaultEnd() time.Time { return DefaultEndAt(time.Now()) }

// DefaultEndAt is DefaultEnd relative to now.
func DefaultEndAt(now time.Time) time.Time { return AddDays(TodayAt(now), -LagDays) }

// Parse reads a YYYY-MM-DD date as a Pacific calendar day at 00:00.
func Parse(s string) (time.Time, error) {
	t, err := time.ParseInLocation(Layout, s, pacific)
	if err != nil {
		return time.Time{}, fmt.Errorf("invalid date %q: want YYYY-MM-DD", s)
	}
	return t, nil
}

// Format renders t as YYYY-MM-DD in its own location.
func Format(t time.Time) string { return t.Format(Layout) }

// AddDays shifts a calendar day by n days (DST-safe: it moves the date, not
// 24-hour multiples).
func AddDays(t time.Time, n int) time.Time { return t.AddDate(0, 0, n) }

// DaysBetween returns the number of calendar days from a to b (b - a).
func DaysBetween(a, b time.Time) int {
	au := time.Date(a.Year(), a.Month(), a.Day(), 0, 0, 0, 0, time.UTC)
	bu := time.Date(b.Year(), b.Month(), b.Day(), 0, 0, 0, 0, time.UTC)
	return int(bu.Sub(au).Hours() / 24)
}

// WeekStart returns the Monday of the week containing t, at 00:00.
func WeekStart(t time.Time) time.Time {
	d := time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, t.Location())
	return d.AddDate(0, 0, -((int(d.Weekday()) + 6) % 7))
}

// Week is one Monday-to-Sunday calendar week (both ends inclusive).
type Week struct {
	Start time.Time
	End   time.Time
}

// NewWeek builds the week that starts on the given Monday.
func NewWeek(monday time.Time) Week {
	return Week{Start: monday, End: AddDays(monday, 6)}
}

// StartString and EndString format the week bounds as YYYY-MM-DD.
func (w Week) StartString() string { return Format(w.Start) }
func (w Week) EndString() string   { return Format(w.End) }

// LastCompleteWeeks returns n consecutive complete Monday-Sunday weeks in
// ascending order, the last one ending on or before end.
func LastCompleteWeeks(n int, end time.Time) []Week {
	if n <= 0 {
		return nil
	}
	lastSunday := AddDays(end, -int(end.Weekday())) // Sunday == 0
	first := AddDays(lastSunday, -(7*n - 1))
	return weeksFrom(first, n)
}

// WeeksBetween returns the complete Monday-Sunday weeks inside [start, end]:
// start snaps forward to the next Monday and end snaps back to the previous
// Sunday, so a partial week never shows up as a misleading dip. The result is
// empty when no complete week fits.
func WeeksBetween(start, end time.Time) []Week {
	first := start
	if first.Weekday() != time.Monday {
		first = AddDays(WeekStart(first), 7)
	}
	lastSunday := AddDays(end, -int(end.Weekday()))
	if lastSunday.Before(AddDays(first, 6)) {
		return nil
	}
	return weeksFrom(first, DaysBetween(first, lastSunday)/7+1)
}

func weeksFrom(first time.Time, n int) []Week {
	out := make([]Week, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, NewWeek(AddDays(first, 7*i)))
	}
	return out
}
