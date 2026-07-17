package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
	"github.com/xb0or/opencode-GO/config"
	"github.com/xb0or/opencode-GO/modelsync"
	"github.com/xb0or/opencode-GO/pool"
	"github.com/xb0or/opencode-GO/store"
)

func TestMountWithPickerBindsPickerForHealth(t *testing.T) {
	if err := store.InitForTest("file::memory:?cache=shared"); err != nil {
		t.Fatalf("init test db: %v", err)
	}
	gin.SetMode(gin.TestMode)

	r := gin.New()
	MountWithPicker(r.Group("/admin"), pool.NewPicker())

	req := httptest.NewRequest(http.MethodGet, "/admin/health", nil)
	req.Header.Set("Authorization", "Bearer "+signedAdminToken(t))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("unexpected status: %d body=%s", w.Code, w.Body.String())
	}
}

func TestUpdateKeySettingsMasksReturnedValue(t *testing.T) {
	if err := store.InitForTest("file:admin_update_key?mode=memory&cache=shared"); err != nil {
		t.Fatalf("init test db: %v", err)
	}
	gin.SetMode(gin.TestMode)

	key := &store.Key{Value: "original-secret", Group: "go", Label: "old", Enabled: true, Weight: 1}
	if err := store.DB().Create(key).Error; err != nil {
		t.Fatalf("create key: %v", err)
	}

	r := gin.New()
	MountWithPicker(r.Group("/admin"), pool.NewPicker())

	body := bytes.NewBufferString(`{"value":"new-secret-value","label":"new","weight":3,"proxy_url":"http://proxy:8080"}`)
	req := httptest.NewRequest(http.MethodPatch, "/admin/keys/1", body)
	req.Header.Set("Authorization", "Bearer "+signedAdminToken(t))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("unexpected status: %d body=%s", w.Code, w.Body.String())
	}
	var got store.Key
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if got.Value == "new-secret-value" {
		t.Fatal("updated key value should be masked in response")
	}

	var saved store.Key
	store.DB().First(&saved, key.ID)
	if saved.Value != "new-secret-value" || saved.Label != "new" || saved.Weight != 3 || saved.ProxyURL != "http://proxy:8080" {
		t.Fatalf("key not updated: %#v", saved)
	}
}

func TestListTokensReturnsCopyableSKToken(t *testing.T) {
	if err := store.InitForTest("file:admin_token_list?mode=memory&cache=shared"); err != nil {
		t.Fatalf("init test db: %v", err)
	}
	gin.SetMode(gin.TestMode)

	tok, err := pool.CreateToken("copyable", "", 0, nil)
	if err != nil {
		t.Fatalf("create token: %v", err)
	}
	if tok.Token[:3] != "sk-" {
		t.Fatalf("token prefix = %q, want sk-", tok.Token[:3])
	}

	r := gin.New()
	MountWithPicker(r.Group("/admin"), pool.NewPicker())

	req := httptest.NewRequest(http.MethodGet, "/admin/tokens", nil)
	req.Header.Set("Authorization", "Bearer "+signedAdminToken(t))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("unexpected status: %d body=%s", w.Code, w.Body.String())
	}
	var payload struct {
		Data []store.Token `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(payload.Data) != 1 || payload.Data[0].Token != tok.Token {
		t.Fatalf("token should be returned unmasked for copy: %#v", payload.Data)
	}
}

func TestListTokensReturnsUsageAggregate(t *testing.T) {
	if err := store.InitForTest("file:admin_token_usage?mode=memory&cache=shared"); err != nil {
		t.Fatalf("init test db: %v", err)
	}
	gin.SetMode(gin.TestMode)

	tok, err := pool.CreateToken("usage-token", "", 0, nil)
	if err != nil {
		t.Fatalf("create token: %v", err)
	}
	other, err := pool.CreateToken("other-token", "", 0, nil)
	if err != nil {
		t.Fatalf("create other token: %v", err)
	}
	now := time.Now()
	rows := []store.UsageLog{
		{TokenID: tok.ID, TokenName: tok.Name, StatusCode: 200, InputTokens: 10, OutputTokens: 5, TotalTokens: 15, CreatedAt: now},
		{TokenID: tok.ID, TokenName: tok.Name, StatusCode: 200, InputTokens: 20, OutputTokens: 10, TotalTokens: 30, CreatedAt: now.Add(-10 * time.Minute)},
		{TokenID: tok.ID, TokenName: tok.Name, StatusCode: 200, InputTokens: 40, OutputTokens: 20, TotalTokens: 60, CreatedAt: now.Add(-48 * time.Hour)},
		{TokenID: other.ID, TokenName: other.Name, StatusCode: 200, InputTokens: 1, OutputTokens: 2, TotalTokens: 3, CreatedAt: now.Add(-10 * time.Minute)},
	}
	for _, row := range rows {
		if err := store.DB().Create(&row).Error; err != nil {
			t.Fatalf("create usage log: %v", err)
		}
	}

	r := gin.New()
	MountWithPicker(r.Group("/admin"), pool.NewPicker())

	req := httptest.NewRequest(http.MethodGet, "/admin/tokens", nil)
	req.Header.Set("Authorization", "Bearer "+signedAdminToken(t))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("unexpected status: %d body=%s", w.Code, w.Body.String())
	}
	var payload struct {
		Data []struct {
			store.Token
			TotalRequests     int64 `json:"total_requests"`
			TotalInputTokens  int64 `json:"total_input_tokens"`
			TotalOutputTokens int64 `json:"total_output_tokens"`
			TotalTokens       int64 `json:"total_tokens"`
			TodayRequests     int64 `json:"today_requests"`
			TodayTokens       int64 `json:"today_tokens"`
			LastHourRequests  int64 `json:"last_hour_requests"`
			LastHourTokens    int64 `json:"last_hour_tokens"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	var got *struct {
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
	for i := range payload.Data {
		if payload.Data[i].ID == tok.ID {
			got = &payload.Data[i]
			break
		}
	}
	if got == nil {
		t.Fatalf("usage token not found in payload: %#v", payload.Data)
	}
	if got.TotalRequests != 3 || got.TotalTokens != 105 || got.TotalInputTokens != 70 || got.TotalOutputTokens != 35 {
		t.Fatalf("unexpected total aggregate: %#v", got)
	}
	if got.TodayRequests != 2 || got.TodayTokens != 45 {
		t.Fatalf("unexpected today aggregate: %#v", got)
	}
	if got.LastHourRequests != 2 || got.LastHourTokens != 45 {
		t.Fatalf("unexpected last-hour aggregate: %#v", got)
	}
}

func TestStatsIncludesDashboardAccountingFields(t *testing.T) {
	if err := store.InitForTest("file:admin_stats_dashboard?mode=memory&cache=shared"); err != nil {
		t.Fatalf("init test db: %v", err)
	}
	gin.SetMode(gin.TestMode)

	if err := store.DB().Create(&store.Key{Value: "enabled-key", Group: "go", Enabled: true, Weight: 1}).Error; err != nil {
		t.Fatalf("create enabled key: %v", err)
	}
	disabledKey := store.Key{Value: "disabled-key", Group: "go", Enabled: true, Weight: 1}
	if err := store.DB().Create(&disabledKey).Error; err != nil {
		t.Fatalf("create disabled key: %v", err)
	}
	if err := store.DB().Model(&store.Key{}).Where("id = ?", disabledKey.ID).Update("enabled", false).Error; err != nil {
		t.Fatalf("disable key: %v", err)
	}
	if err := store.DB().Create(&store.Token{Token: "sk-enabled", Name: "enabled", Enabled: true}).Error; err != nil {
		t.Fatalf("create enabled token: %v", err)
	}
	disabledToken := store.Token{Token: "sk-disabled", Name: "disabled", Enabled: true}
	if err := store.DB().Create(&disabledToken).Error; err != nil {
		t.Fatalf("create disabled token: %v", err)
	}
	if err := store.DB().Model(&store.Token{}).Where("id = ?", disabledToken.ID).Update("enabled", false).Error; err != nil {
		t.Fatalf("disable token: %v", err)
	}

	now := time.Now()
	rows := []store.UsageLog{
		{Model: "glm-5.1", Protocol: "chat", StatusCode: 200, DurationMs: 1000, InputTokens: 100, OutputTokens: 50, CacheTokens: 20, CacheReadTokens: 20, TotalTokens: 150, TotalCost: 0.15, ActualCost: 0.14, AccountCost: 0.16, CreatedAt: now},
		{Model: "glm-5.1", Protocol: "chat", StatusCode: 200, DurationMs: 3000, InputTokens: 60, OutputTokens: 90, CacheTokens: 15, CacheCreationTokens: 15, TotalTokens: 150, TotalCost: 0.30, ActualCost: 0.28, AccountCost: 0.32, CreatedAt: now},
		{Model: "glm-5", Protocol: "chat", StatusCode: 500, DurationMs: 2000, InputTokens: 10, OutputTokens: 20, CacheTokens: 5, CacheReadTokens: 5, TotalTokens: 30, TotalCost: 0.03, ActualCost: 0.02, AccountCost: 0.04, CreatedAt: now.Add(-48 * time.Hour)},
	}
	if err := store.DB().Create(&rows).Error; err != nil {
		t.Fatalf("create usage logs: %v", err)
	}

	r := gin.New()
	MountWithPicker(r.Group("/admin"), pool.NewPicker())

	req := httptest.NewRequest(http.MethodGet, "/admin/stats", nil)
	req.Header.Set("Authorization", "Bearer "+signedAdminToken(t))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("unexpected status: %d body=%s", w.Code, w.Body.String())
	}
	var got map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode response: %v", err)
	}

	assertJSONNumber(t, got, "keys", 2)
	assertJSONNumber(t, got, "enabled_keys", 1)
	assertJSONNumber(t, got, "tokens", 2)
	assertJSONNumber(t, got, "enabled_tokens", 1)
	assertJSONNumber(t, got, "today_calls", 2)
	assertJSONNumber(t, got, "total_calls", 3)
	assertJSONNumber(t, got, "today_input_tokens", 160)
	assertJSONNumber(t, got, "today_output_tokens", 140)
	assertJSONNumber(t, got, "today_cache_tokens", 35)
	assertJSONNumber(t, got, "today_cache_read_tokens", 20)
	assertJSONNumber(t, got, "today_cache_creation_tokens", 15)
	assertJSONNumber(t, got, "today_total_tokens", 300)
	assertJSONNumber(t, got, "total_input_tokens", 170)
	assertJSONNumber(t, got, "total_output_tokens", 160)
	assertJSONNumber(t, got, "total_cache_tokens", 40)
	assertJSONNumber(t, got, "total_cache_read_tokens", 25)
	assertJSONNumber(t, got, "total_cache_creation_tokens", 15)
	assertJSONNumber(t, got, "total_tokens", 330)
	assertJSONNumber(t, got, "tpm", 300)
	assertJSONFloat(t, got, "today_total_cost", 0.45)
	assertJSONFloat(t, got, "today_actual_cost", 0.42)
	assertJSONFloat(t, got, "today_account_cost", 0.48)
	assertJSONFloat(t, got, "total_cost", 0.48)
	assertJSONFloat(t, got, "total_actual_cost", 0.44)
	assertJSONFloat(t, got, "total_account_cost", 0.52)
}

func TestModelMappingCRUDUpdatesStoreAndRuntimeConfig(t *testing.T) {
	if err := store.InitForTest("file:admin_model_mapping?mode=memory&cache=shared"); err != nil {
		t.Fatalf("init test db: %v", err)
	}
	gin.SetMode(gin.TestMode)
	config.RegisterModelMappings(map[string]string{})

	r := gin.New()
	MountWithPicker(r.Group("/admin"), pool.NewPicker())

	body := bytes.NewBufferString(`{"source_model":"gpt-5.5","target_model":"glm-51"}`)
	req := httptest.NewRequest(http.MethodPost, "/admin/model-mappings", body)
	req.Header.Set("Authorization", "Bearer "+signedAdminToken(t))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("upsert status=%d body=%s", w.Code, w.Body.String())
	}
	if got, ok := config.LookupModelMapping("gpt-5.5"); !ok || got != "glm-51" {
		t.Fatalf("runtime mapping = %q ok=%v, want glm-51 true", got, ok)
	}

	req = httptest.NewRequest(http.MethodGet, "/admin/model-mappings", nil)
	req.Header.Set("Authorization", "Bearer "+signedAdminToken(t))
	w = httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("list status=%d body=%s", w.Code, w.Body.String())
	}
	var payload struct {
		Data []store.ModelMappingRow `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(payload.Data) != 1 || payload.Data[0].SourceModel != "gpt-5.5" || payload.Data[0].TargetModel != "glm-51" {
		t.Fatalf("unexpected mappings: %#v", payload.Data)
	}

	req = httptest.NewRequest(http.MethodDelete, "/admin/model-mappings/gpt-5.5", nil)
	req.Header.Set("Authorization", "Bearer "+signedAdminToken(t))
	w = httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("delete status=%d body=%s", w.Code, w.Body.String())
	}
	if got, ok := config.LookupModelMapping("gpt-5.5"); ok {
		t.Fatalf("runtime mapping should be removed, got %q", got)
	}
}

func TestDeleteModelMappingSupportsSlashInSource(t *testing.T) {
	if err := store.InitForTest("file:admin_model_mapping_slash?mode=memory&cache=shared"); err != nil {
		t.Fatalf("init test db: %v", err)
	}
	gin.SetMode(gin.TestMode)
	config.RegisterModelMappings(map[string]string{})
	if err := store.SaveModelMapping(&store.ModelMappingRow{
		SourceModel: "openai/gpt-5.5",
		TargetModel: "glm-51",
	}); err != nil {
		t.Fatalf("save mapping: %v", err)
	}
	config.RegisterModelMapping("openai/gpt-5.5", "glm-51")

	r := gin.New()
	MountWithPicker(r.Group("/admin"), pool.NewPicker())

	req := httptest.NewRequest(http.MethodDelete, "/admin/model-mappings/openai/gpt-5.5", nil)
	req.Header.Set("Authorization", "Bearer "+signedAdminToken(t))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("delete status=%d body=%s", w.Code, w.Body.String())
	}
	if got, ok := config.LookupModelMapping("openai/gpt-5.5"); ok {
		t.Fatalf("runtime slash mapping should be removed, got %q", got)
	}
	rows, err := store.LoadModelMappings()
	if err != nil {
		t.Fatalf("load mappings: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("mapping row should be deleted: %#v", rows)
	}
}

func TestModelPatchAndTogglePersistRuntimeConfig(t *testing.T) {
	if err := store.InitForTest("file:admin_model_patch_toggle?mode=memory&cache=shared"); err != nil {
		t.Fatalf("init test db: %v", err)
	}
	gin.SetMode(gin.TestMode)
	initial := config.ModelRoute{
		ID:        "editable-model",
		Name:      "Editable Model",
		Upstream:  config.UpstreamGo,
		Protocol:  config.ProtocolChat,
		RealModel: "editable-model",
		Group:     "go",
		Status:    config.ModelStatusPtr(config.ModelStatusEnabled),
	}
	row := store.NewModelRouteRow(initial)
	if err := store.SaveModelRoute(&row); err != nil {
		t.Fatalf("save model: %v", err)
	}
	config.ReplaceModels([]config.ModelRoute{initial})
	defer config.ReplaceModels(nil)

	r := gin.New()
	MountWithPicker(r.Group("/admin"), pool.NewPicker())

	body := bytes.NewBufferString(`{"name":"Admin Name","context_len":32000,"priority":7,"tags":["code","reasoning"],"pricing":{"prompt":"0.000001","completion":"0.000002"}}`)
	req := httptest.NewRequest(http.MethodPatch, "/admin/models/editable-model", body)
	req.Header.Set("Authorization", "Bearer "+signedAdminToken(t))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("patch status=%d body=%s", w.Code, w.Body.String())
	}

	route, ok := config.LookupModel("editable-model")
	if !ok {
		t.Fatal("runtime model missing after patch")
	}
	if route.Name != "Admin Name" || route.ContextLen != 32000 || route.Priority != 7 || route.Pricing["completion"] != "0.000002" {
		t.Fatalf("runtime route not patched: %#v", route)
	}
	for _, want := range []string{"name", "context_len", "priority", "tags", "pricing"} {
		if !config.IsModelFieldCustomized(route, want) {
			t.Fatalf("customized field %q not recorded: %#v", want, route.CustomizedFields)
		}
	}

	req = httptest.NewRequest(http.MethodPost, "/admin/models/editable-model/toggle", nil)
	req.Header.Set("Authorization", "Bearer "+signedAdminToken(t))
	w = httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("toggle status=%d body=%s", w.Code, w.Body.String())
	}
	route, _ = config.LookupModel("editable-model")
	if route.IsEnabled() {
		t.Fatalf("model should be disabled after toggle: %#v", route)
	}
}

func assertJSONNumber(t *testing.T, payload map[string]any, key string, want float64) {
	t.Helper()
	got, ok := payload[key].(float64)
	if !ok {
		t.Fatalf("%s missing or not numeric: %#v", key, payload[key])
	}
	if got != want {
		t.Fatalf("%s = %v, want %v", key, got, want)
	}
}

func assertJSONFloat(t *testing.T, payload map[string]any, key string, want float64) {
	t.Helper()
	got, ok := payload[key].(float64)
	if !ok {
		t.Fatalf("%s missing or not numeric: %#v", key, payload[key])
	}
	if diff := got - want; diff < -0.000001 || diff > 0.000001 {
		t.Fatalf("%s = %v, want %v", key, got, want)
	}
}

func signedAdminToken(t *testing.T) string {
	t.Helper()
	claims := jwt.MapClaims{
		"role": "admin",
		"exp":  time.Now().Add(time.Hour).Unix(),
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	signed, err := tok.SignedString([]byte(config.Get().JWTSecret))
	if err != nil {
		t.Fatalf("sign token: %v", err)
	}
	return signed
}

// TestAdminCreatedUpstreamConfigSurvivesSync verifies that a model created
// via POST /admin/models with explicit upstream/upstreams retains those
// values after a model sync (they are marked as customized).
func TestAdminCreatedUpstreamConfigSurvivesSync(t *testing.T) {
	if err := store.InitForTest("file:admin_upstream_sync?mode=memory&cache=shared"); err != nil {
		t.Fatalf("init test db: %v", err)
	}
	config.ReplaceModels(nil)
	gin.SetMode(gin.TestMode)

	r := gin.New()
	MountWithPicker(r.Group("/admin"), pool.NewPicker())

	// Create a model with explicit upstream and upstreams.
	body := bytes.NewBufferString(`{"id":"test-model","protocol":"chat","upstream":"ollama","upstreams":["ollama","go"]}`)
	req := httptest.NewRequest(http.MethodPost, "/admin/models", body)
	req.Header.Set("Authorization", "Bearer "+signedAdminToken(t))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("create model: status=%d body=%s", w.Code, w.Body.String())
	}

	// Run a sync that would normally overwrite upstreams.
	opencodeSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"test-model"}]}`))
	}))
	defer opencodeSrv.Close()

	ollamaSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"models":[]}`))
	}))
	defer ollamaSrv.Close()

	openrouterSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[]}`))
	}))
	defer openrouterSrv.Close()

	_, err := modelsync.Sync(context.Background(), modelsync.Options{
		OpenCodeModelsURL:   opencodeSrv.URL,
		OllamaModelsURL:     ollamaSrv.URL,
		OpenRouterModelsURL: openrouterSrv.URL,
		Now:                 func() time.Time { return time.Unix(100, 0) },
	})
	if err != nil {
		t.Fatalf("sync: %v", err)
	}

	got, ok := config.LookupModel("test-model")
	if !ok {
		t.Fatal("model missing after sync")
	}
	if got.Upstream != config.UpstreamOllama {
		t.Fatalf("Upstream overwritten: got %q, want %q", got.Upstream, config.UpstreamOllama)
	}
	if len(got.Upstreams) != 2 || got.Upstreams[0] != config.UpstreamOllama || got.Upstreams[1] != config.UpstreamGo {
		t.Fatalf("Upstreams overwritten: got %v, want [ollama go]", got.Upstreams)
	}
}

// TestAdminCreatedUpstreamGroupsSurviveSync verifies that upstream_groups
// set via POST /admin/models are preserved after a model sync.
func TestAdminCreatedUpstreamGroupsSurviveSync(t *testing.T) {
	if err := store.InitForTest("file:admin_groups_sync?mode=memory&cache=shared"); err != nil {
		t.Fatalf("init test db: %v", err)
	}
	config.ReplaceModels(nil)
	gin.SetMode(gin.TestMode)

	r := gin.New()
	MountWithPicker(r.Group("/admin"), pool.NewPicker())

	// Create a model with upstream_groups.
	body := bytes.NewBufferString(`{"id":"test-model-groups","protocol":"chat","upstream":"go","upstreams":["go","ollama"],"upstream_groups":{"go":"premium-go","ollama":"premium-ollama"}}`)
	req := httptest.NewRequest(http.MethodPost, "/admin/models", body)
	req.Header.Set("Authorization", "Bearer "+signedAdminToken(t))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("create model: status=%d body=%s", w.Code, w.Body.String())
	}

	// Run a sync that would normally clear upstream_groups.
	opencodeSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"test-model-groups"}]}`))
	}))
	defer opencodeSrv.Close()

	ollamaSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"models":[]}`))
	}))
	defer ollamaSrv.Close()

	openrouterSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[]}`))
	}))
	defer openrouterSrv.Close()

	_, err := modelsync.Sync(context.Background(), modelsync.Options{
		OpenCodeModelsURL:   opencodeSrv.URL,
		OllamaModelsURL:     ollamaSrv.URL,
		OpenRouterModelsURL: openrouterSrv.URL,
		Now:                 func() time.Time { return time.Unix(100, 0) },
	})
	if err != nil {
		t.Fatalf("sync: %v", err)
	}

	got, ok := config.LookupModel("test-model-groups")
	if !ok {
		t.Fatal("model missing after sync")
	}
	if got.UpstreamGroups == nil {
		t.Fatal("UpstreamGroups cleared after sync")
	}
	if got.UpstreamGroups[config.UpstreamGo] != "premium-go" {
		t.Fatalf("UpstreamGroups[go] overwritten: got %q, want %q", got.UpstreamGroups[config.UpstreamGo], "premium-go")
	}
	if got.UpstreamGroups[config.UpstreamOllama] != "premium-ollama" {
		t.Fatalf("UpstreamGroups[ollama] overwritten: got %q, want %q", got.UpstreamGroups[config.UpstreamOllama], "premium-ollama")
	}
}

// TestAdminPatchedUpstreamConfigSurvivesSync verifies that upstream/upstreams
// updated via PATCH /admin/models/:id are preserved after a model sync.
func TestAdminPatchedUpstreamConfigSurvivesSync(t *testing.T) {
	if err := store.InitForTest("file:admin_patch_upstream_sync?mode=memory&cache=shared"); err != nil {
		t.Fatalf("init test db: %v", err)
	}
	config.ReplaceModels(nil)
	gin.SetMode(gin.TestMode)

	r := gin.New()
	MountWithPicker(r.Group("/admin"), pool.NewPicker())

	// Create a model with default upstream (go).
	body := bytes.NewBufferString(`{"id":"test-patch-model","protocol":"chat"}`)
	req := httptest.NewRequest(http.MethodPost, "/admin/models", body)
	req.Header.Set("Authorization", "Bearer "+signedAdminToken(t))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("create model: status=%d body=%s", w.Code, w.Body.String())
	}

	// PATCH to change upstream to ollama and set upstreams.
	body2 := bytes.NewBufferString(`{"upstream":"ollama","upstreams":["ollama","go"]}`)
	req2 := httptest.NewRequest(http.MethodPatch, "/admin/models/test-patch-model", body2)
	req2.Header.Set("Authorization", "Bearer "+signedAdminToken(t))
	req2.Header.Set("Content-Type", "application/json")
	w2 := httptest.NewRecorder()
	r.ServeHTTP(w2, req2)
	if w2.Code != http.StatusOK {
		t.Fatalf("patch model: status=%d body=%s", w2.Code, w2.Body.String())
	}

	// Run a sync that would normally overwrite upstreams.
	opencodeSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"test-patch-model"}]}`))
	}))
	defer opencodeSrv.Close()

	ollamaSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"models":[]}`))
	}))
	defer ollamaSrv.Close()

	openrouterSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[]}`))
	}))
	defer openrouterSrv.Close()

	_, err := modelsync.Sync(context.Background(), modelsync.Options{
		OpenCodeModelsURL:   opencodeSrv.URL,
		OllamaModelsURL:     ollamaSrv.URL,
		OpenRouterModelsURL: openrouterSrv.URL,
		Now:                 func() time.Time { return time.Unix(100, 0) },
	})
	if err != nil {
		t.Fatalf("sync: %v", err)
	}

	got, ok := config.LookupModel("test-patch-model")
	if !ok {
		t.Fatal("model missing after sync")
	}
	if got.Upstream != config.UpstreamOllama {
		t.Fatalf("Upstream overwritten after patch: got %q, want %q", got.Upstream, config.UpstreamOllama)
	}
	if len(got.Upstreams) != 2 || got.Upstreams[0] != config.UpstreamOllama || got.Upstreams[1] != config.UpstreamGo {
		t.Fatalf("Upstreams overwritten after patch: got %v, want [ollama go]", got.Upstreams)
	}
}
