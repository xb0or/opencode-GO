package api

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/opencode-sw/gateway/pool"
)

// NewRouter builds the public API router: health + the four universal endpoints.
//
//   GET  /health
//   GET  /v1/models                 (public)
//   POST /v1/chat/completions       (auth)
//   POST /v1/messages               (auth)
//   POST /v1/responses              (auth)
func NewRouter(p *pool.Picker) *gin.Engine {
	r := gin.New()
	r.Use(gin.Recovery())
	r.Use(requestLogger())

	r.GET("/health", health)
	r.GET("/", health)

	v1 := r.Group("/v1")
	v1.GET("/models", listModels)

	rateLimit := RateLimitMiddleware()

	auth := v1.Group("", Auth(), rateLimit)
	{
		auth.POST("/chat/completions", proxyChat(p))
		auth.POST("/messages", proxyMessages(p))
		auth.POST("/responses", proxyResponses(p))
	}

	// OpenCode clients sometimes hit the bare endpoints without /v1; mirror them.
	auth2 := r.Group("", Auth(), rateLimit)
	{
		auth2.POST("/chat/completions", proxyChat(p))
		auth2.POST("/messages", proxyMessages(p))
		auth2.POST("/responses", proxyResponses(p))
	}
	r.GET("/models", listModels)

	return r
}

// health is a simple liveness probe.
func health(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{
		"status":  "ok",
		"service": "opencode-sw",
	})
}

// requestLogger logs one line per request at the access level.
func requestLogger() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Next()
	}
}
