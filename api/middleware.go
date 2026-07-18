package api

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/xb0or/opencode-GO/pool"
	"github.com/xb0or/opencode-GO/store"
)

// extractToken pulls the bearer token from Authorization header (preferred) or
// the `x-api-key` header / `api_key` query param (Anthropic-style fallbacks).
func extractToken(c *gin.Context) string {
	if h := c.GetHeader("Authorization"); h != "" {
		if strings.HasPrefix(strings.ToLower(h), "bearer ") {
			return strings.TrimSpace(h[7:])
		}
		return strings.TrimSpace(h)
	}
	if k := c.GetHeader("x-api-key"); k != "" {
		return strings.TrimSpace(k)
	}
	if q := c.Query("api_key"); q != "" {
		return strings.TrimSpace(q)
	}
	return ""
}

// Auth middleware validates a gateway token and stashes the *store.Token in
// the gin context as "token". Bypassed only for /health and /v1/models (models
// is public so clients can discover the catalog).
func Auth() gin.HandlerFunc {
	return func(c *gin.Context) {
		val := extractToken(c)
		if val == "" {
			abortAuth(c, "missing api key")
			return
		}
		t := pool.FindToken(val)
		if t == nil || !t.Enabled {
			abortAuth(c, "invalid api key")
			return
		}
		if pool.IsExpired(t) {
			abortAuth(c, "api key expired")
			return
		}
		c.Set("token", t)
		c.Next()
	}
}

// RequireGroup ensures the authenticated token may use the group the chosen
// model belongs to. Must run after a handler has set "group" in the context.
func RequireGroup() gin.HandlerFunc {
	return func(c *gin.Context) {
		t, _ := c.Get("token")
		tok, _ := t.(*store.Token)
		group, _ := c.Get("group")
		g, _ := group.(string)
		if tok != nil && !pool.GroupAllowed(tok, g) {
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{
				"error": gin.H{
					"type":    "permission_denied",
					"message": "this token is not allowed to use group: " + g,
				},
			})
			return
		}
		c.Next()
	}
}

// RequestLimitMiddleware enforces the per-token total request cap (MaxRequests)
// using atomic pre-reservation. Must run after Auth() so the token is
// available in context.
//
// Instead of reading a snapshot of RequestsUsed and incrementing only on
// success (which allows N concurrent requests to all pass the check when
// only 1 slot remains), this middleware atomically increments requests_used
// with a conditional UPDATE that checks the limit in the same statement.
// If the request fails and should not count, incrementRequestsUsed is
// NOT called — but the reservation was already made at entry. A rollback
// mechanism (ReleaseRequest) can be used if the request fails early.
func RequestLimitMiddleware() gin.HandlerFunc {
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
		// Unlimited tokens (MaxRequests <= 0) skip the DB write entirely,
		// avoiding a write transaction on every request.
		if tok.MaxRequests <= 0 {
			c.Next()
			return
		}
		// Atomic pre-reserve: increment + limit-check in one SQL statement.
		// This prevents concurrent requests from all passing the check
		// when only one slot remains.
		reserved, err := store.TryReserveRequest(tok.ID)
		if err != nil {
			// Database error — return 503, NOT 403. A SQLite busy/error
			// must not masquerade as "quota exhausted".
			c.AbortWithStatusJSON(http.StatusServiceUnavailable, gin.H{
				"error": gin.H{
					"type":    "service_unavailable",
					"message": "request limit service temporarily unavailable",
				},
			})
			return
		}
		if !reserved {
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{
				"error": gin.H{
					"type":    "request_limit_exceeded",
					"message": "this token has reached its total request limit",
				},
			})
			return
		}
		c.Next()
	}
}

func abortAuth(c *gin.Context, msg string) {
	c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{
		"error": gin.H{
			"type":    "authentication_error",
			"message": msg,
		},
	})
}
