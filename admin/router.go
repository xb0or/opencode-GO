package admin

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
	"github.com/xb0or/opencode-GO/config"
	"github.com/xb0or/opencode-GO/modelsync"
	"github.com/xb0or/opencode-GO/pool"
	"github.com/xb0or/opencode-GO/store"
	"github.com/xb0or/opencode-GO/version"
	"gorm.io/gorm"
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
//	POST /admin/keys                       {value,label}            -> Key
//	POST /admin/keys/import-github          import via GitHub login
//	PATCH /admin/keys/:id                  update key settings
//	POST /admin/keys/:id/toggle                                      toggle
//	POST /admin/keys/:id/reset                                        reset cooldown
//	DELETE /admin/keys/:id
//	GET  /admin/tokens                                                -> []Token
//	POST /admin/tokens                      create
//	DELETE /admin/tokens/:id
//	GET  /admin/stats                                                   usage summary
//	GET  /admin/usage                                                   paginated usage logs
//	GET  /admin/models                                                  model route table
//	POST /admin/models/sync                                             sync model catalog
//	POST /admin/models                      add/update route
//	PATCH /admin/models/:id                 edit route
//	POST /admin/models/:id/toggle                                      enable/disable route
//	DELETE /admin/models/:id
//	GET  /admin/model-mappings                                          model rewrite rules
//	POST /admin/model-mappings              add/update rule
//	DELETE /admin/model-mappings/:source    delete rule
func Mount(rg *gin.RouterGroup) {
	rg.POST("/login", login)

	authed := rg.Group("")
	authed.Use(adminAuth())
	{
		authed.GET("/health", poolHealth)
		authed.GET("/version", versionInfo)
		authed.GET("/keys", listKeys)
		authed.POST("/keys", createKey)
		authed.POST("/keys/import-github", importGithubKeys)
		authed.PATCH("/keys/:id", updateKey)
		authed.POST("/keys/:id/toggle", toggleKey)
		authed.POST("/keys/:id/reset", resetKeyCooldown)
		authed.GET("/keys/:id/quota", fetchKeyQuota)
		authed.DELETE("/keys/:id", deleteKey)

		authed.GET("/tokens", listTokens)
		authed.POST("/tokens", createTokenAdmin)
		authed.PATCH("/tokens/:id", updateToken)
		authed.DELETE("/tokens/:id", deleteToken)

		authed.GET("/stats", stats)
		authed.GET("/usage", listUsageLogs)
		authed.GET("/models", listModelsAdmin)
		authed.POST("/models/sync", syncModels)
		authed.POST("/models", upsertModel)
		authed.PATCH("/models/:id", updateModel)
		authed.POST("/models/:id/toggle", toggleModel)
		authed.DELETE("/models/:id", deleteModel)
		authed.GET("/model-mappings", listModelMappingsAdmin)
		authed.POST("/model-mappings", upsertModelMapping)
		authed.DELETE("/model-mappings/*source", deleteModelMapping)
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
	groups := []string{"go", "ollama"}
	health := map[string]any{}
	for _, g := range groups {
		health[g] = picker.Stats(g)
	}
	c.JSON(http.StatusOK, gin.H{
		"status": "ok",
		"pools":  health,
	})
}

// --- Version ---

// versionInfo reports the running build version and, best-effort, the latest
// published GitHub release so the admin panel can flag available updates.
func versionInfo(c *gin.Context) {
	resp := gin.H{
		"version":    version.Version,
		"repo":       version.Repo,
		"github_url": version.GitHubURL(),
	}
	latest, err := version.FetchLatestRelease(c.Request.Context())
	if err == nil && latest.Tag != "" {
		resp["latest"] = gin.H{
			"tag":              latest.Tag,
			"name":             latest.Name,
			"html_url":         latest.HTMLURL,
			"published_at":     latest.PublishedAt,
			"update_available": version.Compare(version.Version, latest.Tag) < 0,
		}
	} else {
		resp["latest"] = nil
		resp["update_available"] = false
	}
	c.JSON(http.StatusOK, resp)
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
	items := make([]keyDTO, 0, len(keys))
	for _, k := range keys {
		items = append(items, decorateKey(k))
	}
	c.JSON(http.StatusOK, gin.H{"data": items})
}

type keyDTO struct {
	store.Key
	LastQuota any `json:"last_quota,omitempty"`
}

func decorateKey(k store.Key) keyDTO {
	k.Value = maskSecret(k.Value)
	dto := keyDTO{Key: k}
	if strings.TrimSpace(k.QuotaSnapshot) == "" {
		return dto
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(k.QuotaSnapshot), &payload); err == nil && len(payload) > 0 {
		dto.LastQuota = payload
	}
	return dto
}

func createKey(c *gin.Context) {
	var body struct {
		Value       string `json:"value" binding:"required"`
		Group       string `json:"group"`
		Label       string `json:"label"`
		Weight      int    `json:"weight"`
		ProxyURL    string `json:"proxy_url"`
		Cookie      string `json:"cookie"`
		WorkspaceID string `json:"workspace_id"`
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
	if group != "go" && group != "ollama" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "only go and ollama groups are supported"})
		return
	}
	k := &store.Key{
		Value:       strings.TrimSpace(body.Value),
		Group:       group,
		Label:       body.Label,
		Enabled:     true,
		Weight:      w,
		ProxyURL:    strings.TrimSpace(body.ProxyURL),
		Cookie:      normalizeAuthCookie(body.Cookie),
		WorkspaceID: normalizeWorkspaceID(body.WorkspaceID),
	}
	if err := store.DB().Create(k).Error; err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, k)
}

func updateKey(c *gin.Context) {
	var k store.Key
	if err := store.DB().First(&k, c.Param("id")).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "not found"})
		return
	}

	var body struct {
		Value       string `json:"value"`
		Label       string `json:"label"`
		Weight      *int   `json:"weight"`
		ProxyURL    string `json:"proxy_url"`
		Enabled     *bool  `json:"enabled"`
		Cookie      string `json:"cookie"`
		WorkspaceID string `json:"workspace_id"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	updates := map[string]any{
		"label":     body.Label,
		"proxy_url": strings.TrimSpace(body.ProxyURL),
	}
	oldCookie := normalizeAuthCookie(k.Cookie)
	oldWorkspaceID := normalizeWorkspaceID(k.WorkspaceID)
	nextCookie := oldCookie
	if cookie := normalizeAuthCookie(body.Cookie); cookie != "" {
		updates["cookie"] = cookie
		nextCookie = cookie
	}
	nextWorkspaceID := normalizeWorkspaceID(body.WorkspaceID)
	updates["workspace_id"] = nextWorkspaceID
	if nextCookie != oldCookie || nextWorkspaceID != oldWorkspaceID {
		updates["quota_snapshot"] = ""
		updates["quota_updated_at"] = nil
	}
	if body.Weight != nil {
		weight := *body.Weight
		if weight <= 0 {
			weight = 1
		}
		updates["weight"] = weight
	}
	if value := strings.TrimSpace(body.Value); value != "" {
		updates["value"] = value
	}
	if body.Enabled != nil {
		updates["enabled"] = *body.Enabled
	}
	if err := store.DB().Model(&store.Key{}).Where("id = ?", k.ID).Updates(updates).Error; err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	store.DB().First(&k, k.ID)
	k.Value = maskSecret(k.Value)
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
	c.JSON(http.StatusOK, gin.H{"data": decorateTokens(ts)})
}

type tokenDTO struct {
	store.Token
	TotalRequests     int64 `json:"total_requests"`
	TotalInputTokens  int64 `json:"total_input_tokens"`
	TotalOutputTokens int64 `json:"total_output_tokens"`
	TotalTokens       int64 `json:"total_tokens"`
	TodayRequests     int64 `json:"today_requests"`
	TodayTokens       int64 `json:"today_tokens"`
	LastHourRequests  int64 `json:"last_hour_requests"`
	LastHourTokens    int64 `json:"last_hour_tokens"`
}

type tokenUsageAggregate struct {
	TokenID      uint  `gorm:"column:token_id"`
	Requests     int64 `gorm:"column:requests"`
	InputTokens  int64 `gorm:"column:input_tokens"`
	OutputTokens int64 `gorm:"column:output_tokens"`
	TotalTokens  int64 `gorm:"column:total_tokens"`
}

func decorateTokens(tokens []store.Token) []tokenDTO {
	items := make([]tokenDTO, 0, len(tokens))
	if len(tokens) == 0 {
		return items
	}
	ids := make([]uint, 0, len(tokens))
	for _, tk := range tokens {
		ids = append(ids, tk.ID)
	}
	now := time.Now()
	todayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	lastHour := now.Add(-time.Hour)

	total := tokenUsageAggregates(ids, nil)
	today := tokenUsageAggregates(ids, &todayStart)
	lastHourStats := tokenUsageAggregates(ids, &lastHour)

	for _, tk := range tokens {
		dto := tokenDTO{Token: tk}
		if s, ok := total[tk.ID]; ok {
			dto.TotalRequests = s.Requests
			dto.TotalInputTokens = s.InputTokens
			dto.TotalOutputTokens = s.OutputTokens
			dto.TotalTokens = s.TotalTokens
		}
		if s, ok := today[tk.ID]; ok {
			dto.TodayRequests = s.Requests
			dto.TodayTokens = s.TotalTokens
		}
		if s, ok := lastHourStats[tk.ID]; ok {
			dto.LastHourRequests = s.Requests
			dto.LastHourTokens = s.TotalTokens
		}
		items = append(items, dto)
	}
	return items
}

func tokenUsageAggregates(tokenIDs []uint, since *time.Time) map[uint]tokenUsageAggregate {
	out := map[uint]tokenUsageAggregate{}
	if len(tokenIDs) == 0 {
		return out
	}
	q := store.DB().Model(&store.UsageLog{}).Where("token_id IN ?", tokenIDs)
	if since != nil {
		q = q.Where("created_at >= ?", *since)
	}
	var rows []tokenUsageAggregate
	q.Select("token_id, count(*) as requests, coalesce(sum(input_tokens),0) as input_tokens, coalesce(sum(output_tokens),0) as output_tokens, coalesce(sum(total_tokens),0) as total_tokens").
		Group("token_id").
		Scan(&rows)
	for _, row := range rows {
		out[row.TokenID] = row
	}
	return out
}

func createTokenAdmin(c *gin.Context) {
	var body struct {
		Name          string     `json:"name"`
		Description   string     `json:"description"`
		AllowedGroups string     `json:"allowed_groups"`
		RateLimit     int        `json:"rate_limit"`
		MaxRequests   int        `json:"max_requests"`
		ExpiresAt     *time.Time `json:"expires_at"`
	}
	_ = c.ShouldBindJSON(&body)
	allowedGroups := strings.TrimSpace(body.AllowedGroups)
	if allowedGroups != "" && allowedGroups != "go" && allowedGroups != "ollama" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "only go and ollama groups are supported"})
		return
	}
	var opts []pool.TokenOption
	if body.MaxRequests > 0 {
		opts = append(opts, pool.WithMaxRequests(body.MaxRequests))
	}
	if body.Description != "" {
		opts = append(opts, pool.WithDescription(body.Description))
	}
	t, err := pool.CreateToken(body.Name, allowedGroups, body.RateLimit, body.ExpiresAt, opts...)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, t)
}

func updateToken(c *gin.Context) {
	var tk store.Token
	if err := store.DB().First(&tk, c.Param("id")).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "not found"})
		return
	}

	var body struct {
		Name        *string    `json:"name"`
		Description *string    `json:"description"`
		RateLimit   *int       `json:"rate_limit"`
		MaxRequests *int       `json:"max_requests"`
		Enabled     *bool      `json:"enabled"`
		ExpiresAt   *time.Time `json:"expires_at"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	updates := map[string]any{}
	if body.Name != nil {
		updates["name"] = *body.Name
	}
	if body.Description != nil {
		updates["description"] = *body.Description
	}
	if body.RateLimit != nil {
		updates["rate_limit"] = *body.RateLimit
	}
	if body.MaxRequests != nil {
		updates["max_requests"] = *body.MaxRequests
	}
	if body.Enabled != nil {
		updates["enabled"] = *body.Enabled
	}
	if body.ExpiresAt != nil {
		updates["expires_at"] = *body.ExpiresAt
	}

	if len(updates) > 0 {
		if err := store.DB().Model(&store.Token{}).Where("id = ?", tk.ID).Updates(updates).Error; err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
	}
	store.DB().First(&tk, tk.ID)
	c.JSON(http.StatusOK, tk)
}

func deleteToken(c *gin.Context) {
	store.DB().Delete(&store.Token{}, c.Param("id"))
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// --- Models ---

func listModelsAdmin(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"data": config.AllModels()})
}

func syncModels(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 60*time.Second)
	defer cancel()
	result, err := modelsync.Sync(ctx, modelsync.Options{})
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error(), "result": result})
		return
	}
	c.JSON(http.StatusOK, result)
}

func upsertModel(c *gin.Context) {
	var body struct {
		ID         string            `json:"id"`
		Name       *string           `json:"name"`
		Upstream   config.Upstream   `json:"upstream"`
		Protocol   *config.Protocol  `json:"protocol"`
		RealModel  *string           `json:"real_model"`
		Group      string            `json:"group"`
		ContextLen *int              `json:"context_len"`
		Status     *int              `json:"status"`
		Priority   *int              `json:"priority"`
		Tags       []string          `json:"tags"`
		Pricing    map[string]string `json:"pricing"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	body.ID = strings.TrimSpace(body.ID)
	if body.ID == "" || body.Protocol == nil || *body.Protocol == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "id and protocol are required"})
		return
	}
	if body.Upstream == "" {
		body.Upstream = config.UpstreamGo
	}
	if body.Upstream != config.UpstreamGo && body.Upstream != config.UpstreamOllama {
		c.JSON(http.StatusBadRequest, gin.H{"error": "only go and ollama upstreams are supported"})
		return
	}
	if body.Group == "" {
		if body.Upstream == config.UpstreamOllama {
			body.Group = "ollama"
		} else {
			body.Group = "go"
		}
	}
	route := config.ModelRoute{
		ID:       strings.TrimSpace(body.ID),
		Upstream: body.Upstream,
		Protocol: *body.Protocol,
		Group:    body.Group,
		Status:   config.ModelStatusPtr(config.ModelStatusEnabled),
	}
	changed := []string{"protocol"}
	if body.Name != nil {
		route.Name = strings.TrimSpace(*body.Name)
		changed = append(changed, "name")
	}
	if body.RealModel != nil {
		route.RealModel = strings.TrimSpace(*body.RealModel)
		changed = append(changed, "real_model")
	}
	if body.ContextLen != nil {
		route.ContextLen = *body.ContextLen
		changed = append(changed, "context_len")
	}
	if body.Status != nil {
		route.Status = config.ModelStatusPtr(*body.Status)
	}
	if body.Priority != nil {
		route.Priority = *body.Priority
		changed = append(changed, "priority")
	}
	if body.Tags != nil {
		route.Tags = config.NormalizeModelTags(body.Tags)
		changed = append(changed, "tags")
	}
	if body.Pricing != nil {
		route.Pricing = body.Pricing
		changed = append(changed, "pricing")
	}
	route.CustomizedFields = mergeModelCustomizedFields(route.ID, nil, changed...)
	route.IsCustomized = len(route.CustomizedFields) > 0
	row := store.NewModelRouteRow(route)
	if err := store.SaveModelRoute(&row); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	route = store.ModelRouteFromRow(row)
	config.RegisterModel(route)
	c.JSON(http.StatusOK, route)
}

func updateModel(c *gin.Context) {
	id := strings.TrimSpace(c.Param("id"))
	if id == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "id is required"})
		return
	}
	var row store.ModelRouteRow
	if err := store.DB().First(&row, "id = ?", id).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "not found"})
		return
	}
	route := store.ModelRouteFromRow(row)

	var body struct {
		Name       *string           `json:"name"`
		Protocol   *config.Protocol  `json:"protocol"`
		RealModel  *string           `json:"real_model"`
		ContextLen *int              `json:"context_len"`
		Status     *int              `json:"status"`
		Priority   *int              `json:"priority"`
		Tags       []string          `json:"tags"`
		Pricing    map[string]string `json:"pricing"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	changed := []string{}
	if body.Name != nil {
		route.Name = strings.TrimSpace(*body.Name)
		changed = append(changed, "name")
	}
	if body.Protocol != nil {
		route.Protocol = *body.Protocol
		changed = append(changed, "protocol")
	}
	if body.RealModel != nil {
		route.RealModel = strings.TrimSpace(*body.RealModel)
		changed = append(changed, "real_model")
	}
	if body.ContextLen != nil {
		route.ContextLen = *body.ContextLen
		changed = append(changed, "context_len")
	}
	if body.Tags != nil {
		route.Tags = config.NormalizeModelTags(body.Tags)
		changed = append(changed, "tags")
	}
	if body.Pricing != nil {
		route.Pricing = body.Pricing
		changed = append(changed, "pricing")
	}
	if body.Priority != nil {
		route.Priority = *body.Priority
		changed = append(changed, "priority")
	}
	if body.Status != nil {
		route.Status = config.ModelStatusPtr(*body.Status)
	}
	route.CustomizedFields = mergeModelCustomizedFields(id, route.CustomizedFields, changed...)
	route.IsCustomized = route.IsCustomized || len(changed) > 0 || len(route.CustomizedFields) > 0
	nextRow := store.NewModelRouteRow(route)
	nextRow.CreatedAt = row.CreatedAt
	nextRow.LastSyncedAt = row.LastSyncedAt
	if err := store.SaveModelRoute(&nextRow); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	route = store.ModelRouteFromRow(nextRow)
	config.RegisterModel(route)
	c.JSON(http.StatusOK, route)
}

func toggleModel(c *gin.Context) {
	id := strings.TrimSpace(c.Param("id"))
	var row store.ModelRouteRow
	if err := store.DB().First(&row, "id = ?", id).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "not found"})
		return
	}
	route := store.ModelRouteFromRow(row)
	if route.IsEnabled() {
		route.Status = config.ModelStatusPtr(config.ModelStatusDisabled)
	} else {
		route.Status = config.ModelStatusPtr(config.ModelStatusEnabled)
	}
	nextRow := store.NewModelRouteRow(route)
	nextRow.CreatedAt = row.CreatedAt
	nextRow.LastSyncedAt = row.LastSyncedAt
	if err := store.SaveModelRoute(&nextRow); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	route = store.ModelRouteFromRow(nextRow)
	config.RegisterModel(route)
	c.JSON(http.StatusOK, route)
}

func deleteModel(c *gin.Context) {
	id := c.Param("id")
	config.RemoveModel(id)
	store.DeleteModelRoute(id)
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

func mergeModelCustomizedFields(id string, current []string, extra ...string) []string {
	fields := append([]string{}, current...)
	var row store.ModelRouteRow
	if id != "" && store.DB().First(&row, "id = ?", id).Error == nil {
		fields = append(fields, store.ModelRouteFromRow(row).CustomizedFields...)
	}
	fields = append(fields, extra...)
	return config.NormalizeCustomizedFields(fields)
}

// --- Model Mappings ---

func listModelMappingsAdmin(c *gin.Context) {
	rows, err := store.LoadModelMappings()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": rows})
}

func upsertModelMapping(c *gin.Context) {
	var body struct {
		SourceModel string `json:"source_model" binding:"required"`
		TargetModel string `json:"target_model" binding:"required"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	source := strings.TrimSpace(body.SourceModel)
	target := strings.TrimSpace(body.TargetModel)
	if source == "" || target == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "source_model and target_model are required"})
		return
	}
	row := &store.ModelMappingRow{SourceModel: source, TargetModel: target}
	if err := store.SaveModelMapping(row); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	config.RegisterModelMapping(source, target)
	c.JSON(http.StatusOK, row)
}

func deleteModelMapping(c *gin.Context) {
	source := strings.Trim(strings.TrimSpace(c.Param("source")), "/")
	if source == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "source model is required"})
		return
	}
	config.RemoveModelMapping(source)
	store.DeleteModelMapping(source)
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// --- Stats ---

func stats(c *gin.Context) {
	now := time.Now()
	todayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	lastMinute := now.Add(-time.Minute)
	lastHour := now.Add(-time.Hour)
	last24h := now.Add(-24 * time.Hour)

	var totalCalls int64
	store.DB().Model(&store.UsageLog{}).Count(&totalCalls)

	var todayCalls int64
	store.DB().Model(&store.UsageLog{}).Where("created_at >= ?", todayStart).Count(&todayCalls)

	var lastHourCalls int64
	store.DB().Model(&store.UsageLog{}).Where("created_at >= ?", lastHour).Count(&lastHourCalls)

	var rpm int64
	store.DB().Model(&store.UsageLog{}).Where("created_at >= ?", lastMinute).Count(&rpm)

	var tpm int64
	store.DB().Model(&store.UsageLog{}).
		Select("coalesce(sum(total_tokens),0)").
		Where("created_at >= ?", lastMinute).
		Scan(&tpm)

	var successCalls int64
	store.DB().Model(&store.UsageLog{}).Where("status_code < ?", http.StatusBadRequest).Count(&successCalls)

	var errorCalls int64
	store.DB().Model(&store.UsageLog{}).Where("status_code >= ?", http.StatusBadRequest).Count(&errorCalls)

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

	var todayInputTokens int64
	store.DB().Model(&store.UsageLog{}).
		Select("coalesce(sum(input_tokens),0)").
		Where("created_at >= ?", todayStart).
		Scan(&todayInputTokens)

	var todayOutputTokens int64
	store.DB().Model(&store.UsageLog{}).
		Select("coalesce(sum(output_tokens),0)").
		Where("created_at >= ?", todayStart).
		Scan(&todayOutputTokens)

	var todayReasoningTokens int64
	store.DB().Model(&store.UsageLog{}).
		Select("coalesce(sum(reasoning_tokens),0)").
		Where("created_at >= ?", todayStart).
		Scan(&todayReasoningTokens)

	var todayCacheTokens int64
	store.DB().Model(&store.UsageLog{}).
		Select("coalesce(sum(cache_tokens),0)").
		Where("created_at >= ?", todayStart).
		Scan(&todayCacheTokens)

	var todayCacheReadTokens int64
	store.DB().Model(&store.UsageLog{}).
		Select("coalesce(sum(cache_read_tokens),0)").
		Where("created_at >= ?", todayStart).
		Scan(&todayCacheReadTokens)

	var todayCacheCreationTokens int64
	store.DB().Model(&store.UsageLog{}).
		Select("coalesce(sum(cache_creation_tokens),0)").
		Where("created_at >= ?", todayStart).
		Scan(&todayCacheCreationTokens)

	var todayTotalTokens int64
	store.DB().Model(&store.UsageLog{}).
		Select("coalesce(sum(total_tokens),0)").
		Where("created_at >= ?", todayStart).
		Scan(&todayTotalTokens)

	var totalInputTokens int64
	store.DB().Model(&store.UsageLog{}).
		Select("coalesce(sum(input_tokens),0)").
		Scan(&totalInputTokens)

	var totalOutputTokens int64
	store.DB().Model(&store.UsageLog{}).
		Select("coalesce(sum(output_tokens),0)").
		Scan(&totalOutputTokens)

	var totalReasoningTokens int64
	store.DB().Model(&store.UsageLog{}).
		Select("coalesce(sum(reasoning_tokens),0)").
		Scan(&totalReasoningTokens)

	var totalCacheTokens int64
	store.DB().Model(&store.UsageLog{}).
		Select("coalesce(sum(cache_tokens),0)").
		Scan(&totalCacheTokens)

	var totalCacheReadTokens int64
	store.DB().Model(&store.UsageLog{}).
		Select("coalesce(sum(cache_read_tokens),0)").
		Scan(&totalCacheReadTokens)

	var totalCacheCreationTokens int64
	store.DB().Model(&store.UsageLog{}).
		Select("coalesce(sum(cache_creation_tokens),0)").
		Scan(&totalCacheCreationTokens)

	var totalTokens int64
	store.DB().Model(&store.UsageLog{}).
		Select("coalesce(sum(total_tokens),0)").
		Scan(&totalTokens)

	var todayTotalCost float64
	store.DB().Model(&store.UsageLog{}).
		Select("coalesce(sum(total_cost),0)").
		Where("created_at >= ?", todayStart).
		Scan(&todayTotalCost)

	var todayActualCost float64
	store.DB().Model(&store.UsageLog{}).
		Select("coalesce(sum(actual_cost),0)").
		Where("created_at >= ?", todayStart).
		Scan(&todayActualCost)

	var todayAccountCost float64
	store.DB().Model(&store.UsageLog{}).
		Select("coalesce(sum(account_cost),0)").
		Where("created_at >= ?", todayStart).
		Scan(&todayAccountCost)

	var totalCost float64
	store.DB().Model(&store.UsageLog{}).
		Select("coalesce(sum(total_cost),0)").
		Scan(&totalCost)

	var totalActualCost float64
	store.DB().Model(&store.UsageLog{}).
		Select("coalesce(sum(actual_cost),0)").
		Scan(&totalActualCost)

	var totalAccountCost float64
	store.DB().Model(&store.UsageLog{}).
		Select("coalesce(sum(account_cost),0)").
		Scan(&totalAccountCost)

	var durations []int64
	store.DB().Model(&store.UsageLog{}).Where("duration_ms > 0").Order("duration_ms asc").Limit(10000).Pluck("duration_ms", &durations)
	p50Duration := percentileDuration(durations, 0.50)
	p95Duration := percentileDuration(durations, 0.95)
	p99Duration := percentileDuration(durations, 0.99)

	type timelineBucket struct {
		Bucket      string  `json:"bucket"`
		Total       int64   `json:"total"`
		Success     int64   `json:"success"`
		Errors      int64   `json:"errors"`
		AvgDuration float64 `json:"avg_duration_ms"`
	}
	var timeline []timelineBucket
	store.DB().Model(&store.UsageLog{}).
		Select("strftime('%Y-%m-%d %H:00', created_at) as bucket, count(*) as total, sum(case when status_code < 400 then 1 else 0 end) as success, sum(case when status_code >= 400 then 1 else 0 end) as errors, coalesce(avg(duration_ms),0) as avg_duration").
		Where("created_at >= ?", last24h).
		Group("bucket").
		Order("bucket asc").
		Scan(&timeline)

	type latencyBucket struct {
		Range string `json:"range"`
		Count int64  `json:"count"`
	}
	latencyBuckets := []latencyBucket{
		{Range: "<250ms", Count: countLatencyRange(0, 249)},
		{Range: "250-500ms", Count: countLatencyRange(250, 500)},
		{Range: "500ms-1s", Count: countLatencyRange(501, 1000)},
		{Range: "1-3s", Count: countLatencyRange(1001, 3000)},
		{Range: ">3s", Count: countLatencyRange(3001, 0)},
	}

	var keys int64
	store.DB().Model(&store.Key{}).Count(&keys)
	var enabledKeys int64
	store.DB().Model(&store.Key{}).Where("enabled = ?", true).Count(&enabledKeys)
	var tokens int64
	store.DB().Model(&store.Token{}).Count(&tokens)
	var enabledTokens int64
	store.DB().Model(&store.Token{}).Where("enabled = ?", true).Count(&enabledTokens)

	// Recent logs
	var recent []store.UsageLog
	store.DB().Order("id desc").Limit(50).Find(&recent)

	c.JSON(http.StatusOK, gin.H{
		"total_calls":                 totalCalls,
		"today_calls":                 todayCalls,
		"last_hour_calls":             lastHourCalls,
		"success_calls":               successCalls,
		"error_calls":                 errorCalls,
		"success_rate":                ratio(successCalls, totalCalls),
		"error_rate":                  ratio(errorCalls, totalCalls),
		"rpm":                         rpm,
		"tpm":                         tpm,
		"qps":                         float64(rpm) / 60,
		"by_status":                   byStatus,
		"by_model":                    byModel,
		"by_protocol":                 byProtocol,
		"avg_duration_ms":             avgDuration,
		"p50_duration_ms":             p50Duration,
		"p95_duration_ms":             p95Duration,
		"p99_duration_ms":             p99Duration,
		"timeline":                    timeline,
		"latency_buckets":             latencyBuckets,
		"keys":                        keys,
		"enabled_keys":                enabledKeys,
		"tokens":                      tokens,
		"enabled_tokens":              enabledTokens,
		"today_input_tokens":          todayInputTokens,
		"today_output_tokens":         todayOutputTokens,
		"today_reasoning_tokens":      todayReasoningTokens,
		"today_cache_tokens":          todayCacheTokens,
		"today_cache_read_tokens":     todayCacheReadTokens,
		"today_cache_creation_tokens": todayCacheCreationTokens,
		"today_total_tokens":          todayTotalTokens,
		"total_input_tokens":          totalInputTokens,
		"total_output_tokens":         totalOutputTokens,
		"total_reasoning_tokens":      totalReasoningTokens,
		"total_cache_tokens":          totalCacheTokens,
		"total_cache_read_tokens":     totalCacheReadTokens,
		"total_cache_creation_tokens": totalCacheCreationTokens,
		"total_tokens":                totalTokens,
		"today_total_cost":            todayTotalCost,
		"today_actual_cost":           todayActualCost,
		"today_account_cost":          todayAccountCost,
		"total_cost":                  totalCost,
		"total_actual_cost":           totalActualCost,
		"total_account_cost":          totalAccountCost,
		"recent":                      recent,
	})
}

func listUsageLogs(c *gin.Context) {
	page := queryInt(c, "page", 1)
	if page < 1 {
		page = 1
	}
	pageSize := queryInt(c, "page_size", 25)
	if pageSize < 1 {
		pageSize = 25
	}
	if pageSize > 200 {
		pageSize = 200
	}

	q := usageLogQuery(c)

	var total int64
	if err := q.Model(&store.UsageLog{}).Count(&total).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	sortBy := c.DefaultQuery("sort_by", "created_at")
	sortOrder := strings.ToLower(c.DefaultQuery("sort_order", "desc"))
	if sortOrder != "asc" {
		sortOrder = "desc"
	}
	switch sortBy {
	case "created_at", "duration_ms", "status_code", "model", "protocol":
	default:
		sortBy = "created_at"
	}

	var items []store.UsageLog
	err := q.Order(sortBy + " " + sortOrder).
		Offset((page - 1) * pageSize).
		Limit(pageSize).
		Find(&items).Error
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	decorated := make([]usageLogDTO, 0, len(items))
	for _, item := range items {
		decorated = append(decorated, decorateUsageLog(item))
	}

	c.JSON(http.StatusOK, gin.H{
		"items":     decorated,
		"total":     total,
		"page":      page,
		"page_size": pageSize,
		"summary":   usageLogSummary(c),
	})
}

type usageLogDTO struct {
	store.UsageLog
	ModelName string `json:"model_name"`
}

func decorateUsageLog(row store.UsageLog) usageLogDTO {
	if row.BillingMode == "" {
		row.BillingMode = "token"
	}
	route, ok := config.LookupModel(row.Model)
	if ok {
		if row.Group == "" {
			row.Group = route.Group
		}
		if row.InputUnitPrice == 0 {
			row.InputUnitPrice = adminPriceField(route.Pricing, "prompt")
		}
		if row.OutputUnitPrice == 0 {
			row.OutputUnitPrice = adminPriceField(route.Pricing, "completion")
		}
		if row.CacheReadUnitPrice == 0 {
			row.CacheReadUnitPrice = adminPriceField(route.Pricing, "input_cache_read", "cache_read", "prompt_cache_read")
		}
		if row.CacheWriteUnitPrice == 0 {
			row.CacheWriteUnitPrice = adminPriceField(route.Pricing, "input_cache_write", "cache_write", "prompt_cache_write", "input_cache_creation")
		}
	}
	if row.GroupMultiplier <= 0 {
		row.GroupMultiplier = config.GroupMultiplier(row.Group)
	}
	if row.CacheReadTokens == 0 && row.CacheTokens > 0 && row.CacheCreationTokens == 0 {
		row.CacheReadTokens = row.CacheTokens
	}
	if row.ActualCost == 0 && row.TotalCost > 0 {
		row.ActualCost = row.TotalCost * row.GroupMultiplier
		row.AccountCost = row.ActualCost
	}
	modelName := row.Model
	if ok && route.Name != "" {
		modelName = route.Name
	}
	return usageLogDTO{UsageLog: row, ModelName: modelName}
}

func usageLogSummary(c *gin.Context) gin.H {
	q := usageLogQuery(c)
	if strings.TrimSpace(c.Query("start")) == "" && strings.TrimSpace(c.Query("end")) == "" {
		now := time.Now()
		todayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
		q = q.Where("created_at >= ?", todayStart)
	}

	var totalCalls int64
	q.Count(&totalCalls)
	var successCalls int64
	usageLogQueryForSummary(c).Where("status_code < ?", http.StatusBadRequest).Count(&successCalls)
	var errorCalls int64
	usageLogQueryForSummary(c).Where("status_code >= ?", http.StatusBadRequest).Count(&errorCalls)

	type sums struct {
		InputTokens         int64   `gorm:"column:input_tokens"`
		OutputTokens        int64   `gorm:"column:output_tokens"`
		ReasoningTokens     int64   `gorm:"column:reasoning_tokens"`
		CacheReadTokens     int64   `gorm:"column:cache_read_tokens"`
		CacheCreationTokens int64   `gorm:"column:cache_creation_tokens"`
		TotalTokens         int64   `gorm:"column:total_tokens"`
		TotalCost           float64 `gorm:"column:total_cost"`
		ActualCost          float64 `gorm:"column:actual_cost"`
		AccountCost         float64 `gorm:"column:account_cost"`
		AvgDurationMs       float64 `gorm:"column:avg_duration_ms"`
	}
	var s sums
	usageLogQueryForSummary(c).
		Select("coalesce(sum(input_tokens),0) as input_tokens, coalesce(sum(output_tokens),0) as output_tokens, coalesce(sum(reasoning_tokens),0) as reasoning_tokens, coalesce(sum(cache_read_tokens),0) as cache_read_tokens, coalesce(sum(cache_creation_tokens),0) as cache_creation_tokens, coalesce(sum(total_tokens),0) as total_tokens, coalesce(sum(total_cost),0) as total_cost, coalesce(sum(case when actual_cost > 0 then actual_cost else total_cost end),0) as actual_cost, coalesce(sum(case when account_cost > 0 then account_cost else case when actual_cost > 0 then actual_cost else total_cost end end),0) as account_cost, coalesce(avg(duration_ms),0) as avg_duration_ms").
		Scan(&s)

	lastMinute := time.Now().Add(-time.Minute)
	var rpm int64
	usageLogQueryForSummary(c).Where("created_at >= ?", lastMinute).Count(&rpm)
	var tpm int64
	usageLogQueryForSummary(c).Where("created_at >= ?", lastMinute).
		Select("coalesce(sum(total_tokens),0)").
		Scan(&tpm)

	return gin.H{
		"total_calls":           totalCalls,
		"success_calls":         successCalls,
		"error_calls":           errorCalls,
		"rpm":                   rpm,
		"tpm":                   tpm,
		"avg_duration_ms":       s.AvgDurationMs,
		"input_tokens":          s.InputTokens,
		"output_tokens":         s.OutputTokens,
		"reasoning_tokens":      s.ReasoningTokens,
		"cache_read_tokens":     s.CacheReadTokens,
		"cache_creation_tokens": s.CacheCreationTokens,
		"cache_tokens":          s.CacheReadTokens,
		"total_tokens":          s.TotalTokens,
		"total_cost":            s.TotalCost,
		"actual_cost":           s.ActualCost,
		"account_cost":          s.AccountCost,
	}
}

func usageLogQueryForSummary(c *gin.Context) *gorm.DB {
	q := usageLogQuery(c)
	if strings.TrimSpace(c.Query("start")) == "" && strings.TrimSpace(c.Query("end")) == "" {
		now := time.Now()
		todayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
		q = q.Where("created_at >= ?", todayStart)
	}
	return q
}

func adminPriceField(pricing map[string]string, keys ...string) float64 {
	for _, key := range keys {
		raw := strings.TrimSpace(pricing[key])
		if raw == "" {
			continue
		}
		v, err := strconv.ParseFloat(raw, 64)
		if err == nil && v >= 0 {
			return v
		}
	}
	return 0
}

func usageLogQuery(c *gin.Context) *gorm.DB {
	q := store.DB().Model(&store.UsageLog{})
	if model := strings.TrimSpace(c.Query("model")); model != "" {
		q = q.Where("model = ?", model)
	}
	if protocol := strings.TrimSpace(c.Query("protocol")); protocol != "" {
		q = q.Where("protocol = ?", protocol)
	}
	if token := strings.TrimSpace(c.Query("token")); token != "" {
		q = q.Where("token_name = ?", token)
	}
	if group := strings.TrimSpace(c.Query("group")); group != "" {
		q = q.Where("`group` = ?", group)
	}
	if status := strings.TrimSpace(c.Query("status")); status != "" {
		switch status {
		case "success":
			q = q.Where("status_code < ?", http.StatusBadRequest)
		case "error":
			q = q.Where("status_code >= ?", http.StatusBadRequest)
		default:
			if code, err := strconv.Atoi(status); err == nil {
				q = q.Where("status_code = ?", code)
			}
		}
	}
	if stream := strings.TrimSpace(c.Query("stream")); stream != "" {
		if stream == "true" || stream == "1" {
			q = q.Where("stream = ?", true)
		} else if stream == "false" || stream == "0" {
			q = q.Where("stream = ?", false)
		}
	}
	if qText := strings.TrimSpace(c.Query("q")); qText != "" {
		like := "%" + qText + "%"
		q = q.Where("model LIKE ? OR protocol LIKE ? OR token_name LIKE ? OR `group` LIKE ? OR request_id LIKE ? OR ip_address LIKE ? OR error LIKE ?", like, like, like, like, like, like, like)
	}
	if start := parseQueryTime(c.Query("start")); start != nil {
		q = q.Where("created_at >= ?", *start)
	}
	if end := parseQueryTime(c.Query("end")); end != nil {
		q = q.Where("created_at <= ?", *end)
	}
	return q
}

func queryInt(c *gin.Context, key string, fallback int) int {
	raw := strings.TrimSpace(c.Query(key))
	if raw == "" {
		return fallback
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return fallback
	}
	return n
}

func parseQueryTime(raw string) *time.Time {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	layouts := []string{time.RFC3339, "2006-01-02", "2006-01-02 15:04:05"}
	for _, layout := range layouts {
		if t, err := time.Parse(layout, raw); err == nil {
			return &t
		}
	}
	return nil
}

func ratio(part, total int64) float64 {
	if total <= 0 {
		return 0
	}
	return float64(part) / float64(total)
}

func percentileDuration(values []int64, p float64) int64 {
	if len(values) == 0 {
		return 0
	}
	if p <= 0 {
		return values[0]
	}
	if p >= 1 {
		return values[len(values)-1]
	}
	idx := int(math.Ceil(float64(len(values))*p)) - 1
	if idx < 0 {
		idx = 0
	}
	if idx >= len(values) {
		idx = len(values) - 1
	}
	return values[idx]
}

func countLatencyRange(minMs, maxMs int64) int64 {
	q := store.DB().Model(&store.UsageLog{}).Where("duration_ms >= ?", minMs)
	if maxMs > 0 {
		q = q.Where("duration_ms <= ?", maxMs)
	}
	var count int64
	q.Count(&count)
	return count
}

// --- Key Quota ---

func fetchKeyQuota(c *gin.Context) {
	var k store.Key
	if err := store.DB().First(&k, c.Param("id")).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "key not found"})
		return
	}
	cookie := normalizeAuthCookie(k.Cookie)
	if cookie == "" {
		respondKeyQuota(c, k.ID, gin.H{
			"configured": false,
			"message":    "cookie not configured",
		})
		return
	}

	workspaceID := normalizeWorkspaceID(k.WorkspaceID)
	var result *GoQuotaResponse
	if workspaceID == "" {
		resolvedWorkspaceID, resolvedResult, err := resolveWorkspaceForQuota(cookie)
		if err != nil {
			payload := gin.H{
				"configured": true,
				"error":      "workspace_id not configured and auto-detect failed: " + err.Error(),
				"quota":      nil,
				"hint":       "请确认 Cookie 包含有效 auth=...；如果返回了候选 Workspace ID，可点击候选保存后重试。",
			}
			if autoErr, ok := err.(*workspaceAutoDetectError); ok {
				if candidates := workspaceCandidatePayload(autoErr.Candidates); len(candidates) > 0 {
					payload["workspaceCandidates"] = candidates
				}
			}
			respondKeyQuota(c, k.ID, payload)
			return
		}
		workspaceID = resolvedWorkspaceID
		result = resolvedResult
		store.DB().Model(&store.Key{}).Where("id = ?", k.ID).Updates(map[string]any{
			"cookie":       cookie,
			"workspace_id": workspaceID,
		})
		k.Cookie = cookie
		k.WorkspaceID = workspaceID
	} else {
		updates := map[string]any{}
		if cookie != k.Cookie {
			updates["cookie"] = cookie
			k.Cookie = cookie
		}
		if workspaceID != strings.TrimSpace(k.WorkspaceID) {
			updates["workspace_id"] = workspaceID
			k.WorkspaceID = workspaceID
		}
		if len(updates) > 0 {
			store.DB().Model(&store.Key{}).Where("id = ?", k.ID).Updates(updates)
		}
	}

	// Mask cookie value in response
	maskedCookie := ""
	if len(cookie) > 12 {
		maskedCookie = cookie[:8] + "..." + cookie[len(cookie)-4:]
	} else if len(cookie) > 0 {
		maskedCookie = "****"
	}

	var err error
	if result == nil {
		result, err = fetchGoQuota(cookie, workspaceID)
	}
	if err != nil {
		respondKeyQuota(c, k.ID, gin.H{
			"configured":  true,
			"cookie":      maskedCookie,
			"workspaceID": workspaceID,
			"error":       err.Error(),
			"hint":        quotaErrorHint(err.Error()),
			"quota":       nil,
		})
		return
	}

	// Show error from upstream
	if result.Error != "" {
		respondKeyQuota(c, k.ID, gin.H{
			"configured":  true,
			"cookie":      maskedCookie,
			"workspaceID": workspaceID,
			"error":       result.Error,
			"hint":        quotaErrorHint(result.Error),
			"quota":       nil,
		})
		return
	}

	respondKeyQuota(c, k.ID, gin.H{
		"configured":  true,
		"cookie":      maskedCookie,
		"workspaceID": workspaceID,
		"useBalance":  result.UseBalance,
		"quota": gin.H{
			"rolling": goQuotaBucketPayload(result.RollingUsage),
			"weekly":  goQuotaBucketPayload(result.WeeklyUsage),
			"monthly": goQuotaBucketPayload(result.MonthlyUsage),
		},
	})
}

func respondKeyQuota(c *gin.Context, keyID uint, payload gin.H) {
	checkedAt := time.Now().UTC()
	payload["checkedAt"] = checkedAt.Format(time.RFC3339)
	if _, exists := payload["usage"]; !exists {
		payload["usage"] = keyUsagePayload(keyID)
	}
	persistKeyQuotaSnapshot(keyID, payload, checkedAt)
	c.JSON(http.StatusOK, payload)
}

func persistKeyQuotaSnapshot(keyID uint, payload gin.H, checkedAt time.Time) {
	b, err := json.Marshal(payload)
	if err != nil {
		return
	}
	store.DB().Model(&store.Key{}).Where("id = ?", keyID).Updates(map[string]any{
		"quota_snapshot":   string(b),
		"quota_updated_at": checkedAt,
	})
}

func goQuotaBucketPayload(usage *GoQuotaBucket) gin.H {
	if usage == nil {
		return gin.H{
			"usagePercent": nil,
			"resetInSec":   nil,
			"resetIn":      "",
			"status":       "",
		}
	}
	return gin.H{
		"usagePercent": usage.UsagePercent,
		"resetInSec":   usage.ResetInSec,
		"resetIn":      formatGoQuotaReset(usage.ResetInSec),
		"status":       usage.Status,
	}
}

type quotaUsageStats struct {
	Requests     int64 `json:"requests" gorm:"column:requests"`
	InputTokens  int64 `json:"inputTokens" gorm:"column:input_tokens"`
	OutputTokens int64 `json:"outputTokens" gorm:"column:output_tokens"`
	TotalTokens  int64 `json:"totalTokens" gorm:"column:total_tokens"`
}

func keyUsagePayload(keyID uint) gin.H {
	now := time.Now()
	rollingStart := now.Add(-time.Hour)
	weeklyStart := now.Add(-7 * 24 * time.Hour)
	monthlyStart := now.Add(-30 * 24 * time.Hour)
	return gin.H{
		"total":   keyUsageStats(keyID, nil),
		"rolling": keyUsageStats(keyID, &rollingStart),
		"weekly":  keyUsageStats(keyID, &weeklyStart),
		"monthly": keyUsageStats(keyID, &monthlyStart),
	}
}

func keyUsageStats(keyID uint, since *time.Time) quotaUsageStats {
	var stats quotaUsageStats
	q := store.DB().Model(&store.UsageLog{}).Where("key_id = ?", keyID)
	if since != nil {
		q = q.Where("created_at >= ?", *since)
	}
	q.Select("count(*) as requests, coalesce(sum(input_tokens),0) as input_tokens, coalesce(sum(output_tokens),0) as output_tokens, coalesce(sum(total_tokens),0) as total_tokens").
		Scan(&stats)
	return stats
}

// --- helpers ---

func workspaceCandidatePayload(workspaces []OpenCodeWorkspace) []gin.H {
	out := make([]gin.H, 0, len(workspaces))
	for _, ws := range workspaces {
		id := strings.TrimSpace(ws.ID)
		if id == "" {
			continue
		}
		item := gin.H{"id": id}
		if name := strings.TrimSpace(ws.Name); name != "" {
			item["name"] = name
		}
		out = append(out, item)
	}
	return out
}

func quotaErrorHint(errMsg string) string {
	lower := strings.ToLower(errMsg)
	if strings.Contains(lower, "httperror") || strings.Contains(lower, "missing usage buckets") {
		return "请确认 Workspace ID 是 opencode.ai 工作区 ID，且与当前 auth Cookie 属于同一账号；不确定时可清空 Workspace ID 后重新查询自动识别。"
	}
	if strings.Contains(lower, "html login page") || strings.Contains(lower, "cookie") || strings.Contains(lower, "public") {
		return "请重新从浏览器复制包含 auth=... 的 Cookie，Cookie 可能已过期或不是登录态。"
	}
	return "请检查 Cookie 与 Workspace ID 是否匹配；如果只填 Cookie，可先清空 Workspace ID 让系统自动识别。"
}

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
