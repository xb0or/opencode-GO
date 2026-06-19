package api

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/opencode-sw/gateway/pool"
	"github.com/opencode-sw/gateway/store"
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

// RequestLimitMiddleware enforces the per-token total request cap (MaxRequests).
// Must run after Auth() so the token is available in context.
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
		if tok.MaxRequests > 0 && tok.RequestsUsed >= tok.MaxRequests {
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
