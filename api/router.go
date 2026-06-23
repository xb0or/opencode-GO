package api

import (
	"log"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/xb0or/opencode-GO/pool"
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
	r.Use(corsMiddleware())

	r.GET("/health", health)
	r.GET("/", health)

	v1 := r.Group("/v1")
	v1.GET("/models", listModels)

	rateLimit := RateLimitMiddleware()
	reqLimit := RequestLimitMiddleware()

	auth := v1.Group("", Auth(), rateLimit, reqLimit)
	registerProxyRoutes(auth, p)

	// OpenCode clients sometimes hit the bare endpoints without /v1; mirror them.
	auth2 := r.Group("", Auth(), rateLimit, reqLimit)
	registerProxyRoutes(auth2, p)
	r.GET("/models", listModels)

	return r
}

// health is a simple liveness probe.
func health(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{
		"status":  "ok",
		"service": "opencode-go",
	})
}

// registerProxyRoutes registers the three proxy endpoints on a router group.
func registerProxyRoutes(rg *gin.RouterGroup, p *pool.Picker) {
	rg.POST("/chat/completions", proxyChat(p))
	rg.POST("/messages", proxyMessages(p))
	rg.POST("/responses", proxyResponses(p))
}

// requestLogger logs one line per request at the access level.
func requestLogger() gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()
		c.Next()
		latency := time.Since(start)
		log.Printf("[%s] %s %s %d %v",
			c.Request.Method, c.Request.URL.Path,
			c.ClientIP(), c.Writer.Status(), latency,
		)
	}
}

// corsMiddleware allows cross-origin requests from any origin.
func corsMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Header("Access-Control-Allow-Origin", "*")
		c.Header("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
		c.Header("Access-Control-Allow-Headers", "Authorization, Content-Type, X-Api-Key")
		c.Header("Access-Control-Max-Age", "86400")

		if c.Request.Method == http.MethodOptions {
			c.AbortWithStatus(http.StatusNoContent)
			return
		}
		c.Next()
	}
}
