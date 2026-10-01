package datadog

import (
	"context"
	"strconv"
	"sync"
	"time"
)

// The Logs Aggregate API is aggressively rate limited: at the time of writing
// the response carries "x-ratelimit-name: logs_public_analytics_aggregate",
// "x-ratelimit-limit: 2" and "x-ratelimit-period: 10", i.e. two requests per
// ten seconds per organisation. The pacer keeps the client inside that budget
// (a sliding window of the last `limit` send times) so a full crawl run does
// not burn its retries on 429s, and it re-reads the limit from every response
// so a different plan or org needs no code change.
const (
	defaultRateLimit  = 2
	defaultRatePeriod = 10 * time.Second
	// pacerMargin is added to every window to absorb clock skew between our
	// send timestamp and the server's receive timestamp.
	pacerMargin = 250 * time.Millisecond
	// maxPacerSleep bounds one sleep; wait re-evaluates after each slice.
	maxPacerSleep = time.Second
)

type pacer struct {
	mu          sync.Mutex
	limit       int
	period      time.Duration
	times       []time.Time // send times within the current window, oldest first
	blockedTill time.Time   // server told us remaining == 0 until this instant

	now   func() time.Time
	sleep func(ctx context.Context, d time.Duration) error
}

func newPacer(limit int, period time.Duration) *pacer {
	if limit <= 0 {
		limit = defaultRateLimit
	}
	if period <= 0 {
		period = defaultRatePeriod
	}
	return &pacer{limit: limit, period: period, now: time.Now, sleep: sleepCtx}
}

// Period is the current rate-limit window.
func (p *pacer) Period() time.Duration {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.period
}

// wait blocks until a request may be sent and records it. It returns the
// context error if ctx ends first.
func (p *pacer) wait(ctx context.Context) error {
	for {
		p.mu.Lock()
		now := p.now()
		window := p.period + pacerMargin
		drop := 0
		for drop < len(p.times) && now.Sub(p.times[drop]) >= window {
			drop++
		}
		p.times = p.times[drop:]

		var delay time.Duration
		switch {
		case now.Before(p.blockedTill):
			delay = p.blockedTill.Sub(now)
		case len(p.times) >= p.limit:
			delay = window - now.Sub(p.times[0])
		default:
			p.times = append(p.times, now)
			p.mu.Unlock()
			return nil
		}
		p.mu.Unlock()
		// Sleep in short slices so limits learned from a response while we
		// wait (observe) take effect promptly.
		if delay > maxPacerSleep {
			delay = maxPacerSleep
		}
		if err := p.sleep(ctx, delay); err != nil {
			return err
		}
	}
}

// observe updates the pacer from a response's x-ratelimit-* headers. Missing
// or malformed headers leave the current settings untouched.
func (p *pacer) observe(h rateHeaders) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if h.limit > 0 {
		p.limit = h.limit
	}
	if h.period > 0 {
		p.period = h.period
	}
	if h.hasRemaining && h.remaining <= 0 && h.reset > 0 {
		if until := p.now().Add(h.reset + pacerMargin); until.After(p.blockedTill) {
			p.blockedTill = until
		}
	}
}

// rateHeaders is the parsed x-ratelimit-* header set.
type rateHeaders struct {
	limit        int
	period       time.Duration
	remaining    int
	hasRemaining bool
	reset        time.Duration
}

func parseRateHeaders(get func(string) string) rateHeaders {
	var h rateHeaders
	if n, err := strconv.Atoi(get("X-Ratelimit-Limit")); err == nil {
		h.limit = n
	}
	if n, err := strconv.Atoi(get("X-Ratelimit-Period")); err == nil {
		h.period = time.Duration(n) * time.Second
	}
	if n, err := strconv.Atoi(get("X-Ratelimit-Remaining")); err == nil {
		h.remaining, h.hasRemaining = n, true
	}
	if n, err := strconv.Atoi(get("X-Ratelimit-Reset")); err == nil {
		h.reset = time.Duration(n) * time.Second
	}
	return h
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
