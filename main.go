package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/xb0or/opencode-GO/admin"
	"github.com/xb0or/opencode-GO/api"
	"github.com/xb0or/opencode-GO/config"
	"github.com/xb0or/opencode-GO/modelsync"
	"github.com/xb0or/opencode-GO/pool"
	"github.com/xb0or/opencode-GO/store"
	"github.com/xb0or/opencode-GO/upstream"
	"github.com/xb0or/opencode-GO/web"
)

func main() {
	cfg := config.Load()

	// Production security check: refuse to start with insecure defaults.
	if err := config.ValidateSecurity(); err != nil {
		log.Fatalf("SECURITY CHECK FAILED: %v\nSet ADMIN_PASSWORD and JWT_SECRET environment variables to secure values.", err)
	}

	// Open DB and ensure a bootstrap gateway token exists for first-time use.
	if err := store.Init(); err != nil {
		log.Fatalf("store init failed: %v", err)
	}
	ensureBootstrapToken()

	// Load model routes from DB; seed defaults if DB is empty.
	loadModelRoutes()
	loadModelMappings()
	if result, err := modelsync.Sync(context.Background(), modelsync.Options{}); err != nil {
		log.Printf("warn: model catalog sync failed: %v", err)
	} else {
		log.Printf("synced model catalog: opencode=%d ollama=%d openrouter=%d matched=%d created=%d updated=%d warnings=%v",
			result.OpenCodeCount, result.OllamaCount, result.OpenRouterCount, result.MatchedCount, result.CreatedCount, result.UpdatedCount, result.Warnings)
	}
	modelsync.StartBackground(context.Background(), 6*time.Hour, modelsync.Options{}, func(result modelsync.Result, err error) {
		if err != nil {
			log.Printf("warn: background model catalog sync failed: %v", err)
			return
		}
		log.Printf("background model catalog synced: opencode=%d ollama=%d matched=%d created=%d updated=%d warnings=%v",
			result.OpenCodeCount, result.OllamaCount, result.MatchedCount, result.CreatedCount, result.UpdatedCount, result.Warnings)
	})

	// Release mode in production.
	if os.Getenv("GIN_MODE") == "" {
		gin.SetMode(gin.ReleaseMode)
	}

	picker := pool.NewPicker()
	root := api.NewRouter(picker)

	// Mount admin panel under /admin.
	admin.MountWithPicker(root.Group("/admin"), picker)

	// Serve the admin SPA at /admin (the HTML page).
	// NoRoute fallback serves admin.html for any unknown /admin/* sub-path,
	// both for static files (css, js) and SPA client-side routing.
	root.GET("/admin", gin.WrapH(web.AdminHandler()))
	root.NoRoute(func(c *gin.Context) {
		if strings.HasPrefix(c.Request.URL.Path, "/admin") {
			web.AdminHandler().ServeHTTP(c.Writer, c.Request)
			return
		}
		c.Writer.WriteHeader(http.StatusNotFound)
	})

	addr := ":" + cfg.Port
	log.Printf("opencode-go listening on %s", addr)
	log.Printf("  go  base url : %s", cfg.GoBaseURL)
	log.Printf("  ollama cloud: %s", cfg.OllamaBaseURL)
	log.Printf("  models       : %d registered", len(config.AllModels()))
	log.Printf("  mappings     : %d configured", len(config.AllModelMappings()))

	// Use an explicit http.Server with sensible timeouts and graceful shutdown.
	srv := &http.Server{
		Addr:              addr,
		Handler:           root,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    1 << 20, // 1 MiB header limit
		// WriteTimeout is intentionally NOT set: SSE streams can run for minutes.
		// Per-request timeouts are handled in upstreamRequestContext.
	}

	// Start server in a goroutine so we can handle shutdown signals.
	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("server failed: %v", err)
		}
	}()

	// Graceful shutdown on SIGINT/SIGTERM.
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit
	log.Printf("shutting down server...")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		log.Printf("server forced shutdown: %v", err)
	}
	upstream.CloseIdleConnections()
	log.Printf("server exited")
}

// loadModelMappings loads persisted model rewrite rules, then overlays optional
// env/file rules so runtime config can override DB-managed defaults.
func loadModelMappings() {
	rows, err := store.LoadModelMappings()
	if err != nil {
		log.Printf("warn: cannot load model mappings: %v", err)
		config.LoadModelMappings()
		return
	}
	mappings := map[string]string{}
	for _, r := range rows {
		mappings[r.SourceModel] = r.TargetModel
	}
	if len(mappings) > 0 {
		config.RegisterModelMappings(mappings)
	}
	config.LoadModelMappings()
}

// loadModelRoutes loads model routes from DB. If the DB is empty, it waits for
// the first modelsync.Sync() call (triggered immediately in main) to populate
// the catalog from upstream APIs. No hardcoded seed models are used.
func loadModelRoutes() {
	rows, err := store.LoadModelRoutes()
	if err != nil {
		log.Printf("warn: cannot load model routes: %v", err)
		return
	}
	if len(rows) == 0 {
		// First run: no seed models; modelsync.Sync() will populate from APIs.
		log.Printf("model routes DB is empty; waiting for first sync to populate from upstream APIs")
		return
	}
	// Load from DB into config.
	var routes []config.ModelRoute
	for _, r := range rows {
		if r.Upstream != string(config.UpstreamGo) && r.Upstream != string(config.UpstreamOllama) {
			continue
		}
		if r.Group != "go" && r.Group != "ollama" {
			continue
		}
		routes = append(routes, store.ModelRouteFromRow(r))
	}
	config.ReplaceModels(routes)
	log.Printf("loaded %d model routes from DB", len(rows))
}

// ensureBootstrapToken creates a default gateway token on first run and prints
// it to stdout so the operator can use it immediately.
func ensureBootstrapToken() {
	ts, err := pool.AllTokens()
	if err != nil {
		log.Printf("warn: cannot load tokens: %v", err)
		return
	}
	if len(ts) > 0 {
		return
	}
	t, err := pool.CreateToken("bootstrap", "", 0, nil)
	if err != nil {
		log.Printf("warn: cannot create bootstrap token: %v", err)
		return
	}
	fmt.Println("==================================================")
	fmt.Println(" Bootstrap gateway token created:")
	fmt.Printf("   %s\n", t.Token)
	fmt.Println(" Use this as the api key in opencode / clients.")
	fmt.Println("==================================================")
}
