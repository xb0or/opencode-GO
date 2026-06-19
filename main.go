package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/opencode-sw/gateway/admin"
	"github.com/opencode-sw/gateway/api"
	"github.com/opencode-sw/gateway/config"
	"github.com/opencode-sw/gateway/pool"
	"github.com/opencode-sw/gateway/store"
	"github.com/opencode-sw/gateway/web"
)

func main() {
	cfg := config.Load()

	// Open DB and ensure a bootstrap gateway token exists for first-time use.
	if err := store.Init(); err != nil {
		log.Fatalf("store init failed: %v", err)
	}
	ensureBootstrapToken()

	// Load model routes from DB; seed defaults if DB is empty.
	loadModelRoutes()
	loadModelMappings()
	config.EnrichModelsFromOpenRouter()

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
	log.Printf("opencode-sw listening on %s", addr)
	log.Printf("  go  base url : %s", cfg.GoBaseURL)
	log.Printf("  models       : %d registered", len(config.AllModels()))
	log.Printf("  mappings     : %d configured", len(config.AllModelMappings()))
	if err := root.Run(addr); err != nil {
		log.Fatalf("server failed: %v", err)
	}
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

// loadModelRoutes loads model routes from DB, seeding defaults if empty.
func loadModelRoutes() {
	rows, err := store.LoadModelRoutes()
	defaults := config.DefaultModels()
	if err != nil {
		log.Printf("warn: cannot load model routes: %v", err)
		config.RegisterModels(defaults)
		return
	}
	if len(rows) == 0 {
		// First run: seed defaults into DB.
		for _, m := range defaults {
			store.SaveModelRoute(&store.ModelRouteRow{
			ID: m.ID, Name: m.Name, Upstream: string(m.Upstream),
			Protocol: string(m.Protocol), RealModel: m.RealModel,
			Group: m.Group, ContextLen: m.ContextLen,
		})
		}
		config.RegisterModels(defaults)
		log.Printf("seeded %d default model routes into DB", len(defaults))
		return
	}
	// Load from DB into config.
	allowed := map[string]bool{}
	for _, m := range defaults {
		allowed[m.ID] = true
	}
	var routes []config.ModelRoute
	for _, r := range rows {
		if r.Upstream != string(config.UpstreamGo) || r.Group != "go" || !allowed[r.ID] {
			continue
		}
		routes = append(routes, config.ModelRoute{
			ID: r.ID, Name: r.Name, Upstream: config.Upstream(r.Upstream),
			Protocol: config.Protocol(r.Protocol), RealModel: r.RealModel,
			Group: r.Group, ContextLen: r.ContextLen,
		})
	}
	if len(routes) == 0 {
		defaults := config.DefaultModels()
		for _, m := range defaults {
			store.SaveModelRoute(&store.ModelRouteRow{
				ID: m.ID, Name: m.Name, Upstream: string(m.Upstream),
				Protocol: string(m.Protocol), RealModel: m.RealModel,
				Group: m.Group, ContextLen: m.ContextLen,
			})
		}
		config.RegisterModels(defaults)
		log.Printf("seeded %d default Go model routes into DB", len(defaults))
		return
	}
	config.RegisterModels(routes)
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

// encodeCaps serializes a string slice to JSON for DB storage.
func encodeCaps(caps []string) string {
	if len(caps) == 0 {
		return ""
	}
	b, _ := json.Marshal(caps)
	return string(b)
}

// decodeCaps deserializes capabilities from DB.
func decodeCaps(s string) []string {
	if s == "" {
		return nil
	}
	var caps []string
	json.Unmarshal([]byte(s), &caps)
	return caps
}
