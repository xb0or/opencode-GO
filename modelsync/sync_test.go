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
