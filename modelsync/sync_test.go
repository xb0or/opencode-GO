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
		ID:               "glm-5.2",
		Name:             "GLM 5.2 (Ollama)",
		Upstream:         config.UpstreamOllama,
		Upstreams:        []config.Upstream{config.UpstreamOllama, config.UpstreamGo},
		Protocol:         config.ProtocolChat,
		RealModel:        "glm-5.2",
		Group:            "ollama",
		Status:           config.ModelStatusPtr(config.ModelStatusEnabled),
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
		ID:               "glm-5.2",
		Name:             "Custom GLM",
		Upstream:         config.UpstreamOllama,
		Upstreams:        []config.Upstream{config.UpstreamOllama},
		Protocol:         config.ProtocolChat,
		RealModel:        "glm-5.2",
		Group:            "ollama",
		Status:           config.ModelStatusPtr(config.ModelStatusEnabled),
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

// ---------------------------------------------------------------------------
// P1-3: sourceModel stores full per-Upstream Targets (RealModel + Protocol +
// Group), not just Protocol. buildMergedRoute must use the per-upstream
// RealModel from perUpstreamTargets instead of hardcoding sm.ID.
// ---------------------------------------------------------------------------

// TestP1_3_BuildMergedRouteUsesPerUpstreamRealModel verifies that when two
// upstreams serve the same gateway model ID with DIFFERENT real_model names
// and protocols, buildMergedRoute produces Targets carrying each upstream's
// own real_model and protocol.
func TestP1_3_BuildMergedRouteUsesPerUpstreamRealModel(t *testing.T) {
	sm := sourceModel{
		ID:       "qwen-x",
		Name:     "qwen-x",
		Upstream: config.UpstreamGo,
		Upstreams: []config.Upstream{
			config.UpstreamGo,
			config.UpstreamOllama,
		},
		perUpstreamTargets: map[config.Upstream]config.UpstreamTarget{
			config.UpstreamGo: {
				RealModel: "qwen-x",
				Protocol:  config.ProtocolMessages,
				Group:     string(config.UpstreamGo),
			},
			config.UpstreamOllama: {
				RealModel: "qwen-x:cloud",
				Protocol:  config.ProtocolChat,
				Group:     string(config.UpstreamOllama),
			},
		},
	}

	route := buildMergedRoute(sm, store.ModelRouteRow{}, false, true, true)

	if len(route.Targets) != 2 {
		t.Fatalf("expected 2 Targets, got %d (%v)", len(route.Targets), route.Targets)
	}
	goTarget, ok := route.Targets[config.UpstreamGo]
	if !ok {
		t.Fatal("missing Go target")
	}
	if goTarget.RealModel != "qwen-x" {
		t.Errorf("Go RealModel = %q, want %q", goTarget.RealModel, "qwen-x")
	}
	if goTarget.Protocol != config.ProtocolMessages {
		t.Errorf("Go Protocol = %q, want %q", goTarget.Protocol, config.ProtocolMessages)
	}
	ollamaTarget, ok := route.Targets[config.UpstreamOllama]
	if !ok {
		t.Fatal("missing Ollama target")
	}
	if ollamaTarget.RealModel != "qwen-x:cloud" {
		t.Errorf("Ollama RealModel = %q, want %q", ollamaTarget.RealModel, "qwen-x:cloud")
	}
	if ollamaTarget.Protocol != config.ProtocolChat {
		t.Errorf("Ollama Protocol = %q, want %q", ollamaTarget.Protocol, config.ProtocolChat)
	}
}

func TestBuildMergedRouteKeepsPerUpstreamKeyGroupsWhenTargetsOtherwiseMatch(t *testing.T) {
	sm := sourceModel{
		ID:        "shared-model",
		Name:      "Shared Model",
		Upstream:  config.UpstreamGo,
		Upstreams: []config.Upstream{config.UpstreamGo, config.UpstreamOllama},
		perUpstreamTargets: map[config.Upstream]config.UpstreamTarget{
			config.UpstreamGo: {
				RealModel: "shared-model",
				Protocol:  config.ProtocolChat,
				Group:     "go",
			},
			config.UpstreamOllama: {
				RealModel: "shared-model",
				Protocol:  config.ProtocolChat,
				Group:     "ollama",
			},
		},
	}
	route := buildMergedRoute(sm, store.ModelRouteRow{}, false, true, true)
	if len(route.Targets) != 2 {
		t.Fatalf("key-group difference must produce per-upstream targets: %#v", route.Targets)
	}
	if got := route.ResolveUpstreamGroup(config.UpstreamOllama); got != "ollama" {
		t.Fatalf("Ollama group = %q, want ollama", got)
	}
}

// TestP1_3_SyncGeneratesPerUpstreamTargetsForDifferentProtocols verifies
// through the full Sync flow that when Go and Ollama both report the same
// model ID but with different native protocols, the resulting route has
// Targets with each upstream's real_model and protocol.
func TestP1_3_SyncGeneratesPerUpstreamTargetsForDifferentProtocols(t *testing.T) {
	if err := store.InitForTest("file:p13_sync_targets?mode=memory&cache=shared"); err != nil {
		t.Fatalf("init test db: %v", err)
	}
	config.ReplaceModels(nil)

	// Go reports a model with id "qwen-x" — inferProtocol maps "qwen*"
	// prefixes to Messages, guaranteeing protocol divergence from
	// Ollama's Chat.
	opencodeSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"qwen-x","name":"Qwen X"}]}`))
	}))
	defer opencodeSrv.Close()

	ollamaSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"models":[{"name":"qwen-x"}]}`))
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

	got, ok := config.LookupModel("qwen-x")
	if !ok {
		t.Fatal("model missing after sync")
	}
	if len(got.Upstreams) != 2 {
		t.Fatalf("expected 2 upstreams, got %v", got.Upstreams)
	}
	// Targets must be populated because Go and Ollama use different protocols
	// (Go infers messages for qwen*; Ollama is always chat).
	if len(got.Targets) != 2 {
		t.Fatalf("expected 2 Targets (protocols differ), got %d", len(got.Targets))
	}
	goT, ok := got.Targets[config.UpstreamGo]
	if !ok {
		t.Fatal("missing Go target")
	}
	if goT.RealModel != "qwen-x" {
		t.Errorf("Go RealModel = %q, want %q", goT.RealModel, "qwen-x")
	}
	ollamaT, ok := got.Targets[config.UpstreamOllama]
	if !ok {
		t.Fatal("missing Ollama target")
	}
	if ollamaT.RealModel != "qwen-x" {
		t.Errorf("Ollama RealModel = %q, want %q", ollamaT.RealModel, "qwen-x")
	}
	if ollamaT.Protocol != config.ProtocolChat {
		t.Errorf("Ollama Protocol = %q, want %q", ollamaT.Protocol, config.ProtocolChat)
	}
	// The two upstreams must have different protocols (otherwise Targets
	// wouldn't be needed).
	if goT.Protocol == ollamaT.Protocol {
		t.Fatalf("expected different protocols, both = %q", goT.Protocol)
	}
}

// TestP1_3_SyncPreservesExistingPerUpstreamTargets verifies that when an
// admin has customized per-upstream Targets (e.g. different real_model for
// Ollama), a subsequent sync does NOT overwrite the customized real_model
// back to sm.ID.
func TestP1_3_SyncPreservesExistingPerUpstreamTargets(t *testing.T) {
	if err := store.InitForTest("file:p13_preserve?mode=memory&cache=shared"); err != nil {
		t.Fatalf("init test db: %v", err)
	}
	config.ReplaceModels(nil)

	// Pre-existing route with admin-customized per-upstream Targets where
	// Ollama uses a different real_model than the gateway ID.
	existing := config.ModelRoute{
		ID:        "shared-model",
		Name:      "Shared Model",
		Upstream:  config.UpstreamGo,
		Upstreams: []config.Upstream{config.UpstreamGo, config.UpstreamOllama},
		Protocol:  config.ProtocolMessages,
		RealModel: "shared-model",
		Group:     "go",
		Status:    config.ModelStatusPtr(config.ModelStatusEnabled),
		Targets: map[config.Upstream]config.UpstreamTarget{
			config.UpstreamGo: {
				RealModel: "shared-model",
				Protocol:  config.ProtocolMessages,
				Group:     "go",
			},
			config.UpstreamOllama: {
				RealModel: "shared-model:cloud",
				Protocol:  config.ProtocolChat,
				Group:     "ollama",
			},
		},
		CustomizedFields: []string{"targets"},
		IsCustomized:     true,
	}
	row := store.NewModelRouteRow(existing)
	if err := store.SaveModelRoute(&row); err != nil {
		t.Fatalf("save existing row: %v", err)
	}

	opencodeSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"shared-model","name":"Shared Model"}]}`))
	}))
	defer opencodeSrv.Close()

	ollamaSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"models":[{"name":"shared-model"}]}`))
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

	got, ok := config.LookupModel("shared-model")
	if !ok {
		t.Fatal("model missing after sync")
	}
	ollamaT, ok := got.Targets[config.UpstreamOllama]
	if !ok {
		t.Fatal("missing Ollama target after sync")
	}
	// The admin-customized real_model "shared-model:cloud" must be preserved,
	// not overwritten to "shared-model" by the sync.
	if ollamaT.RealModel != "shared-model:cloud" {
		t.Errorf("Ollama RealModel = %q, want preserved %q", ollamaT.RealModel, "shared-model:cloud")
	}
}

// ---------------------------------------------------------------------------
// G2: Sync model reconciliation — disable models removed from all providers.
// ---------------------------------------------------------------------------

// seedRouteForG2 saves a model route row to the test DB and returns a cleanup
// helper. Used by the G2 reconciliation tests.
func seedRouteForG2(t *testing.T, route config.ModelRoute) {
	t.Helper()
	row := store.NewModelRouteRow(route)
	if err := store.SaveModelRoute(&row); err != nil {
		t.Fatalf("seed route %s: %v", route.ID, err)
	}
}

// emptyOpenRouterServer returns an httptest server that returns an empty
// OpenRouter model list.
func emptyOpenRouterServer() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[]}`))
	}))
}

// failingServer returns an httptest server that always responds HTTP 502,
// simulating a failed catalog fetch.
func failingServer() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
}

// jsonServer returns an httptest server that responds with the given body.
func jsonServer(body string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
}

// TestR4_G2_BothProvidersModelRemoved_Disabled verifies that a DB model not
// present in either the Go or Ollama catalog (both fetches succeeding) is
// disabled and its upstreams cleared.
func TestR4_G2_BothProvidersModelRemoved_Disabled(t *testing.T) {
	if err := store.InitForTest("file:g2_both_removed?mode=memory&cache=shared"); err != nil {
		t.Fatalf("init test db: %v", err)
	}
	config.ReplaceModels(nil)

	seedRouteForG2(t, config.ModelRoute{
		ID:        "legacy-model",
		Name:      "Legacy Model",
		Upstream:  config.UpstreamGo,
		Upstreams: []config.Upstream{config.UpstreamGo, config.UpstreamOllama},
		Protocol:  config.ProtocolChat,
		RealModel: "legacy-model",
		Group:     "go",
		Status:    config.ModelStatusPtr(config.ModelStatusEnabled),
	})

	// Both providers succeed but return only other models, not legacy-model.
	opencodeSrv := jsonServer(`{"object":"list","data":[{"id":"other-go"}]}`)
	defer opencodeSrv.Close()
	ollamaSrv := jsonServer(`{"models":[{"name":"other-ollama"}]}`)
	defer ollamaSrv.Close()
	orSrv := emptyOpenRouterServer()
	defer orSrv.Close()

	result, err := Sync(context.Background(), Options{
		OpenCodeModelsURL:   opencodeSrv.URL,
		OllamaModelsURL:     ollamaSrv.URL,
		OpenRouterModelsURL: orSrv.URL,
		Now:                 func() time.Time { return time.Unix(100, 0) },
	})
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	if result.DisabledCount != 1 {
		t.Fatalf("expected DisabledCount=1, got %d (%#v)", result.DisabledCount, result)
	}

	got, ok := config.LookupModel("legacy-model")
	if !ok {
		t.Fatal("legacy model missing from runtime config")
	}
	if got.IsEnabled() {
		t.Fatalf("legacy model should be disabled, status=%v", got.Status)
	}
	// Verify the DB row has empty upstreams (the runtime layer re-populates
	// Upstreams from Upstream via applyLocalModelDefaults, so we check the
	// persisted row directly).
	rows, err := store.LoadModelRoutes()
	if err != nil {
		t.Fatalf("load rows: %v", err)
	}
	for _, r := range rows {
		if r.ID == "legacy-model" {
			if r.UpstreamsJSON != "" {
				t.Fatalf("legacy model DB upstreams should be empty, got %q", r.UpstreamsJSON)
			}
			if r.Status != config.ModelStatusDisabled {
				t.Fatalf("legacy model DB status should be disabled(0), got %d", r.Status)
			}
		}
	}
}

// TestR4_G2_GoFetchFails_PreserveOllama verifies that when the Go fetch fails,
// Go membership is preserved even though the model is not in any catalog; only
// Ollama (which succeeded but doesn't list the model) is removed.
func TestR4_G2_GoFetchFails_PreserveOllama(t *testing.T) {
	if err := store.InitForTest("file:g2_go_fail?mode=memory&cache=shared"); err != nil {
		t.Fatalf("init test db: %v", err)
	}
	config.ReplaceModels(nil)

	seedRouteForG2(t, config.ModelRoute{
		ID:        "legacy-model",
		Name:      "Legacy Model",
		Upstream:  config.UpstreamGo,
		Upstreams: []config.Upstream{config.UpstreamGo, config.UpstreamOllama},
		Protocol:  config.ProtocolChat,
		RealModel: "legacy-model",
		Group:     "go",
		Status:    config.ModelStatusPtr(config.ModelStatusEnabled),
	})

	// Go fetch fails; Ollama succeeds but doesn't list legacy-model.
	opencodeSrv := failingServer()
	defer opencodeSrv.Close()
	ollamaSrv := jsonServer(`{"models":[{"name":"other-ollama"}]}`)
	defer ollamaSrv.Close()
	orSrv := emptyOpenRouterServer()
	defer orSrv.Close()

	_, err := Sync(context.Background(), Options{
		OpenCodeModelsURL:   opencodeSrv.URL,
		OllamaModelsURL:     ollamaSrv.URL,
		OpenRouterModelsURL: orSrv.URL,
		Now:                 func() time.Time { return time.Unix(100, 0) },
	})
	if err != nil {
		t.Fatalf("sync: %v", err)
	}

	got, ok := config.LookupModel("legacy-model")
	if !ok {
		t.Fatal("legacy model missing from runtime config")
	}
	// Go preserved (fetch failed), Ollama removed → upstreams=[go].
	if len(got.Upstreams) != 1 || got.Upstreams[0] != config.UpstreamGo {
		t.Fatalf("expected upstreams=[go], got %v", got.Upstreams)
	}
	// Still enabled because at least one upstream remains.
	if !got.IsEnabled() {
		t.Fatalf("legacy model should remain enabled, status=%v", got.Status)
	}
}

// TestR4_G2_BothFetchFail_PreserveAll verifies that when both fetches fail, an
// absent-from-catalog model is left entirely untouched.
func TestR4_G2_BothFetchFail_PreserveAll(t *testing.T) {
	if err := store.InitForTest("file:g2_both_fail?mode=memory&cache=shared"); err != nil {
		t.Fatalf("init test db: %v", err)
	}
	config.ReplaceModels(nil)

	seedRouteForG2(t, config.ModelRoute{
		ID:        "legacy-model",
		Name:      "Legacy Model",
		Upstream:  config.UpstreamGo,
		Upstreams: []config.Upstream{config.UpstreamGo, config.UpstreamOllama},
		Protocol:  config.ProtocolChat,
		RealModel: "legacy-model",
		Group:     "go",
		Status:    config.ModelStatusPtr(config.ModelStatusEnabled),
	})

	opencodeSrv := failingServer()
	defer opencodeSrv.Close()
	ollamaSrv := failingServer()
	defer ollamaSrv.Close()
	orSrv := emptyOpenRouterServer()
	defer orSrv.Close()

	result, err := Sync(context.Background(), Options{
		OpenCodeModelsURL:   opencodeSrv.URL,
		OllamaModelsURL:     ollamaSrv.URL,
		OpenRouterModelsURL: orSrv.URL,
		Now:                 func() time.Time { return time.Unix(100, 0) },
	})
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	if result.DisabledCount != 0 {
		t.Fatalf("expected DisabledCount=0, got %d", result.DisabledCount)
	}

	got, ok := config.LookupModel("legacy-model")
	if !ok {
		t.Fatal("legacy model missing from runtime config")
	}
	if !got.IsEnabled() {
		t.Fatalf("legacy model should remain enabled, status=%v", got.Status)
	}
	if len(got.Upstreams) != 2 {
		t.Fatalf("legacy model upstreams should be unchanged, got %v", got.Upstreams)
	}
}

// TestR4_G2_AdminCustomizedUpstreams_Preserved verifies that when an admin has
// customized the upstreams field, reconciliation does not modify upstreams and
// therefore does not disable the route (upstreams remain non-empty).
func TestR4_G2_AdminCustomizedUpstreams_Preserved(t *testing.T) {
	if err := store.InitForTest("file:g2_custom_upstreams?mode=memory&cache=shared"); err != nil {
		t.Fatalf("init test db: %v", err)
	}
	config.ReplaceModels(nil)

	seedRouteForG2(t, config.ModelRoute{
		ID:               "legacy-model",
		Name:             "Legacy Model",
		Upstream:         config.UpstreamGo,
		Upstreams:        []config.Upstream{config.UpstreamGo, config.UpstreamOllama},
		Protocol:         config.ProtocolChat,
		RealModel:        "legacy-model",
		Group:            "go",
		Status:           config.ModelStatusPtr(config.ModelStatusEnabled),
		IsCustomized:     true,
		CustomizedFields: []string{"upstreams"},
	})

	// Both providers succeed but don't list legacy-model.
	opencodeSrv := jsonServer(`{"object":"list","data":[{"id":"other-go"}]}`)
	defer opencodeSrv.Close()
	ollamaSrv := jsonServer(`{"models":[{"name":"other-ollama"}]}`)
	defer ollamaSrv.Close()
	orSrv := emptyOpenRouterServer()
	defer orSrv.Close()

	result, err := Sync(context.Background(), Options{
		OpenCodeModelsURL:   opencodeSrv.URL,
		OllamaModelsURL:     ollamaSrv.URL,
		OpenRouterModelsURL: orSrv.URL,
		Now:                 func() time.Time { return time.Unix(100, 0) },
	})
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	if result.DisabledCount != 0 {
		t.Fatalf("expected DisabledCount=0, got %d", result.DisabledCount)
	}

	got, ok := config.LookupModel("legacy-model")
	if !ok {
		t.Fatal("legacy model missing from runtime config")
	}
	// Upstreams preserved (admin-customized).
	if len(got.Upstreams) != 2 {
		t.Fatalf("admin-customized upstreams should be preserved, got %v", got.Upstreams)
	}
	// Status NOT disabled because upstreams remain non-empty.
	if !got.IsEnabled() {
		t.Fatalf("legacy model should remain enabled (upstreams non-empty), status=%v", got.Status)
	}
}

// TestR4_G2_AdminCustomizedStatus_Preserved verifies that when an admin has
// customized the status field, reconciliation does not disable the route even
// when all upstreams are removed.
func TestR4_G2_AdminCustomizedStatus_Preserved(t *testing.T) {
	if err := store.InitForTest("file:g2_custom_status?mode=memory&cache=shared"); err != nil {
		t.Fatalf("init test db: %v", err)
	}
	config.ReplaceModels(nil)

	seedRouteForG2(t, config.ModelRoute{
		ID:               "legacy-model",
		Name:             "Legacy Model",
		Upstream:         config.UpstreamGo,
		Upstreams:        []config.Upstream{config.UpstreamGo, config.UpstreamOllama},
		Protocol:         config.ProtocolChat,
		RealModel:        "legacy-model",
		Group:            "go",
		Status:           config.ModelStatusPtr(config.ModelStatusEnabled),
		IsCustomized:     true,
		CustomizedFields: []string{"status"},
	})

	// Both providers succeed but don't list legacy-model → upstreams cleared,
	// but status is admin-customized so must remain enabled.
	opencodeSrv := jsonServer(`{"object":"list","data":[{"id":"other-go"}]}`)
	defer opencodeSrv.Close()
	ollamaSrv := jsonServer(`{"models":[{"name":"other-ollama"}]}`)
	defer ollamaSrv.Close()
	orSrv := emptyOpenRouterServer()
	defer orSrv.Close()

	result, err := Sync(context.Background(), Options{
		OpenCodeModelsURL:   opencodeSrv.URL,
		OllamaModelsURL:     ollamaSrv.URL,
		OpenRouterModelsURL: orSrv.URL,
		Now:                 func() time.Time { return time.Unix(100, 0) },
	})
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	// DisabledCount should not increment because status was admin-preserved.
	if result.DisabledCount != 0 {
		t.Fatalf("expected DisabledCount=0, got %d", result.DisabledCount)
	}

	got, ok := config.LookupModel("legacy-model")
	if !ok {
		t.Fatal("legacy model missing from runtime config")
	}
	// Upstreams cleared in the DB (not customized). The runtime layer
	// re-populates Upstreams from Upstream, so verify the persisted row.
	rows, err := store.LoadModelRoutes()
	if err != nil {
		t.Fatalf("load rows: %v", err)
	}
	for _, r := range rows {
		if r.ID == "legacy-model" {
			if r.UpstreamsJSON != "" {
				t.Fatalf("upstreams should be cleared in DB, got %q", r.UpstreamsJSON)
			}
		}
	}
	// Status preserved as enabled (admin-customized).
	if !got.IsEnabled() {
		t.Fatalf("admin-customized status should be preserved (enabled), got %v", got.Status)
	}
}

// TestR4_G2_DisabledModelNotInPublicList verifies that after a model is
// disabled by reconciliation, it appears in AllModels() but NOT in
// AllEnabledModels(), and IsEnabled() reports false.
func TestR4_G2_DisabledModelNotInPublicList(t *testing.T) {
	if err := store.InitForTest("file:g2_public_list?mode=memory&cache=shared"); err != nil {
		t.Fatalf("init test db: %v", err)
	}
	config.ReplaceModels(nil)

	seedRouteForG2(t, config.ModelRoute{
		ID:        "legacy-model",
		Name:      "Legacy Model",
		Upstream:  config.UpstreamGo,
		Upstreams: []config.Upstream{config.UpstreamGo, config.UpstreamOllama},
		Protocol:  config.ProtocolChat,
		RealModel: "legacy-model",
		Group:     "go",
		Status:    config.ModelStatusPtr(config.ModelStatusEnabled),
	})

	opencodeSrv := jsonServer(`{"object":"list","data":[{"id":"alive-model"}]}`)
	defer opencodeSrv.Close()
	ollamaSrv := jsonServer(`{"models":[{"name":"alive-model"}]}`)
	defer ollamaSrv.Close()
	orSrv := emptyOpenRouterServer()
	defer orSrv.Close()

	if _, err := Sync(context.Background(), Options{
		OpenCodeModelsURL:   opencodeSrv.URL,
		OllamaModelsURL:     ollamaSrv.URL,
		OpenRouterModelsURL: orSrv.URL,
		Now:                 func() time.Time { return time.Unix(100, 0) },
	}); err != nil {
		t.Fatalf("sync: %v", err)
	}

	// The disabled model should be present in AllModels() with IsEnabled()==false.
	foundDisabled := false
	for _, m := range config.AllModels() {
		if m.ID == "legacy-model" {
			if m.IsEnabled() {
				t.Fatalf("legacy-model should be disabled in AllModels(), status=%v", m.Status)
			}
			foundDisabled = true
			break
		}
	}
	if !foundDisabled {
		t.Fatal("legacy-model not found in AllModels()")
	}

	// The disabled model should NOT appear in AllEnabledModels().
	for _, m := range config.AllEnabledModels() {
		if m.ID == "legacy-model" {
			t.Fatalf("legacy-model should not appear in AllEnabledModels(): %#v", m)
		}
	}
	// Sanity: alive-model should be in AllEnabledModels().
	foundAlive := false
	for _, m := range config.AllEnabledModels() {
		if m.ID == "alive-model" {
			foundAlive = true
			break
		}
	}
	if !foundAlive {
		t.Fatal("alive-model missing from AllEnabledModels()")
	}
}

func TestRebuildAtomicallyReplacesCatalogAndKeepsMetadataSeparate(t *testing.T) {
	if err := store.InitForTest("file:model_rebuild_success?mode=memory&cache=shared"); err != nil {
		t.Fatalf("init test db: %v", err)
	}
	old := config.ModelRoute{
		ID:               "manual-old",
		Name:             "Manual Old",
		Upstream:         config.UpstreamGo,
		Upstreams:        []config.Upstream{config.UpstreamGo},
		Protocol:         config.ProtocolChat,
		RealModel:        "manual-old",
		Group:            "go",
		Status:           config.ModelStatusPtr(config.ModelStatusEnabled),
		IsCustomized:     true,
		CustomizedFields: []string{"name"},
	}
	seedRouteForG2(t, old)
	config.ReplaceModels([]config.ModelRoute{old})
	defer config.ReplaceModels(nil)

	opencodeSrv := jsonServer(`{"object":"list","data":[{"id":"shared-model","name":"Shared"},{"id":"go-only","name":"Go Only"}]}`)
	defer opencodeSrv.Close()
	ollamaSrv := jsonServer(`{"models":[{"name":"shared-model"},{"name":"ollama-only"}]}`)
	defer ollamaSrv.Close()
	openrouterSrv := jsonServer(`{"data":[{"id":"shared-model","name":"Shared Metadata","context_length":131072,"pricing":{"prompt":"0.000001"}}]}`)
	defer openrouterSrv.Close()

	result, err := Rebuild(context.Background(), Options{
		OpenCodeModelsURL:   opencodeSrv.URL,
		OllamaModelsURL:     ollamaSrv.URL,
		OpenRouterModelsURL: openrouterSrv.URL,
		Now:                 func() time.Time { return time.Unix(200, 0) },
	})
	if err != nil {
		t.Fatalf("rebuild: %v", err)
	}
	if result.DeletedCount != 1 || result.CreatedCount != 3 || result.TotalCount != 3 {
		t.Fatalf("unexpected rebuild counts: %#v", result)
	}
	if result.MatchedCount != 1 {
		t.Fatalf("matched count = %d, want 1", result.MatchedCount)
	}
	if _, ok := config.LookupModel("manual-old"); ok {
		t.Fatal("old manual route should be removed by a full rebuild")
	}
	shared, ok := config.LookupModel("shared-model")
	if !ok {
		t.Fatal("shared model missing after rebuild")
	}
	if len(shared.Upstreams) != 2 || shared.Upstreams[0] != config.UpstreamGo || shared.Upstreams[1] != config.UpstreamOllama {
		t.Fatalf("shared upstreams = %v, want [go ollama]", shared.Upstreams)
	}
	if shared.OpenRouterID != "shared-model" || shared.ContextLen != 131072 {
		t.Fatalf("OpenRouter metadata not applied: %#v", shared)
	}
	for _, upstream := range shared.Upstreams {
		if upstream != config.UpstreamGo && upstream != config.UpstreamOllama {
			t.Fatalf("metadata source leaked into routable upstreams: %v", shared.Upstreams)
		}
	}
	rows, err := store.LoadModelRoutes()
	if err != nil {
		t.Fatalf("load rebuilt rows: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("persisted rebuilt rows = %d, want 3", len(rows))
	}
}

func TestRebuildFetchFailurePreservesExistingCatalog(t *testing.T) {
	if err := store.InitForTest("file:model_rebuild_failure?mode=memory&cache=shared"); err != nil {
		t.Fatalf("init test db: %v", err)
	}
	old := config.ModelRoute{
		ID:        "keep-me",
		Name:      "Keep Me",
		Upstream:  config.UpstreamGo,
		Upstreams: []config.Upstream{config.UpstreamGo},
		Protocol:  config.ProtocolChat,
		RealModel: "keep-me",
		Group:     "go",
		Status:    config.ModelStatusPtr(config.ModelStatusEnabled),
	}
	seedRouteForG2(t, old)
	config.ReplaceModels([]config.ModelRoute{old})
	defer config.ReplaceModels(nil)

	opencodeSrv := jsonServer(`{"object":"list","data":[{"id":"new-model"}]}`)
	defer opencodeSrv.Close()
	ollamaSrv := failingServer()
	defer ollamaSrv.Close()
	openrouterSrv := emptyOpenRouterServer()
	defer openrouterSrv.Close()

	if _, err := Rebuild(context.Background(), Options{
		OpenCodeModelsURL:   opencodeSrv.URL,
		OllamaModelsURL:     ollamaSrv.URL,
		OpenRouterModelsURL: openrouterSrv.URL,
	}); err == nil {
		t.Fatal("rebuild should fail when a routable catalog cannot be fetched")
	}
	if _, ok := config.LookupModel("keep-me"); !ok {
		t.Fatal("runtime catalog changed after failed rebuild")
	}
	rows, err := store.LoadModelRoutes()
	if err != nil {
		t.Fatalf("load rows: %v", err)
	}
	if len(rows) != 1 || rows[0].ID != "keep-me" {
		t.Fatalf("persisted catalog changed after failed rebuild: %#v", rows)
	}
}

func TestSyncReportsUnchangedModelsInsteadOfFalseUpdates(t *testing.T) {
	if err := store.InitForTest("file:model_sync_unchanged_count?mode=memory&cache=shared"); err != nil {
		t.Fatalf("init test db: %v", err)
	}
	config.ReplaceModels(nil)
	defer config.ReplaceModels(nil)

	opencodeSrv := jsonServer(`{"object":"list","data":[{"id":"stable-model","name":"Stable Model"}]}`)
	defer opencodeSrv.Close()
	ollamaSrv := jsonServer(`{"models":[]}`)
	defer ollamaSrv.Close()
	openrouterSrv := jsonServer(`{"data":[{"id":"stable-model","name":"Stable Model","context_length":64000,"pricing":{"prompt":"0.000001"}}]}`)
	defer openrouterSrv.Close()

	options := Options{
		OpenCodeModelsURL:   opencodeSrv.URL,
		OllamaModelsURL:     ollamaSrv.URL,
		OpenRouterModelsURL: openrouterSrv.URL,
		Now:                 func() time.Time { return time.Unix(300, 0) },
	}
	first, err := Sync(context.Background(), options)
	if err != nil {
		t.Fatalf("first sync: %v", err)
	}
	if first.CreatedCount != 1 {
		t.Fatalf("first sync counts: %#v", first)
	}
	options.Now = func() time.Time { return time.Unix(400, 0) }
	second, err := Sync(context.Background(), options)
	if err != nil {
		t.Fatalf("second sync: %v", err)
	}
	if second.UpdatedCount != 0 || second.UnchangedCount != 1 {
		t.Fatalf("identical second sync should be unchanged, got %#v", second)
	}
}
