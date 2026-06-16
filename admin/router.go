package admin

import (
	"crypto/subtle"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
	"github.com/opencode-sw/gateway/config"
	"github.com/opencode-sw/gateway/pool"
	"github.com/opencode-sw/gateway/store"
)

// picker is set at startup via MountWithPicker.
var picker *pool.Picker

// MountWithPicker attaches the admin API with access to the KEY pool.
func MountWithPicker(rg *gin.RouterGroup, p *pool.Picker) {
	picker = p
	Mount(rg)
}

// Mount attaches the admin API under the given router group.
//
//	POST /admin/login                       {password}           -> {token}
//	GET  /admin/health                                                pool health
//	GET  /admin/keys                                                  -> []Key
//	POST /admin/keys                       {value,group,label}      -> Key
//	POST /admin/keys/:id/toggle                                      toggle
//	POST /admin/keys/:id/reset                                        reset cooldown
//	DELETE /admin/keys/:id
//	GET  /admin/tokens                                                -> []Token
//	POST /admin/tokens                      create
//	DELETE /admin/tokens/:id
//	GET  /admin/stats                                                   usage summary
//	GET  /admin/models                                                  model route table
//	POST /admin/models                      add/update route
//	DELETE /admin/models/:id
func Mount(rg *gin.RouterGroup) {
	rg.POST("/login", login)

	authed := rg.Group("")
	authed.Use(adminAuth())
	{
		authed.GET("/health", poolHealth)
		authed.GET("/keys", listKeys)
		authed.POST("/keys", createKey)
		authed.POST("/keys/:id/toggle", toggleKey)
		authed.POST("/keys/:id/reset", resetKeyCooldown)
		authed.DELETE("/keys/:id", deleteKey)

		authed.GET("/tokens", listTokens)
		authed.POST("/tokens", createTokenAdmin)
		authed.DELETE("/tokens/:id", deleteToken)

		authed.GET("/stats", stats)
		authed.GET("/models", listModelsAdmin)
		authed.POST("/models", upsertModel)
		authed.DELETE("/models/:id", deleteModel)
	}
}

// login exchanges the admin password for a short-lived JWT.
func login(c *gin.Context) {
	var body struct {
		Password string `json:"password"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
		return
	}
	if !subtleEqual(body.Password, config.Get().AdminPassword) {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid password"})
		return
	}
	claims := jwt.MapClaims{
		"role": "admin",
		"exp":  time.Now().Add(12 * time.Hour).Unix(),
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	signed, err := tok.SignedString([]byte(config.Get().JWTSecret))
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "sign jwt: " + err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"token": signed})
}

// adminAuth validates the admin JWT.
func adminAuth() gin.HandlerFunc {
	return func(c *gin.Context) {
		h := c.GetHeader("Authorization")
		if !strings.HasPrefix(strings.ToLower(h), "bearer ") {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "missing bearer token"})
			return
		}
		raw := strings.TrimSpace(h[7:])
		claims := jwt.MapClaims{}
		_, err := jwt.ParseWithClaims(raw, claims, func(t *jwt.Token) (any, error) {
			return []byte(config.Get().JWTSecret), nil
		})
		if err != nil {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "invalid token"})
			return
		}
		if role, _ := claims["role"].(string); role != "admin" {
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "not admin"})
			return
		}
		c.Next()
	}
}

// --- Pool Health ---

func poolHealth(c *gin.Context) {
	groups := []string{"go"}
	health := map[string]any{}
	for _, g := range groups {
		health[g] = picker.Stats(g)
	}
	c.JSON(http.StatusOK, gin.H{
		"status": "ok",
		"pools":  health,
	})
}

// --- Keys ---

func listKeys(c *gin.Context) {
	group := c.Query("group")
	if group == "" {
		group = "go"
	}
	keys, err := pool.AllByGroup(group)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	for i := range keys {
		keys[i].Value = maskSecret(keys[i].Value)
	}
	c.JSON(http.StatusOK, gin.H{"data": keys})
}

func createKey(c *gin.Context) {
	var body struct {
		Value    string `json:"value" binding:"required"`
		Group    string `json:"group" binding:"required"`
		Label    string `json:"label"`
		Weight   int    `json:"weight"`
		ProxyURL string `json:"proxy_url"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	w := body.Weight
	if w <= 0 {
		w = 1
	}
	group := strings.TrimSpace(body.Group)
	if group == "" {
		group = "go"
	}
	if group != "go" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "only go group is supported"})
		return
	}
	k := &store.Key{
		Value:    strings.TrimSpace(body.Value),
		Group:    group,
		Label:    body.Label,
		Enabled:  true,
		Weight:   w,
		ProxyURL: strings.TrimSpace(body.ProxyURL),
	}
	if err := store.DB().Create(k).Error; err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, k)
}

func toggleKey(c *gin.Context) {
	var k store.Key
	if err := store.DB().First(&k, c.Param("id")).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "not found"})
		return
	}
	k.Enabled = !k.Enabled
	store.DB().Model(&store.Key{}).Where("id = ?", k.ID).Update("enabled", k.Enabled)
	if !k.Enabled {
		// also clear cooldown when disabling
		store.DB().Model(&store.Key{}).Where("id = ?", k.ID).Updates(map[string]any{
			"fail_count":     0,
			"cooldown_until": nil,
		})
	}
	c.JSON(http.StatusOK, k)
}

func resetKeyCooldown(c *gin.Context) {
	id, _ := strconv.ParseUint(c.Param("id"), 10, 64)
	picker.ResetCooldown(uint(id))
	c.JSON(http.StatusOK, gin.H{"ok": true, "message": "cooldown cleared"})
}

func deleteKey(c *gin.Context) {
	store.DB().Delete(&store.Key{}, c.Param("id"))
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// --- Tokens ---

func listTokens(c *gin.Context) {
	ts, err := pool.AllTokens()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	for i := range ts {
		ts[i].Token = maskSecret(ts[i].Token)
	}
	c.JSON(http.StatusOK, gin.H{"data": ts})
}

func createTokenAdmin(c *gin.Context) {
	var body struct {
		Name          string     `json:"name"`
		AllowedGroups string     `json:"allowed_groups"`
		RateLimit     int        `json:"rate_limit"`
		ExpiresAt     *time.Time `json:"expires_at"`
	}
	_ = c.ShouldBindJSON(&body)
	allowedGroups := strings.TrimSpace(body.AllowedGroups)
	if allowedGroups != "" && allowedGroups != "go" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "only go group is supported"})
		return
	}
	t, err := pool.CreateToken(body.Name, allowedGroups, body.RateLimit, body.ExpiresAt)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, t)
}

func deleteToken(c *gin.Context) {
	store.DB().Delete(&store.Token{}, c.Param("id"))
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// --- Models ---

func listModelsAdmin(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"data": config.AllModels()})
}

func upsertModel(c *gin.Context) {
	var body config.ModelRoute
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if body.ID == "" || body.Protocol == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "id and protocol are required"})
		return
	}
	if body.Upstream == "" {
		body.Upstream = config.UpstreamGo
	}
	if body.Upstream != config.UpstreamGo {
		c.JSON(http.StatusBadRequest, gin.H{"error": "only go upstream is supported"})
		return
	}
	if body.Group == "" {
		body.Group = "go"
	}
	config.RegisterModel(body)
	// Persist to DB.
	store.SaveModelRoute(&store.ModelRouteRow{
		ID: body.ID, Name: body.Name, Upstream: string(body.Upstream),
		Protocol: string(body.Protocol), RealModel: body.RealModel,
		Group: body.Group, ContextLen: body.ContextLen,
	})
	c.JSON(http.StatusOK, body)
}

func deleteModel(c *gin.Context) {
	id := c.Param("id")
	config.RemoveModel(id)
	store.DeleteModelRoute(id)
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// --- Stats ---

func stats(c *gin.Context) {
	var totalCalls int64
	store.DB().Model(&store.UsageLog{}).Count(&totalCalls)

	type bucket struct {
		StatusCode int   `json:"status_code"`
		Cnt        int64 `json:"count"`
	}
	var byStatus []bucket
	store.DB().Model(&store.UsageLog{}).
		Select("status_code, count(*) as count").
		Group("status_code").
		Scan(&byStatus)

	var byModel []struct {
		Model string `json:"model"`
		Cnt   int64  `json:"count"`
	}
	store.DB().Model(&store.UsageLog{}).
		Select("model, count(*) as count").
		Group("model").
		Order("count desc").
		Scan(&byModel)

	var byProtocol []struct {
		Protocol string `json:"protocol"`
		Cnt      int64  `json:"count"`
	}
	store.DB().Model(&store.UsageLog{}).
		Select("protocol, count(*) as count").
		Group("protocol").
		Scan(&byProtocol)

	var avgDuration float64
	store.DB().Model(&store.UsageLog{}).Select("coalesce(avg(duration_ms),0)").Scan(&avgDuration)

	var keys int64
	store.DB().Model(&store.Key{}).Count(&keys)
	var tokens int64
	store.DB().Model(&store.Token{}).Count(&tokens)

	// Recent logs
	var recent []store.UsageLog
	store.DB().Order("id desc").Limit(50).Find(&recent)

	c.JSON(http.StatusOK, gin.H{
		"total_calls":     totalCalls,
		"by_status":       byStatus,
		"by_model":        byModel,
		"by_protocol":     byProtocol,
		"avg_duration_ms": avgDuration,
		"keys":            keys,
		"tokens":          tokens,
		"recent":          recent,
	})
}

// --- helpers ---

func maskSecret(s string) string {
	if len(s) <= 8 {
		return "****"
	}
	return s[:4] + "..." + s[len(s)-4:]
}

// subtleEqual is a constant-time string compare for the admin password.
func subtleEqual(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}
