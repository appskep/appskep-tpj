package middleware

import (
	"sync"
	"time"
)

// Limiter is a token bucket per key, with the keys held in memory.
//
// One process, one map: this app runs as a single binary and there is no shared
// cache to put a counter in. A second instance would each get their own budget,
// which is a limit that is twice as loose — not one that fails open.
//
// A token bucket rather than a fixed window because a window resets on a clock
// edge, so a caller can spend a full allowance on either side of it and send 2N
// requests back to back. The bucket refills continuously and burst is explicit.
type Limiter struct {
	// rate is tokens added per second, burst the ceiling a bucket refills to.
	rate  float64
	burst float64
	// ttl is how long an untouched bucket survives a sweep.
	ttl time.Duration
	// maxKeys bounds the map. A map keyed by client IP is itself a memory
	// exhaustion vector: without this, spraying requests from many source
	// addresses grows it without limit. At the cap the oldest entries are dropped
	// — a caller who gets a fresh bucket is at worst unlimited, which is exactly
	// where we were before the limiter existed.
	maxKeys int

	mu      sync.Mutex
	buckets map[string]*bucket
	// now is injectable so a test does not have to sleep.
	now func() time.Time
}

type bucket struct {
	tokens float64
	seen   time.Time
}

// NewLimiter builds a limiter allowing perMinute requests per key, sustained,
// with a burst of the same size. perMinute <= 0 disables it: Allow always says
// yes, which is what a config of 0 should mean.
func NewLimiter(perMinute int, maxKeys int) *Limiter {
	l := &Limiter{
		rate:    float64(perMinute) / 60,
		burst:   float64(perMinute),
		ttl:     10 * time.Minute,
		maxKeys: maxKeys,
		buckets: make(map[string]*bucket),
		now:     time.Now,
	}
	return l
}

// Allow spends a token for key, reporting whether there was one, and how long
// until the next token arrives when there was not.
func (l *Limiter) Allow(key string) (bool, time.Duration) {
	if l.burst <= 0 {
		return true, 0
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	now := l.now()
	b, ok := l.buckets[key]
	if !ok {
		if len(l.buckets) >= l.maxKeys {
			l.evict(now)
		}
		b = &bucket{tokens: l.burst}
		l.buckets[key] = b
	} else {
		b.tokens += now.Sub(b.seen).Seconds() * l.rate
		if b.tokens > l.burst {
			b.tokens = l.burst
		}
	}
	b.seen = now

	if b.tokens < 1 {
		// Round up: a Retry-After of 0 invites an immediate retry that is also
		// refused.
		wait := time.Duration((1 - b.tokens) / l.rate * float64(time.Second))
		return false, wait.Round(time.Second) + time.Second
	}

	b.tokens--
	return true, 0
}

// evict drops idle buckets, and if that frees nothing, the whole map.
//
// Clearing wholesale is crude but bounded and predictable: it happens only when
// maxKeys distinct callers are all inside the ttl, which is an attack rather than
// a Tuesday, and the alternative is tracking an LRU for a defence whose cost of
// being wrong is one caller getting a fresh bucket.
//
// Caller holds the lock.
func (l *Limiter) evict(now time.Time) {
	for k, b := range l.buckets {
		if now.Sub(b.seen) > l.ttl {
			delete(l.buckets, k)
		}
	}
	if len(l.buckets) >= l.maxKeys {
		clear(l.buckets)
	}
}
