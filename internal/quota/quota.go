// Package quota models Google's URL Inspection quota: the daily allowance
// resets at midnight America/Los_Angeles, and the per-minute allowance is
// enforced client-side with a Limiter shared across workers.
package quota

import (
	"context"
	"sync"
	"time"
	_ "time/tzdata" // embed the zone database so the quota day is right on minimal hosts
)

// Zone is the time zone in which Google's daily quota resets.
const Zone = "America/Los_Angeles"

var location = sync.OnceValue(func() *time.Location {
	loc, err := time.LoadLocation(Zone)
	if err != nil {
		// Unreachable with time/tzdata embedded; PST is the safe fallback.
		return time.FixedZone("PST", -8*60*60)
	}
	return loc
})

// Day returns the Google quota day of t as YYYY-MM-DD.
func Day(t time.Time) string {
	return t.In(location()).Format("2006-01-02")
}

// Limiter spaces calls evenly (60s / perMinute apart). It is safe for
// concurrent use: every caller of Wait reserves the next free slot, so N
// workers sharing one Limiter still respect the overall rate.
type Limiter struct {
	mu       sync.Mutex
	interval time.Duration
	next     time.Time // earliest start of the next permitted call

	now   func() time.Time
	sleep func(ctx context.Context, d time.Duration) error
}

// NewLimiter returns a Limiter for perMinute calls per minute. perMinute <= 0
// disables limiting.
func NewLimiter(perMinute int) *Limiter {
	l := &Limiter{now: time.Now, sleep: sleepCtx}
	if perMinute > 0 {
		l.interval = time.Minute / time.Duration(perMinute)
	}
	return l
}

// Interval is the minimum spacing between two calls (0 = unlimited).
func (l *Limiter) Interval() time.Duration {
	if l == nil {
		return 0
	}
	return l.interval
}

// Wait blocks until the caller may issue its call, or ctx is done (in which
// case it returns ctx's error). A nil Limiter never blocks.
func (l *Limiter) Wait(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if l == nil || l.interval <= 0 {
		return nil
	}
	l.mu.Lock()
	now := l.now()
	slot := l.next
	if slot.Before(now) {
		slot = now
	}
	l.next = slot.Add(l.interval)
	l.mu.Unlock()

	d := slot.Sub(now)
	if d <= 0 {
		return nil
	}
	return l.sleep(ctx, d)
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
