package modelsync

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/xb0or/opencode-GO/config"
	"github.com/xb0or/opencode-GO/store"
)

func TestSyncMergesSourcesAndPreservesCustomizedFields(t *testing.T) {
	if err := store.InitForTest("file:modelsync_merge?mode=memory&cache=shared"); err != nil {
		t.Fatalf("init test db: %v", err)
	}
	config.ReplaceModels(nil)

	custom := config.ModelRoute{
		ID:               "gpt-4o",
		Name:             "Admin 4o",
		Upstream:         config.UpstreamGo,
		Protocol:         config.ProtocolChat,
		RealModel:        "gpt-4o",
		Group:            "go",
		ContextLen:       999,
		Status:           config.ModelStatusPtr(config.ModelStatusEnabled),
		Tags:             []string{"custom"},
		Pricing:          map[string]string{"prompt": "9"},
		CustomizedFields: []string{"name", "context_len", "tags", "pricing"},
		IsCustomized:     true,
	}
	row := store.NewModelRouteRow(custom)
	if err := store.SaveModelRoute(&row); err != nil {
		t.Fatalf("save custom row: %v", err)
	}

	opencodeSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"gpt-4o"},{"id":"glm-5.2"}]}`))
	}))
	defer opencodeSrv.Close()

	ollamaSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"models":[{"name":"glm-5.2"},{"name":"gpt-oss:120b"}]}`))
	}))
	defer ollamaSrv.Close()

	openrouterSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[
			{"id":"openai/gpt-4o","name":"OpenAI: GPT-4o","context_length":128000,"pricing":{"prompt":"0.000001"},"supported_parameters":["tools","structured_outputs"],"architecture":{"input_modalities":["text","image"]}},
			{"id":"z-ai/glm-5.2","name":"Z.ai: GLM 5.2","context_length":202752,"supported_parameters":["reasoning"],"architecture":{"input_modalities":["text"]}}
		]}`))
	}))
	defer openrouterSrv.Close()

	result, err := Sync(context.Background(), Options{
		OpenCodeModelsURL:   opencodeSrv.URL,
		OllamaModelsURL:     ollamaSrv.URL,
		OpenRouterModelsURL: openrouterSrv.URL,
		Now:                 func() time.Time { return time.Unix(100, 0) },
	})
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	// Go returned 2 models, Ollama returned 2 models, one overlaps (glm-5.2).
	// Merged unique: gpt-4o, glm-5.2, gpt-oss:120b = 3 models.
	// gpt-4o existed (updated), glm-5.2 + gpt-oss:120b are new (created).
	if result.OpenCodeCount != 2 || result.OllamaCount != 2 || result.MatchedCount != 2 || result.CreatedCount != 2 || result.UpdatedCount != 1 {
		t.Fatalf("unexpected result: %#v", result)
	}

	// Custom fields preserved
	got, ok := config.LookupModel("gpt-4o")
	if !ok {
		t.Fatal("custom model missing from runtime config")
	}
	if got.Name != "Admin 4o" || got.ContextLen != 999 || got.Pricing["prompt"] != "9" {
		t.Fatalf("custom fields overwritten: %#v", got)
	}
	if len(got.Tags) != 1 || got.Tags[0] != "custom" {
		t.Fatalf("custom tags overwritten: %#v", got.Tags)
	}
	if got.OpenRouterID != "openai/gpt-4o" {
		t.Fatalf("OpenRouter metadata not refreshed: %#v", got)
	}
	// gpt-4o is Go-only → Upstreams should be ["go"]
	if len(got.Upstreams) != 1 || got.Upstreams[0] != config.UpstreamGo {
		t.Fatalf("gpt-4o upstreams should be [go]: %#v", got.Upstreams)
	}

	// Overlapping model: glm-5.2 appears in both Go and Ollama
	overlap, ok := config.LookupModel("glm-5.2")
	if !ok {
		t.Fatal("overlapping model glm-5.2 missing from runtime config")
	}
	if !overlap.IsEnabled() || overlap.ContextLen != 202752 || overlap.OpenRouterID != "z-ai/glm-5.2" {
		t.Fatalf("overlapping model not enriched/enabled: %#v", overlap)
	}
	// glm-5.2 appears in both Go and Ollama → Upstreams should be ["go","ollama"]
	if len(overlap.Upstreams) != 2 {
		t.Fatalf("glm-5.2 should have 2 upstreams: %#v", overlap.Upstreams)
	}
	hasGo, hasOllama := false, false
	for _, u := range overlap.Upstreams {
		if u == config.UpstreamGo {
			hasGo = true
		}
		if u == config.UpstreamOllama {
			hasOllama = true
		}
	}
	if !hasGo || !hasOllama {
		t.Fatalf("glm-5.2 should have both go and ollama upstreams: %#v", overlap.Upstreams)
	}

	// Ollama-only model: gpt-oss:120b
	ollamaOnly, ok := config.LookupModel("gpt-oss:120b")
	if !ok {
		t.Fatal("ollama-only model gpt-oss:120b missing from runtime config")
	}
	if len(ollamaOnly.Upstreams) != 1 || ollamaOnly.Upstreams[0] != config.UpstreamOllama {
		t.Fatalf("gpt-oss:120b should have only ollama upstream: %#v", ollamaOnly.Upstreams)
	}
	if ollamaOnly.Group != "ollama" {
		t.Fatalf("gpt-oss:120b group should be ollama: %s", ollamaOnly.Group)
	}
}

func TestSyncContinuesWhenOpenRouterFails(t *testing.T) {
	if err := store.InitForTest("file:modelsync_openrouter_fail?mode=memory&cache=shared"); err != nil {
		t.Fatalf("init test db: %v", err)
	}
	config.ReplaceModels(nil)
	opencodeSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"fallback-model"}]}`))
	}))
	defer opencodeSrv.Close()

	ollamaSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"models":[]}`))
	}))
	defer ollamaSrv.Close()

	openrouterSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer openrouterSrv.Close()

	result, err := Sync(context.Background(), Options{
		OpenCodeModelsURL:   opencodeSrv.URL,
		OllamaModelsURL:     ollamaSrv.URL,
		OpenRouterModelsURL: openrouterSrv.URL,
	})
	if err != nil {
		t.Fatalf("sync should tolerate OpenRouter failure: %v", err)
	}
	if len(result.Warnings) == 0 || result.CreatedCount != 1 {
		t.Fatalf("expected warning and created row: %#v", result)
	}
	if _, ok := config.LookupModel("fallback-model"); !ok {
		t.Fatal("fallback model missing after partial sync")
	}
}

func TestSyncContinuesWhenOllamaFails(t *testing.T) {
	if err := store.InitForTest("file:modelsync_ollama_fail?mode=memory&cache=shared"); err != nil {
		t.Fatalf("init test db: %v", err)
	}
	config.ReplaceModels(nil)
	opencodeSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"go-only-model"}]}`))
	}))
	defer opencodeSrv.Close()

	// Ollama server returns error
	ollamaSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer ollamaSrv.Close()

	openrouterSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[]}`))
	}))
	defer openrouterSrv.Close()

	result, err := Sync(context.Background(), Options{
		OpenCodeModelsURL:   opencodeSrv.URL,
		OllamaModelsURL:     ollamaSrv.URL,
		OpenRouterModelsURL: openrouterSrv.URL,
	})
	if err != nil {
		t.Fatalf("sync should tolerate Ollama failure: %v", err)
	}
	if len(result.Warnings) == 0 || result.CreatedCount != 1 {
		t.Fatalf("expected warning and created row: %#v", result)
	}
	got, ok := config.LookupModel("go-only-model")
	if !ok {
		t.Fatal("go-only model missing after partial sync")
	}
	if len(got.Upstreams) != 1 || got.Upstreams[0] != config.UpstreamGo {
		t.Fatalf("go-only model should have only go upstream: %#v", got.Upstreams)
	}
}

// TestSyncPreservesOllamaUpstream verifies that when an existing route has
// Ollama as its primary upstream, model sync does NOT overwrite it to Go.
func TestSyncPreservesOllamaUpstream(t *testing.T) {
	if err := store.InitForTest("file:modelsync_preserve_ollama?mode=memory&cache=shared"); err != nil {
		t.Fatalf("init test db: %v", err)
	}
	config.ReplaceModels(nil)

	ollamaRoute := config.ModelRoute{
		ID:        "glm-5.2",
		Name:      "GLM 5.2 (Ollama)",
		Upstream:  config.UpstreamOllama,
		Upstreams: []config.Upstream{config.UpstreamOllama, config.UpstreamGo},
		Protocol:  config.ProtocolChat,
		RealModel: "glm-5.2",
		Group:     "ollama",
		Status:    config.ModelStatusPtr(config.ModelStatusEnabled),
		IsCustomized:     true,
		CustomizedFields: []string{"upstream", "upstreams", "group"},
	}
	row := store.NewModelRouteRow(ollamaRoute)
	if err := store.SaveModelRoute(&row); err != nil {
		t.Fatalf("save ollama route: %v", err)
	}

	opencodeSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"glm-5.2"}]}`))
	}))
	defer opencodeSrv.Close()

	openrouterSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[]}`))
	}))
	defer openrouterSrv.Close()

	_, err := Sync(context.Background(), Options{
		OpenCodeModelsURL:   opencodeSrv.URL,
		OpenRouterModelsURL: openrouterSrv.URL,
		Now:                 func() time.Time { return time.Unix(100, 0) },
	})
	if err != nil {
		t.Fatalf("sync: %v", err)
	}

	got, ok := config.LookupModel("glm-5.2")
	if !ok {
		t.Fatal("model missing after sync")
	}
	if got.Upstream != config.UpstreamOllama {
		t.Fatalf("Upstream overwritten: got %q, want %q", got.Upstream, config.UpstreamOllama)
	}
	if len(got.Upstreams) != 2 || got.Upstreams[0] != config.UpstreamOllama {
		t.Fatalf("Upstreams modified: got %v, want [ollama go]", got.Upstreams)
	}
	if got.Group != "ollama" {
		t.Fatalf("Group overwritten: got %q, want %q", got.Group, "ollama")
	}
}

// TestReloadRuntimeFromStorePreservesCustomGroup verifies that routes with
// custom groups (e.g. "premium") survive ReloadRuntimeFromStore.
func TestReloadRuntimeFromStorePreservesCustomGroup(t *testing.T) {
	if err := store.InitForTest("file:modelsync_custom_group?mode=memory&cache=shared"); err != nil {
		t.Fatalf("init test db: %v", err)
	}
	config.ReplaceModels(nil)

	customRoute := config.ModelRoute{
		ID:        "premium-model",
		Name:      "Premium Model",
		Upstream:  config.UpstreamGo,
		Protocol:  config.ProtocolChat,
		RealModel: "premium-v1",
		Group:     "premium",
		Status:    config.ModelStatusPtr(config.ModelStatusEnabled),
	}
	row := store.NewModelRouteRow(customRoute)
	if err := store.SaveModelRoute(&row); err != nil {
		t.Fatalf("save custom route: %v", err)
	}

	if err := ReloadRuntimeFromStore(); err != nil {
		t.Fatalf("ReloadRuntimeFromStore: %v", err)
	}

	got, ok := config.LookupModel("premium-model")
	if !ok {
		t.Fatal("premium model missing after ReloadRuntimeFromStore")
	}
	if got.Group != "premium" {
		t.Fatalf("Group overwritten: got %q, want %q", got.Group, "premium")
	}
	if !got.IsEnabled() {
		t.Fatal("premium model should be enabled")
	}
}

// TestSyncUpdatesDiscoveredUpstreamsForExistingRoute verifies that when a
// model was previously Go-only and Ollama later starts serving it, the sync
// updates Upstreams to include both providers.
func TestSyncUpdatesDiscoveredUpstreamsForExistingRoute(t *testing.T) {
	if err := store.InitForTest("file:modelsync_discover_upstream?mode=memory&cache=shared"); err != nil {
		t.Fatalf("init test db: %v", err)
	}
	config.ReplaceModels(nil)

	// Route was previously Go-only (set by a previous sync run, not customized).
	existing := config.ModelRoute{
		ID:        "glm-5.2",
		Upstream:  config.UpstreamGo,
		Upstreams: []config.Upstream{config.UpstreamGo},
		Protocol:  config.ProtocolChat,
		RealModel: "glm-5.2",
		Group:     "go",
		Status:    config.ModelStatusPtr(config.ModelStatusEnabled),
	}
	row := store.NewModelRouteRow(existing)
	if err := store.SaveModelRoute(&row); err != nil {
		t.Fatalf("save existing route: %v", err)
	}

	// Both Go and Ollama now serve this model.
	opencodeSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"glm-5.2"}]}`))
	}))
	defer opencodeSrv.Close()

	ollamaSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"models":[{"name":"glm-5.2"}]}`))
	}))
	defer ollamaSrv.Close()

	openrouterSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[]}`))
	}))
	defer openrouterSrv.Close()

	_, err := Sync(context.Background(), Options{
		OpenCodeModelsURL:   opencodeSrv.URL,
		OllamaModelsURL:     ollamaSrv.URL,
		OpenRouterModelsURL: openrouterSrv.URL,
		Now:                 func() time.Time { return time.Unix(100, 0) },
	})
	if err != nil {
		t.Fatalf("sync: %v", err)
	}

	got, ok := config.LookupModel("glm-5.2")
	if !ok {
		t.Fatal("model missing after sync")
	}
	if len(got.Upstreams) != 2 {
		t.Fatalf("expected 2 upstreams after discovery, got %v", got.Upstreams)
	}
	hasGo, hasOllama := false, false
	for _, u := range got.Upstreams {
		if u == config.UpstreamGo {
			hasGo = true
		}
		if u == config.UpstreamOllama {
			hasOllama = true
		}
	}
	if !hasGo || !hasOllama {
		t.Fatalf("expected both go and ollama upstreams, got %v", got.Upstreams)
	}
}

// TestSyncRemovesUnavailableUpstreamForExistingRoute verifies that when a
// provider stops serving a model, the sync removes it from Upstreams.
func TestSyncRemovesUnavailableUpstreamForExistingRoute(t *testing.T) {
	if err := store.InitForTest("file:modelsync_remove_upstream?mode=memory&cache=shared"); err != nil {
		t.Fatalf("init test db: %v", err)
	}
	config.ReplaceModels(nil)

	// Route previously had both Go and Ollama (set by sync, not customized).
	existing := config.ModelRoute{
		ID:        "glm-5.2",
		Upstream:  config.UpstreamGo,
		Upstreams: []config.Upstream{config.UpstreamGo, config.UpstreamOllama},
		Protocol:  config.ProtocolChat,
		RealModel: "glm-5.2",
		Group:     "go",
		Status:    config.ModelStatusPtr(config.ModelStatusEnabled),
	}
	row := store.NewModelRouteRow(existing)
	if err := store.SaveModelRoute(&row); err != nil {
		t.Fatalf("save existing route: %v", err)
	}

	// Only Go serves this model now; Ollama no longer has it.
	opencodeSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"glm-5.2"}]}`))
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

	_, err := Sync(context.Background(), Options{
		OpenCodeModelsURL:   opencodeSrv.URL,
		OllamaModelsURL:     ollamaSrv.URL,
		OpenRouterModelsURL: openrouterSrv.URL,
		Now:                 func() time.Time { return time.Unix(100, 0) },
	})
	if err != nil {
		t.Fatalf("sync: %v", err)
	}

	got, ok := config.LookupModel("glm-5.2")
	if !ok {
		t.Fatal("model missing after sync")
	}
	if len(got.Upstreams) != 1 || got.Upstreams[0] != config.UpstreamGo {
		t.Fatalf("expected only go upstream after removal, got %v", got.Upstreams)
	}
}

// TestSyncPreservesExplicitlyCustomizedUpstreams verifies that when an admin
// explicitly customized upstream/upstreams, sync does NOT overwrite them.
func TestSyncPreservesExplicitlyCustomizedUpstreams(t *testing.T) {
	if err := store.InitForTest("file:modelsync_custom_upstream?mode=memory&cache=shared"); err != nil {
		t.Fatalf("init test db: %v", err)
	}
	config.ReplaceModels(nil)

	// Admin explicitly customized the upstream fields.
	custom := config.ModelRoute{
		ID:        "glm-5.2",
		Name:      "Custom GLM",
		Upstream:  config.UpstreamOllama,
		Upstreams: []config.Upstream{config.UpstreamOllama},
		Protocol:  config.ProtocolChat,
		RealModel: "glm-5.2",
		Group:     "ollama",
		Status:    config.ModelStatusPtr(config.ModelStatusEnabled),
		IsCustomized:     true,
		CustomizedFields: []string{"upstream", "upstreams"},
	}
	row := store.NewModelRouteRow(custom)
	if err := store.SaveModelRoute(&row); err != nil {
		t.Fatalf("save custom route: %v", err)
	}

	// Go also serves this model, but admin customization should be preserved.
	opencodeSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"glm-5.2"}]}`))
	}))
	defer opencodeSrv.Close()

	openrouterSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[]}`))
	}))
	defer openrouterSrv.Close()

	_, err := Sync(context.Background(), Options{
		OpenCodeModelsURL:   opencodeSrv.URL,
		OpenRouterModelsURL: openrouterSrv.URL,
		Now:                 func() time.Time { return time.Unix(100, 0) },
	})
	if err != nil {
		t.Fatalf("sync: %v", err)
	}

	got, ok := config.LookupModel("glm-5.2")
	if !ok {
		t.Fatal("model missing after sync")
	}
	if got.Upstream != config.UpstreamOllama {
		t.Fatalf("customized Upstream overwritten: got %q, want %q", got.Upstream, config.UpstreamOllama)
	}
	if len(got.Upstreams) != 1 || got.Upstreams[0] != config.UpstreamOllama {
		t.Fatalf("customized Upstreams overwritten: got %v, want [ollama]", got.Upstreams)
	}
}

// TestSyncOllamaFailurePreservesExistingOllamaMembership verifies that when
// the Ollama catalog fetch fails, existing routes with Ollama in their
// upstreams do NOT lose the Ollama membership.
func TestSyncOllamaFailurePreservesExistingOllamaMembership(t *testing.T) {
	if err := store.InitForTest("file:sync_ollama_fail?mode=memory&cache=shared"); err != nil {
		t.Fatalf("init test db: %v", err)
	}
	config.ReplaceModels(nil)

	// Route has both Go and Ollama (set by a previous sync, not customized).
	existing := config.ModelRoute{
		ID:        "glm-5.2",
		Upstream:  config.UpstreamGo,
		Upstreams: []config.Upstream{config.UpstreamGo, config.UpstreamOllama},
		Protocol:  config.ProtocolChat,
		RealModel: "glm-5.2",
		Group:     "go",
		Status:    config.ModelStatusPtr(config.ModelStatusEnabled),
	}
	row := store.NewModelRouteRow(existing)
	if err := store.SaveModelRoute(&row); err != nil {
		t.Fatalf("save existing route: %v", err)
	}

	// Go succeeds, Ollama fails (returns 502).
	opencodeSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"glm-5.2"}]}`))
	}))
	defer opencodeSrv.Close()

	ollamaSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer ollamaSrv.Close()

	openrouterSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[]}`))
	}))
	defer openrouterSrv.Close()

	_, err := Sync(context.Background(), Options{
		OpenCodeModelsURL:   opencodeSrv.URL,
		OllamaModelsURL:     ollamaSrv.URL,
		OpenRouterModelsURL: openrouterSrv.URL,
		Now:                 func() time.Time { return time.Unix(100, 0) },
	})
	if err != nil {
		t.Fatalf("sync: %v", err)
	}

	got, ok := config.LookupModel("glm-5.2")
	if !ok {
		t.Fatal("model missing after sync")
	}
	// Ollama membership must be preserved despite the fetch failure.
	hasOllama := false
	for _, u := range got.Upstreams {
		if u == config.UpstreamOllama {
			hasOllama = true
			break
		}
	}
	if !hasOllama {
		t.Fatalf("Ollama membership lost after failed fetch: upstreams=%v", got.Upstreams)
	}
	if len(got.Upstreams) != 2 {
		t.Fatalf("expected 2 upstreams (preserved), got %v", got.Upstreams)
	}
}

// TestSyncGoFailurePreservesExistingGoMembership verifies that when
// the Go catalog fetch fails, existing routes with Go in their
// upstreams do NOT lose the Go membership.
func TestSyncGoFailurePreservesExistingGoMembership(t *testing.T) {
	if err := store.InitForTest("file:sync_go_fail?mode=memory&cache=shared"); err != nil {
		t.Fatalf("init test db: %v", err)
	}
	config.ReplaceModels(nil)

	// Route has both Go and Ollama (set by a previous sync, not customized).
	existing := config.ModelRoute{
		ID:        "glm-5.2",
		Upstream:  config.UpstreamGo,
		Upstreams: []config.Upstream{config.UpstreamGo, config.UpstreamOllama},
		Protocol:  config.ProtocolChat,
		RealModel: "glm-5.2",
		Group:     "go",
		Status:    config.ModelStatusPtr(config.ModelStatusEnabled),
	}
	row := store.NewModelRouteRow(existing)
	if err := store.SaveModelRoute(&row); err != nil {
		t.Fatalf("save existing route: %v", err)
	}

	// Go fails, Ollama succeeds.
	opencodeSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer opencodeSrv.Close()

	ollamaSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"models":[{"name":"glm-5.2"}]}`))
	}))
	defer ollamaSrv.Close()

	openrouterSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[]}`))
	}))
	defer openrouterSrv.Close()

	_, err := Sync(context.Background(), Options{
		OpenCodeModelsURL:   opencodeSrv.URL,
		OllamaModelsURL:     ollamaSrv.URL,
		OpenRouterModelsURL: openrouterSrv.URL,
		Now:                 func() time.Time { return time.Unix(100, 0) },
	})
	if err != nil {
		t.Fatalf("sync: %v", err)
	}

	got, ok := config.LookupModel("glm-5.2")
	if !ok {
		t.Fatal("model missing after sync")
	}
	// Go membership must be preserved despite the fetch failure.
	hasGo := false
	for _, u := range got.Upstreams {
		if u == config.UpstreamGo {
			hasGo = true
			break
		}
	}
	if !hasGo {
		t.Fatalf("Go membership lost after failed fetch: upstreams=%v", got.Upstreams)
	}
	if len(got.Upstreams) != 2 {
		t.Fatalf("expected 2 upstreams (preserved), got %v", got.Upstreams)
	}
}

// TestSyncSuccessfulEmptySourceRemovesMembership verifies that when a
// provider's catalog fetch succeeds but returns empty, the provider is
// removed from existing routes' upstreams.
func TestSyncSuccessfulEmptySourceRemovesMembership(t *testing.T) {
	if err := store.InitForTest("file:sync_empty_remove?mode=memory&cache=shared"); err != nil {
		t.Fatalf("init test db: %v", err)
	}
	config.ReplaceModels(nil)

	// Route has both Go and Ollama (set by a previous sync, not customized).
	existing := config.ModelRoute{
		ID:        "glm-5.2",
		Upstream:  config.UpstreamGo,
		Upstreams: []config.Upstream{config.UpstreamGo, config.UpstreamOllama},
		Protocol:  config.ProtocolChat,
		RealModel: "glm-5.2",
		Group:     "go",
		Status:    config.ModelStatusPtr(config.ModelStatusEnabled),
	}
	row := store.NewModelRouteRow(existing)
	if err := store.SaveModelRoute(&row); err != nil {
		t.Fatalf("save existing route: %v", err)
	}

	// Both succeed, but Ollama no longer serves this model.
	opencodeSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"glm-5.2"}]}`))
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

	_, err := Sync(context.Background(), Options{
		OpenCodeModelsURL:   opencodeSrv.URL,
		OllamaModelsURL:     ollamaSrv.URL,
		OpenRouterModelsURL: openrouterSrv.URL,
		Now:                 func() time.Time { return time.Unix(100, 0) },
	})
	if err != nil {
		t.Fatalf("sync: %v", err)
	}

	got, ok := config.LookupModel("glm-5.2")
	if !ok {
		t.Fatal("model missing after sync")
	}
	// Ollama must be removed since its fetch succeeded with empty results.
	if len(got.Upstreams) != 1 || got.Upstreams[0] != config.UpstreamGo {
		t.Fatalf("expected only go upstream after authoritative empty result, got %v", got.Upstreams)
	}
}
