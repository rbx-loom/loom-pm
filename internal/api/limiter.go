package api

import (
	"sync"
	"time"
)

// idleBuckets is when a sweep starts: a bucket that has refilled is indistinguishable from
// one that never existed, so holding it only costs memory.
const idleBuckets = 4096

// limiter is a token bucket per key.
//
// Keyed by user rather than by address, because the cost being bounded is what one
// credential may write, and a publisher behind a shared address must not be spending
// somebody else's allowance.
type limiter struct {
	every time.Duration
	burst float64

	mu      sync.Mutex
	buckets map[int64]*bucket
}

type bucket struct {
	tokens float64
	filled time.Time
}

func newLimiter(every time.Duration, burst int) *limiter {
	return &limiter{every: every, burst: float64(burst), buckets: map[int64]*bucket{}}
}

// allow takes a token for key, answering how long until the next one when there is none.
func (l *limiter) allow(key int64, now time.Time) (time.Duration, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()

	held, ok := l.buckets[key]
	if !ok {
		if len(l.buckets) >= idleBuckets {
			l.sweep(now)
		}

		held = &bucket{tokens: l.burst, filled: now}
		l.buckets[key] = held
	}

	held.tokens = min(l.burst, held.tokens+now.Sub(held.filled).Seconds()/l.every.Seconds())
	held.filled = now

	if held.tokens < 1 {
		return time.Duration((1 - held.tokens) * float64(l.every)), false
	}

	held.tokens--
	return 0, true
}

// sweep drops the buckets that have refilled, which are the ones that would answer the
// same as a bucket that is not there at all.
func (l *limiter) sweep(now time.Time) {
	for key, held := range l.buckets {
		if held.tokens+now.Sub(held.filled).Seconds()/l.every.Seconds() >= l.burst {
			delete(l.buckets, key)
		}
	}
}
