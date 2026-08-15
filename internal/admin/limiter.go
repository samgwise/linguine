package admin

import (
	"sync"
	"time"
)

// loginLimiter is a single-process, per-IP fixed-window rate limiter for the
// admin login endpoint. It bounds argon2id CPU burn from prefix-matching
// password guesses.
//
// NOTE: this only works for a single router instance. A multi-router
// deployment behind a load balancer would need a shared store (Redis et al.)
// to throttle consistently across nodes; that is a later-phase concern.
type loginLimiter struct {
	mu     sync.Mutex
	window time.Duration
	limit  int
	hits   map[string]*ipBucket
}

type ipBucket struct {
	count   int
	resetAt time.Time
}

func newLoginLimiter(limit int, window time.Duration) *loginLimiter {
	return &loginLimiter{window: window, limit: limit, hits: make(map[string]*ipBucket)}
}

// Allow reports whether a request from ip is within the window's limit,
// consuming a slot either way. A request past the limit is refused but still
// counts toward the window so a burst of attempts does not reset the clock.
func (l *loginLimiter) Allow(ip string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	b, ok := l.hits[ip]
	if !ok || now.After(b.resetAt) {
		b = &ipBucket{count: 0, resetAt: now.Add(l.window)}
		l.hits[ip] = b
	}
	b.count++
	return b.count <= l.limit
}
