package config

import "testing"

func TestMatchOpenRouterModelUsesSlugSuffix(t *testing.T) {
	candidates := []openRouterModel{
		{ID: "other/provider-model", CanonicalSlug: "other/provider-model", Name: "Provider Model"},
		{ID: "moonshotai/kimi-k2.7-code", CanonicalSlug: "moonshotai/kimi-k2.7-code", Name: "MoonshotAI: Kimi K2.7 Code"},
	}

	got, matchedBy, ok := matchOpenRouterModel(ModelRoute{
		ID:        "kimi-k2.7-code",
		Name:      "Kimi K2.7 Code",
		RealModel: "kimi-k2.7-code",
	}, candidates)

	if !ok {
		t.Fatal("expected match")
	}
	if got.ID != "moonshotai/kimi-k2.7-code" {
		t.Fatalf("matched ID = %q, want moonshotai/kimi-k2.7-code", got.ID)
	}
	if matchedBy != "id" {
		t.Fatalf("matchedBy = %q, want id", matchedBy)
	}
}

func TestApplyOpenRouterMetadataCopiesCatalogFields(t *testing.T) {
	route := ModelRoute{ID: "glm-5.1", Name: "GLM-5.1", ContextLen: 0}
	applyOpenRouterMetadata(&route, openRouterModel{
		ID:            "z-ai/glm-5.1",
		Name:          "Z.ai: GLM 5.1",
		Description:   "test description",
		ContextLength: 202752,
		Architecture: &ModelArchitecture{
			InputModalities:  []string{"text"},
			OutputModalities: []string{"text"},
			Tokenizer:        "GLM",
		},
		Pricing: map[string]string{
			"prompt":           "0.00000098",
			"completion":       "0.00000308",
			"input_cache_read": "0.000000182",
		},
		SupportedParameters: []string{"tools", "temperature", "structured_outputs"},
		KnowledgeCutoff:     "2026-01",
	}, "id")

	if route.OpenRouterID != "z-ai/glm-5.1" {
		t.Fatalf("OpenRouterID = %q, want z-ai/glm-5.1", route.OpenRouterID)
	}
	if route.ContextLen != 202752 {
		t.Fatalf("ContextLen = %d, want 202752", route.ContextLen)
	}
	if route.Pricing["prompt"] != "0.00000098" {
		t.Fatalf("prompt pricing = %q", route.Pricing["prompt"])
	}
	if len(route.SupportedParameters) != 3 || route.SupportedParameters[0] != "structured_outputs" {
		t.Fatalf("SupportedParameters not sorted/copied: %#v", route.SupportedParameters)
	}
}
