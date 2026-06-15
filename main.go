package main

import (
	"fmt"
	"log"
	"os"

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

	// Release mode in production.
	if os.Getenv("GIN_MODE") == "" {
		gin.SetMode(gin.ReleaseMode)
	}

	picker := pool.NewPicker()
	root := api.NewRouter(picker)

	// Mount admin panel under /admin.
	admin.MountWithPicker(root.Group("/admin"), picker)

	// Serve the admin SPA at /admin (the HTML page).
	root.GET("/admin", gin.WrapH(web.AdminHandler()))

	addr := ":" + cfg.Port
	log.Printf("opencode-sw listening on %s", addr)
	log.Printf("  zen base url : %s", cfg.ZenBaseURL)
	log.Printf("  go  base url : %s", cfg.GoBaseURL)
	log.Printf("  models       : %d registered", len(config.AllModels()))
	if err := root.Run(addr); err != nil {
		log.Fatalf("server failed: %v", err)
	}
}

// loadModelRoutes loads model routes from DB, seeding defaults if empty.
func loadModelRoutes() {
	rows, err := store.LoadModelRoutes()
	if err != nil {
		log.Printf("warn: cannot load model routes: %v", err)
		config.RegisterModels(config.DefaultModels())
		return
	}
	if len(rows) == 0 {
		// First run: seed defaults into DB.
		defaults := config.DefaultModels()
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
	var routes []config.ModelRoute
	for _, r := range rows {
		routes = append(routes, config.ModelRoute{
			ID: r.ID, Name: r.Name, Upstream: config.Upstream(r.Upstream),
			Protocol: config.Protocol(r.Protocol), RealModel: r.RealModel,
			Group: r.Group, ContextLen: r.ContextLen,
		})
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
