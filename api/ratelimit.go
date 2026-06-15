package api

import (
	"net/http"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/opencode-sw/gateway/store"
)

// rateLimiter is a per-token sliding-window rate limiter.
// Each token is allowed `RateLimit` requests per minute (0 = unlimited).
type rateLimiter struct {
	mu      sync.Mutex
	windows map[uint]*window // token ID -> sliding window
}

type window struct {
timestamps []time.Time
limit      int
}

func newRateLimiter() *rateLimiter {
	return &rateLimiter{
		windows: make(map[uint]*window),
	}
}

// allow reports whether the token is within its rate limit.
func (rl *rateLimiter) allow(tok *store.Token) bool {
	if tok.RateLimit <= 0 {
		return true // unlimited
	}
	rl.mu.Lock()
	defer rl.mu.Unlock()

	w, ok := rl.windows[tok.ID]
	if !ok {
		w = &window{limit: tok.RateLimit}
		rl.windows[tok.ID] = w
	}

	now := time.Now()
	cutoff := now.Add(-time.Minute)

	// Purge expired timestamps.
	valid := w.timestamps[:0]
	for _, t := range w.timestamps {
		if t.After(cutoff) {
			valid = append(valid, t)
		}
	}
	w.timestamps = valid

	if len(w.timestamps) >= w.limit {
		return false
	}
	w.timestamps = append(w.timestamps, now)
	return true
}

// RateLimitMiddleware returns a Gin middleware that enforces per-token rate limits.
func RateLimitMiddleware() gin.HandlerFunc {
	rl := newRateLimiter()
	return func(c *gin.Context) {
		tokAny, exists := c.Get("token")
		if !exists {
			c.Next()
			return
		}
		tok, ok := tokAny.(*store.Token)
		if !ok || tok == nil {
			c.Next()
			return
		}
		if !rl.allow(tok) {
			c.AbortWithStatusJSON(http.StatusTooManyRequests, gin.H{
				"error": gin.H{
					"type":    "rate_limit_exceeded",
					"message": "rate limit exceeded for this token",
				},
			})
			return
		}
		c.Next()
	}
}
