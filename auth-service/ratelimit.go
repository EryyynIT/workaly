package main

import (
	"sync"
	"time"
)

// rateLimiter is a per-process sliding-window limiter. It protects a single
// auth-service instance; coordinated limits across replicas require a shared
// store (deferred until Redis or an equivalent is introduced with a concrete
// task). Keys expire after the window to bound memory under floods of random
// identifiers.
type rateLimiter struct {
	mu       sync.Mutex
	attempts map[string][]time.Time
}

func newRateLimiter() *rateLimiter {
	return &rateLimiter{attempts: make(map[string][]time.Time)}
}

// Allow reports whether the action is permitted and records the attempt.
func (r *rateLimiter) Allow(key string, max int, window time.Duration) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := time.Now()
	cutoff := now.Add(-window)

	entries := r.attempts[key]
	kept := entries[:0]
	for _, t := range entries {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	if len(kept) >= max {
		r.attempts[key] = kept
		return false
	}
	r.attempts[key] = append(kept, now)

	// Periodic sweep so keys of one-off visitors do not live forever.
	if len(r.attempts) > 4096 {
		for k, v := range r.attempts {
			if len(v) == 0 || v[len(v)-1].Before(cutoff) {
				delete(r.attempts, k)
			}
		}
	}
	return true
}

// Reset clears all state. Used in tests.
func (r *rateLimiter) Reset() {
	r.mu.Lock()
	r.attempts = make(map[string][]time.Time)
	r.mu.Unlock()
}
