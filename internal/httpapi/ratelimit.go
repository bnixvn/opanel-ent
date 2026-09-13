package httpapi

import (
	"sync"
	"time"
)

// limiter is a fixed-window counter keyed by client IP, used to slow password
// guessing. It is intentionally in-process: the panel is a single binary on a
// single host, so a shared store would add a dependency and buy nothing.
type limiter struct {
	mu      sync.Mutex
	hits    map[string]*window
	limit   int
	window  time.Duration
	lastGC  time.Time
	nowFunc func() time.Time
}

type window struct {
	count int
	reset time.Time
}

func newLimiter(limit int, per time.Duration) *limiter {
	return &limiter{
		hits:    make(map[string]*window),
		limit:   limit,
		window:  per,
		nowFunc: time.Now,
	}
}

// Allow records an attempt and reports whether it is under the limit. The
// second return is how long to wait when it is not.
func (l *limiter) Allow(key string) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := l.nowFunc()
	l.gc(now)

	w, ok := l.hits[key]
	if !ok || now.After(w.reset) {
		l.hits[key] = &window{count: 1, reset: now.Add(l.window)}
		return true, 0
	}
	w.count++
	if w.count > l.limit {
		return false, w.reset.Sub(now)
	}
	return true, 0
}

// Reset clears a key, called after a successful login so a user who mistyped
// a few times is not still throttled.
func (l *limiter) Reset(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.hits, key)
}

// gc drops expired windows. Without it the map grows once per attacking IP
// and never shrinks.
func (l *limiter) gc(now time.Time) {
	if now.Sub(l.lastGC) < l.window {
		return
	}
	l.lastGC = now
	for k, w := range l.hits {
		if now.After(w.reset) {
			delete(l.hits, k)
		}
	}
}
