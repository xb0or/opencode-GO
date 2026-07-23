package config

import (
	"encoding/json"
	"testing"
)

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

func TestOpenRouterModelIgnoresStructuredPricingExtensions(t *testing.T) {
	var model OpenRouterModel
	err := json.Unmarshal([]byte(`{
		"id":"vendor/model",
		"name":"Model",
		"pricing":{
			"prompt":"0.000001",
			"completion":0.000002,
			"overrides":[{"min_prompt_tokens":272000,"prompt":"0.000003"}],
			"internal":null
		}
	}`), &model)
	if err != nil {
		t.Fatalf("unmarshal OpenRouter model: %v", err)
	}
	if model.Pricing["prompt"] != "0.000001" || model.Pricing["completion"] != "0.000002" {
		t.Fatalf("scalar pricing not preserved: %#v", model.Pricing)
	}
	if _, ok := model.Pricing["overrides"]; ok {
		t.Fatalf("structured pricing extension should be ignored: %#v", model.Pricing)
	}
	if _, ok := model.Pricing["internal"]; ok {
		t.Fatalf("null pricing extension should be ignored: %#v", model.Pricing)
	}
}

func TestParseGroupMultipliers(t *testing.T) {
	jsonMultipliers := parseGroupMultipliers(`{"go":0.8,"default":1}`)
	if jsonMultipliers["go"] != 0.8 || jsonMultipliers["default"] != 1 {
		t.Fatalf("unexpected JSON multipliers: %#v", jsonMultipliers)
	}

	listMultipliers := parseGroupMultipliers("go=0.7,default:1.2,broken")
	if listMultipliers["go"] != 0.7 || listMultipliers["default"] != 1.2 {
		t.Fatalf("unexpected list multipliers: %#v", listMultipliers)
	}
}

// TestR4_G1_ResolveUpstreamGroup_TargetsPriority verifies that
// Targets[upstream].Group takes the highest priority and overrides both
// UpstreamGroups and the legacy route-level Group.
func TestR4_G1_ResolveUpstreamGroup_TargetsPriority(t *testing.T) {
	route := ModelRoute{
		ID:       "priority-model",
		Upstream: UpstreamOllama,
		Group:    "legacy",
		UpstreamGroups: map[Upstream]string{
			UpstreamOllama: "standard",
		},
		Targets: map[Upstream]UpstreamTarget{
			UpstreamOllama: {Group: "premium"},
		},
	}
	if got := route.ResolveUpstreamGroup(UpstreamOllama); got != "premium" {
		t.Fatalf("ResolveUpstreamGroup(ollama) = %q, want %q (Targets priority)", got, "premium")
	}
}

// TestR4_G1_ResolveUpstreamGroup_UpstreamGroupsFallback verifies that when
// no Targets entry exists for the upstream, UpstreamGroups is used instead
// of the legacy route-level Group.
func TestR4_G1_ResolveUpstreamGroup_UpstreamGroupsFallback(t *testing.T) {
	route := ModelRoute{
		ID:       "fallback-model",
		Upstream: UpstreamOllama,
		Group:    "legacy",
		UpstreamGroups: map[Upstream]string{
			UpstreamOllama: "standard",
		},
		// no Targets
	}
	if got := route.ResolveUpstreamGroup(UpstreamOllama); got != "standard" {
		t.Fatalf("ResolveUpstreamGroup(ollama) = %q, want %q (UpstreamGroups fallback)", got, "standard")
	}
}

// TestR4_G1_ResolveUpstreamGroup_LegacyGroupFallback verifies that the
// route-level Group is used only when the upstream matches route.Upstream
// (legacy backward-compat). For a non-matching upstream, the default is used.
func TestR4_G1_ResolveUpstreamGroup_LegacyGroupFallback(t *testing.T) {
	route := ModelRoute{
		ID:       "legacy-model",
		Upstream: UpstreamOllama,
		Group:    "premium",
		// no Targets, no UpstreamGroups
	}
	if got := route.ResolveUpstreamGroup(UpstreamOllama); got != "premium" {
		t.Fatalf("ResolveUpstreamGroup(ollama) = %q, want %q (legacy Group, upstream matches)", got, "premium")
	}
	// go != route.Upstream (ollama), so legacy Group must NOT apply.
	if got := route.ResolveUpstreamGroup(UpstreamGo); got != "go" {
		t.Fatalf("ResolveUpstreamGroup(go) = %q, want %q (default, upstream mismatch)", got, "go")
	}
}

// TestR4_G1_ResolveUpstreamGroup_Default verifies that when no Targets,
// no UpstreamGroups, and no route-level Group are set, the upstream name
// itself is used as the group.
func TestR4_G1_ResolveUpstreamGroup_Default(t *testing.T) {
	route := ModelRoute{
		ID:       "default-model",
		Upstream: UpstreamOllama,
		// no Targets, no UpstreamGroups, Group intentionally empty
	}
	if got := route.ResolveUpstreamGroup(UpstreamOllama); got != "ollama" {
		t.Fatalf("ResolveUpstreamGroup(ollama) = %q, want %q (default)", got, "ollama")
	}
}
