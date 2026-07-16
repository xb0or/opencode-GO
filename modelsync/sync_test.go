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

	openrouterSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[
			{"id":"openai/gpt-4o","name":"OpenAI: GPT-4o","context_length":128000,"pricing":{"prompt":"0.000001"},"supported_parameters":["tools","structured_outputs"],"architecture":{"input_modalities":["text","image"]}},
			{"id":"z-ai/glm-5.2","name":"Z.ai: GLM 5.2","context_length":202752,"supported_parameters":["reasoning"],"architecture":{"input_modalities":["text"]}}
		]}`))
	}))
	defer openrouterSrv.Close()

	ollamaSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"models":[{"name":"ollama-test-model"}]}`))
	}))
	defer ollamaSrv.Close()

	result, err := Sync(context.Background(), Options{
		OpenCodeModelsURL:   opencodeSrv.URL,
		OpenRouterModelsURL: openrouterSrv.URL,
		OllamaModelsURL:     ollamaSrv.URL,
		Now:                 func() time.Time { return time.Unix(100, 0) },
	})
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	// OpenCode: 2 models (gpt-4o updated, glm-5.2 created)
	// Ollama: 1 model (ollama-test-model created) — not overlapping with Go
	// Total created = 1 (glm-5.2) + 1 (ollama-test-model) = 2
	// Total updated = 1 (gpt-4o)
	if result.OpenCodeCount != 2 || result.MatchedCount != 2 || result.CreatedCount != 2 || result.UpdatedCount != 1 {
		t.Fatalf("unexpected result: %#v", result)
	}

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

	created, ok := config.LookupModel("glm-5.2")
	if !ok {
		t.Fatal("new opencode model missing from runtime config")
	}
	if !created.IsEnabled() || created.ContextLen != 202752 || created.OpenRouterID != "z-ai/glm-5.2" {
		t.Fatalf("new model not enriched/enabled: %#v", created)
	}
	if len(created.Tags) == 0 {
		t.Fatalf("new model should have derived tags: %#v", created)
	}

	// Verify Ollama model was synced
	ollamaModel, ok := config.LookupModel("ollama-test-model")
	if !ok {
		t.Fatal("ollama model missing from runtime config")
	}
	if ollamaModel.Upstream != config.UpstreamOllama || ollamaModel.Group != "ollama" {
		t.Fatalf("ollama model has wrong upstream/group: %#v", ollamaModel)
	}
	if ollamaModel.Protocol != config.ProtocolChat {
		t.Fatalf("ollama model should use chat protocol: %#v", ollamaModel)
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
	openrouterSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer openrouterSrv.Close()
	ollamaSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"models":[]}`))
	}))
	defer ollamaSrv.Close()

	result, err := Sync(context.Background(), Options{OpenCodeModelsURL: opencodeSrv.URL, OpenRouterModelsURL: openrouterSrv.URL, OllamaModelsURL: ollamaSrv.URL})
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