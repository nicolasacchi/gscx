package quota

import (
	"context"
	"errors"
	"sort"
	"sync"
	"testing"
	"time"
)

func TestDay(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"PDT midnight boundary, last second", "2026-10-01T06:59:59Z", "2026-09-30"},
		{"PDT midnight boundary, first second", "2026-10-01T07:00:00Z", "2026-10-01"},
		{"PST midnight boundary, last second", "2026-12-01T07:59:59Z", "2026-11-30"},
		{"PST midnight boundary, first second", "2026-12-01T08:00:00Z", "2026-12-01"},
		{"UTC afternoon is same LA day", "2026-10-01T15:00:00Z", "2026-10-01"},
		{"non-UTC input", "2026-10-01T02:00:00+02:00", "2026-09-30"},
		{"DST spring forward day", "2026-03-08T09:59:59Z", "2026-03-08"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in, err := time.Parse(time.RFC3339, tc.in)
			if err != nil {
				t.Fatal(err)
			}
			if got := Day(in); got != tc.want {
				t.Errorf("Day(%s) = %s, want %s", tc.in, got, tc.want)
			}
		})
	}
}

// fakeLimiter returns a Limiter on a fake clock; slept collects every sleep.
func fakeLimiter(perMinute int, advance bool) (*Limiter, *[]time.Duration, *sync.Mutex) {
	var (
		mu    sync.Mutex
		clock = time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
		slept []time.Duration
	)
	l := NewLimiter(perMinute)
	l.now = func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		return clock
	}
	l.sleep = func(_ context.Context, d time.Duration) error {
		mu.Lock()
		defer mu.Unlock()
		slept = append(slept, d)
		if advance {
			clock = clock.Add(d)
		}
		return nil
	}
	return l, &slept, &mu
}

func TestLimiterSpacesSequentialCalls(t *testing.T) {
	l, slept, _ := fakeLimiter(60, true) // 1s apart
	for range 4 {
		if err := l.Wait(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	// first call is immediate; each following call waits the full interval.
	want := []time.Duration{time.Second, time.Second, time.Second}
	if len(*slept) != len(want) {
		t.Fatalf("slept = %v, want %v", *slept, want)
	}
	for i := range want {
		if (*slept)[i] != want[i] {
			t.Errorf("slept[%d] = %v, want %v", i, (*slept)[i], want[i])
		}
	}
}

func TestLimiterSharedAcrossWorkers(t *testing.T) {
	l, slept, mu := fakeLimiter(120, false) // 500ms apart, clock frozen
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := l.Wait(context.Background()); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	mu.Lock()
	defer mu.Unlock()
	got := append([]time.Duration(nil), (*slept)...)
	sort.Slice(got, func(i, j int) bool { return got[i] < got[j] })
	want := []time.Duration{500 * time.Millisecond, time.Second, 1500 * time.Millisecond}
	if len(got) != len(want) {
		t.Fatalf("slept = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("slot %d = %v, want %v", i, got[i], want[i])
		}
	}
}

func TestLimiterUnlimited(t *testing.T) {
	for _, l := range []*Limiter{nil, NewLimiter(0), NewLimiter(-5)} {
		for range 100 {
			if err := l.Wait(context.Background()); err != nil {
				t.Fatal(err)
			}
		}
		if l.Interval() != 0 {
			t.Errorf("Interval = %v, want 0", l.Interval())
		}
	}
}

func TestLimiterHonorsContext(t *testing.T) {
	l := NewLimiter(1) // 60s interval: second call would block a minute
	if err := l.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := l.Wait(ctx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want deadline exceeded", err)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatalf("Wait did not return promptly on ctx expiry")
	}

	cancelled, cancel2 := context.WithCancel(context.Background())
	cancel2()
	if err := NewLimiter(0).Wait(cancelled); !errors.Is(err, context.Canceled) {
		t.Errorf("already-cancelled ctx: err = %v, want canceled", err)
	}
}

func TestLimiterRealClockInterval(t *testing.T) {
	l := NewLimiter(6000) // 10ms apart
	start := time.Now()
	for range 5 {
		if err := l.Wait(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if el := time.Since(start); el < 35*time.Millisecond {
		t.Errorf("5 calls at 10ms spacing took %v, want >= 40ms-ish", el)
	}
}
