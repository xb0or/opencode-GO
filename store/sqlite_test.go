package store

import (
	"testing"

	"github.com/xb0or/opencode-GO/config"
)

// TestModelRouteRoundTrip verifies that Upstreams and UpstreamGroups survive
// a full Save → Load → ModelRouteFromRow round trip through SQLite.
func TestModelRouteRoundTrip(t *testing.T) {
	if err := InitForTest("file:store_roundtrip?mode=memory&cache=shared"); err != nil {
		t.Fatalf("init test db: %v", err)
	}

	original := config.ModelRoute{
		ID:        "roundtrip-model",
		Name:      "Round Trip Model",
		Upstream:  config.UpstreamGo,
		Upstreams: []config.Upstream{config.UpstreamGo, config.UpstreamOllama},
		UpstreamGroups: map[config.Upstream]string{
			config.UpstreamGo:    "premium",
			config.UpstreamOllama: "ollama-premium",
		},
		Protocol:  config.ProtocolChat,
		RealModel: "real-roundtrip-model",
		Group:     "premium",
	}

	// Save to DB
	row := NewModelRouteRow(original)
	if err := SaveModelRoute(&row); err != nil {
		t.Fatalf("save model route: %v", err)
	}

	// Load back from DB
	rows, err := LoadModelRoutes()
	if err != nil {
		t.Fatalf("load model routes: %v", err)
	}
	if len(rows) == 0 {
		t.Fatal("no model routes loaded")
	}

	// Find our row
	var foundRow ModelRouteRow
	for _, r := range rows {
		if r.ID == "roundtrip-model" {
			foundRow = r
			break
		}
	}
	if foundRow.ID == "" {
		t.Fatal("roundtrip-model not found in loaded routes")
	}

	// Convert back to runtime config
	restored := ModelRouteFromRow(foundRow)

	// Verify Upstreams survived
	if len(restored.Upstreams) != 2 {
		t.Fatalf("Upstreams length = %d, want 2; got %#v", len(restored.Upstreams), restored.Upstreams)
	}
	if restored.Upstreams[0] != config.UpstreamGo {
		t.Fatalf("Upstreams[0] = %q, want %q", restored.Upstreams[0], config.UpstreamGo)
	}
	if restored.Upstreams[1] != config.UpstreamOllama {
		t.Fatalf("Upstreams[1] = %q, want %q", restored.Upstreams[1], config.UpstreamOllama)
	}

	// Verify UpstreamGroups survived
	if len(restored.UpstreamGroups) != 2 {
		t.Fatalf("UpstreamGroups length = %d, want 2; got %#v", len(restored.UpstreamGroups), restored.UpstreamGroups)
	}
	if restored.UpstreamGroups[config.UpstreamGo] != "premium" {
		t.Fatalf("UpstreamGroups[go] = %q, want %q", restored.UpstreamGroups[config.UpstreamGo], "premium")
	}
	if restored.UpstreamGroups[config.UpstreamOllama] != "ollama-premium" {
		t.Fatalf("UpstreamGroups[ollama] = %q, want %q", restored.UpstreamGroups[config.UpstreamOllama], "ollama-premium")
	}

	// Verify other fields are preserved
	if restored.Upstream != config.UpstreamGo {
		t.Fatalf("Upstream = %q, want %q", restored.Upstream, config.UpstreamGo)
	}
	if restored.Protocol != config.ProtocolChat {
		t.Fatalf("Protocol = %q, want %q", restored.Protocol, config.ProtocolChat)
	}
	if restored.RealModel != "real-roundtrip-model" {
		t.Fatalf("RealModel = %q, want %q", restored.RealModel, "real-roundtrip-model")
	}
	if restored.Group != "premium" {
		t.Fatalf("Group = %q, want %q", restored.Group, "premium")
	}
}

// TestModelRouteRoundTripEmptyUpstreams verifies that a route with only
// Upstream (no Upstreams/UpstreamGroups) round-trips correctly and the
// fields remain empty/nil.
func TestModelRouteRoundTripEmptyUpstreams(t *testing.T) {
	if err := InitForTest("file:store_roundtrip_empty?mode=memory&cache=shared"); err != nil {
		t.Fatalf("init test db: %v", err)
	}

	original := config.ModelRoute{
		ID:        "simple-model",
		Name:      "Simple Model",
		Upstream:  config.UpstreamGo,
		Protocol:  config.ProtocolChat,
		RealModel: "simple-model",
		Group:     "go",
	}

	row := NewModelRouteRow(original)
	if err := SaveModelRoute(&row); err != nil {
		t.Fatalf("save model route: %v", err)
	}

	rows, err := LoadModelRoutes()
	if err != nil {
		t.Fatalf("load model routes: %v", err)
	}

	var foundRow ModelRouteRow
	for _, r := range rows {
		if r.ID == "simple-model" {
			foundRow = r
			break
		}
	}
	if foundRow.ID == "" {
		t.Fatal("simple-model not found")
	}

	restored := ModelRouteFromRow(foundRow)

	if len(restored.Upstreams) != 0 {
		t.Fatalf("Upstreams should be empty, got %#v", restored.Upstreams)
	}
	if len(restored.UpstreamGroups) != 0 {
		t.Fatalf("UpstreamGroups should be empty, got %#v", restored.UpstreamGroups)
	}
}
