package middleware

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

// In-package because Limiter.now is unexported. Its comment says it is
// "injectable so a test does not have to sleep", and this is that test — a
// limiter tested by sleeping is a slow test that is also flaky on a loaded
// machine.

// clock is a hand-wound replacement for time.Now.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func newClock() *clock {
	return &clock{t: time.Date(2026, time.July, 27, 12, 0, 0, 0, time.UTC)}
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// wound builds a limiter driven by the returned clock.
func wound(perMinute, maxKeys int) (*Limiter, *clock) {
	l := NewLimiter(perMinute, maxKeys)
	c := newClock()
	l.now = c.Now
	return l, c
}

// TestLimiterBurstThenRefusal is the shape Phase 12 measured by hand: 25 rapid
// booking POSTs gave exactly 20 accepted then 5 refused.
func TestLimiterBurstThenRefusal(t *testing.T) {
	l, _ := wound(20, 100) // RATE_LIMIT_BOOKING's default

	for i := range 20 {
		if ok, _ := l.Allow("203.0.113.7"); !ok {
			t.Fatalf("request %d was refused inside the burst of 20", i+1)
		}
	}
	for i := range 5 {
		ok, retry := l.Allow("203.0.113.7")
		if ok {
			t.Fatalf("request %d past the burst was allowed", 21+i)
		}
		// A Retry-After of 0 invites an immediate retry that is also refused.
		if retry <= 0 {
			t.Errorf("Retry-After = %v, want a positive wait", retry)
		}
	}
}

// TestLimiterRefillsContinuously: a token bucket rather than a fixed window,
// because a window resets on a clock edge and a caller can spend a full
// allowance on either side of it — sending 2N requests back to back.
func TestLimiterRefillsContinuously(t *testing.T) {
	l, c := wound(60, 100) // one token per second

	for range 60 {
		if ok, _ := l.Allow("a"); !ok {
			t.Fatal("a request inside the burst was refused")
		}
	}
	if ok, _ := l.Allow("a"); ok {
		t.Fatal("the bucket did not run dry")
	}

	// One second buys exactly one token.
	c.Advance(time.Second)
	if ok, _ := l.Allow("a"); !ok {
		t.Error("no token after a second at one per second")
	}
	if ok, _ := l.Allow("a"); ok {
		t.Error("a second later, two tokens had arrived")
	}

	// Ten seconds buys ten.
	c.Advance(10 * time.Second)
	for i := range 10 {
		if ok, _ := l.Allow("a"); !ok {
			t.Fatalf("token %d of 10 did not arrive after ten seconds", i+1)
		}
	}
	if ok, _ := l.Allow("a"); ok {
		t.Error("an eleventh token arrived after ten seconds")
	}
}

// TestLimiterDoesNotRefillPastBurst: an idle caller may not bank an unlimited
// allowance and then spend it all at once.
func TestLimiterDoesNotRefillPastBurst(t *testing.T) {
	l, c := wound(20, 100)

	if ok, _ := l.Allow("a"); !ok {
		t.Fatal("the first request was refused")
	}
	// An hour idle, against a one-minute budget.
	c.Advance(time.Hour)

	allowed := 0
	for range 100 {
		if ok, _ := l.Allow("a"); ok {
			allowed++
		}
	}
	if allowed != 20 {
		t.Errorf("an hour of idling bought %d requests, want the burst of 20", allowed)
	}
}

// TestLimiterKeysAreIndependent: exhausting the booking budget from one address
// must not lock anyone else out.
func TestLimiterKeysAreIndependent(t *testing.T) {
	l, _ := wound(5, 100)

	for range 5 {
		if ok, _ := l.Allow("a"); !ok {
			t.Fatal("a request inside the burst was refused")
		}
	}
	if ok, _ := l.Allow("a"); ok {
		t.Fatal("key a did not run dry")
	}
	if ok, _ := l.Allow("b"); !ok {
		t.Error("key b was refused because key a was exhausted")
	}
}

// TestLimiterZeroDisables: config documents 0 as the way to turn a limit off, and
// validate() rejects a negative as a typo.
func TestLimiterZeroDisables(t *testing.T) {
	for _, perMinute := range []int{0, -1} {
		l, _ := wound(perMinute, 100)
		for i := range 1000 {
			if ok, retry := l.Allow("a"); !ok {
				t.Fatalf("perMinute=%d refused request %d (retry %v)", perMinute, i, retry)
			}
		}
	}
}

// TestLimiterRetryAfterIsAlwaysAtLeastASecond. It is rendered into a Retry-After
// header, where 0 means "try again now" — and the answer would be another 429.
func TestLimiterRetryAfterIsAlwaysAtLeastASecond(t *testing.T) {
	l, c := wound(120, 100) // two tokens a second, so the natural wait is under one

	for range 120 {
		l.Allow("a")
	}
	for range 20 {
		ok, retry := l.Allow("a")
		if ok {
			t.Fatal("the bucket refilled without the clock moving")
		}
		if retry < time.Second {
			t.Fatalf("Retry-After = %v, want at least a second", retry)
		}
		c.Advance(10 * time.Millisecond)
	}
}

// TestLimiterEvictsIdleBuckets: a map keyed by client IP is itself a
// memory-exhaustion vector, so it is bounded and swept.
func TestLimiterEvictsIdleBuckets(t *testing.T) {
	const maxKeys = 10
	l, c := wound(20, maxKeys)

	for i := range maxKeys {
		l.Allow(fmt.Sprintf("old-%d", i))
	}
	if got := len(l.buckets); got != maxKeys {
		t.Fatalf("map holds %d buckets, want %d", got, maxKeys)
	}

	// Past the 10-minute ttl, so the sweep can reclaim all of them.
	c.Advance(11 * time.Minute)
	l.Allow("new")

	if len(l.buckets) > maxKeys {
		t.Errorf("map grew to %d past the cap of %d", len(l.buckets), maxKeys)
	}
	if _, still := l.buckets["old-0"]; still {
		t.Error("an idle bucket survived the sweep")
	}
	if _, ok := l.buckets["new"]; !ok {
		t.Error("the new bucket was not recorded")
	}
}

// TestLimiterClearsWhenNothingIsIdle. Crude but bounded: it happens only when
// maxKeys distinct callers are all inside the ttl, which is an attack rather than
// a Tuesday, and the cost of being wrong is one caller getting a fresh bucket.
func TestLimiterClearsWhenNothingIsIdle(t *testing.T) {
	const maxKeys = 10
	l, _ := wound(20, maxKeys)

	for i := range maxKeys {
		l.Allow(fmt.Sprintf("live-%d", i))
	}
	l.Allow("one-more") // no bucket is idle, so the map is cleared wholesale

	if len(l.buckets) > maxKeys {
		t.Errorf("map holds %d buckets, past the cap of %d", len(l.buckets), maxKeys)
	}
	// Being reset to a fresh bucket is at worst unlimited, which is exactly where
	// the app was before the limiter existed — never a crash or a lockout.
	if ok, _ := l.Allow("live-0"); !ok {
		t.Error("a caller was locked out by the eviction rather than reset")
	}
}

// TestLimiterIsSafeUnderConcurrency: it is shared across every request for its
// route, so -race must find nothing.
func TestLimiterIsSafeUnderConcurrency(t *testing.T) {
	l := NewLimiter(1000, 100) // the real clock; this is about the mutex, not timing

	var wg sync.WaitGroup
	for i := range 50 {
		wg.Go(func() {
			for range 20 {
				l.Allow(fmt.Sprintf("client-%d", i%5))
			}
		})
	}
	wg.Wait()
}
