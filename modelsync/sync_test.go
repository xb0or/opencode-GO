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

	result, err := Sync(context.Background(), Options{
		OpenCodeModelsURL:   opencodeSrv.URL,
		OpenRouterModelsURL: openrouterSrv.URL,
		Now:                 func() time.Time { return time.Unix(100, 0) },
	})
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	if result.OpenCodeCount != 2 || result.MatchedCount != 2 || result.CreatedCount != 1 || result.UpdatedCount != 1 {
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

	result, err := Sync(context.Background(), Options{OpenCodeModelsURL: opencodeSrv.URL, OpenRouterModelsURL: openrouterSrv.URL})
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
