package datadog

import (
	"context"
	"errors"
	"testing"
	"time"
)

// fakeClock drives a pacer without real sleeping: sleep advances the clock.
type fakeClock struct {
	now    time.Time
	slept  []time.Duration
	sleeps int
}

func (f *fakeClock) Now() time.Time { return f.now }
func (f *fakeClock) Sleep(_ context.Context, d time.Duration) error {
	f.sleeps++
	f.slept = append(f.slept, d)
	f.now = f.now.Add(d)
	return nil
}

func testPacer(limit int, period time.Duration) (*pacer, *fakeClock) {
	fc := &fakeClock{now: time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)}
	p := newPacer(limit, period)
	p.now, p.sleep = fc.Now, fc.Sleep
	return p, fc
}

func TestPacerBurstThenWaitsForWindow(t *testing.T) {
	p, fc := testPacer(2, 10*time.Second)
	ctx := context.Background()
	start := fc.now
	for i := 0; i < 2; i++ {
		if err := p.wait(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if fc.sleeps != 0 {
		t.Fatalf("the first two requests fit the window, slept %v", fc.slept)
	}
	if err := p.wait(ctx); err != nil {
		t.Fatal(err)
	}
	if got, min := fc.now.Sub(start), 10*time.Second; got < min {
		t.Errorf("third request released after %s, want >= %s", got, min)
	}
}

// At most `limit` requests may start within any window-sized span.
func TestPacerNeverExceedsLimitInAnyWindow(t *testing.T) {
	p, fc := testPacer(2, 10*time.Second)
	var sent []time.Time
	for i := 0; i < 12; i++ {
		if err := p.wait(context.Background()); err != nil {
			t.Fatal(err)
		}
		sent = append(sent, fc.now)
	}
	for i := 2; i < len(sent); i++ {
		if sent[i].Sub(sent[i-2]) < 10*time.Second {
			t.Fatalf("requests %d..%d within %s", i-2, i, sent[i].Sub(sent[i-2]))
		}
	}
}

func TestPacerHonoursContext(t *testing.T) {
	p, _ := testPacer(1, time.Hour)
	if err := p.wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	p.sleep = sleepCtx // real sleep, cancelled context returns immediately
	if err := p.wait(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
}

func TestPacerObserveAdoptsServerLimits(t *testing.T) {
	p, fc := testPacer(2, 10*time.Second)
	p.observe(rateHeaders{limit: 5, period: 3 * time.Second})
	if p.limit != 5 || p.period != 3*time.Second {
		t.Fatalf("limit=%d period=%s", p.limit, p.period)
	}
	// remaining == 0 blocks until the server's reset.
	p.observe(rateHeaders{remaining: 0, hasRemaining: true, reset: 7 * time.Second})
	start := fc.now
	if err := p.wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	if fc.now.Sub(start) < 7*time.Second {
		t.Errorf("waited %s, want >= 7s", fc.now.Sub(start))
	}
}

func TestParseRateHeaders(t *testing.T) {
	h := map[string]string{
		"X-Ratelimit-Limit": "2", "X-Ratelimit-Period": "10", "X-Ratelimit-Remaining": "0", "X-Ratelimit-Reset": "7",
	}
	got := parseRateHeaders(func(k string) string { return h[k] })
	want := rateHeaders{limit: 2, period: 10 * time.Second, remaining: 0, hasRemaining: true, reset: 7 * time.Second}
	if got != want {
		t.Errorf("got %+v, want %+v", got, want)
	}
	if got := parseRateHeaders(func(string) string { return "" }); got != (rateHeaders{}) {
		t.Errorf("missing headers must parse to zero value, got %+v", got)
	}
}
