package router

import (
	"encoding/json"
	"testing"

	"github.com/xb0or/opencode-GO/config"
)

func TestResolvePassthroughFallback(t *testing.T) {
	config.ReplaceModels(nil)
	defer config.ReplaceModels(nil)

	res := Resolve("unknown-model", config.ProtocolChat)
	if !res.IsPassthrough {
		t.Fatalf("expected passthrough, got routed=%v", res.IsPassthrough)
	}
	if res.Route.ID != "unknown-model" {
		t.Fatalf("expected ID passthrough, got %q", res.Route.ID)
	}
	if res.Route.Protocol != config.ProtocolChat {
		t.Fatalf("expected protocol to match inbound, got %q", res.Route.Protocol)
	}
}

func TestResolveRegisteredModel(t *testing.T) {
	m := config.ModelRoute{
		ID:        "test-model",
		Name:      "Test",
		Upstream:  config.UpstreamGo,
		Protocol:  config.ProtocolMessages,
		RealModel: "real-test",
		Group:     "go",
	}
	config.RegisterModel(m)
	defer config.RemoveModel("test-model")

	res := Resolve("test-model", config.ProtocolChat)
	if res.IsPassthrough {
		t.Fatalf("expected registered model, got passthrough")
	}
	if res.Route.RealModel != "real-test" {
		t.Fatalf("expected real model, got %q", res.Route.RealModel)
	}
	// Protocol should come from the route, not the inbound protocol
	if res.Route.Protocol != config.ProtocolMessages {
		t.Fatalf("expected Messages protocol from route, got %q", res.Route.Protocol)
	}
}

func TestResolveModelMapping(t *testing.T) {
	config.RegisterModelMapping("alias-model", "real-model")
	defer config.RemoveModelMapping("alias-model")

	m := config.ModelRoute{
		ID:        "real-model",
		Name:      "Real Model",
		Upstream:  config.UpstreamGo,
		Protocol:  config.ProtocolChat,
		RealModel: "real-model",
		Group:     "go",
	}
	config.RegisterModel(m)
	defer config.RemoveModel("real-model")

	res := Resolve("alias-model", config.ProtocolChat)
	if res.IsPassthrough {
		t.Fatalf("aliased model should resolve to registered model")
	}
	if res.Route.ID != "real-model" {
		t.Fatalf("expected mapped model real-model, got %q", res.Route.ID)
	}
}

func TestRewriteModel(t *testing.T) {
	body := []byte(`{"model":"gpt-4","messages":[]}`)
	out, ok := RewriteModel(body, "gpt-5")
	if !ok {
		t.Fatal("RewriteModel should succeed")
	}
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("rewritten body is not JSON: %v", err)
	}
	if m["model"] != "gpt-5" {
		t.Fatalf("expected model gpt-5, got %v", m["model"])
	}
}

func TestEnableStreamUsageChatOnly(t *testing.T) {
	body := []byte(`{"model":"m","stream":true}`)

	// Chat protocol should add stream_options
	out, ok := EnableStreamUsage(body, config.ProtocolChat, true)
	if !ok {
		t.Fatal("EnableStreamUsage should rewrite chat stream request")
	}
	var m map[string]any
	_ = json.Unmarshal(out, &m)
	opts, _ := m["stream_options"].(map[string]any)
	if opts["include_usage"] != true {
		t.Fatalf("stream_options.include_usage = %#v, want true", opts["include_usage"])
	}

	// Messages should not be rewritten
	_, ok = EnableStreamUsage(body, config.ProtocolMessages, true)
	if ok {
		t.Fatal("EnableStreamUsage should not touch Messages protocol")
	}

	// Non-stream should not be rewritten
	_, ok = EnableStreamUsage(body, config.ProtocolChat, false)
	if ok {
		t.Fatal("EnableStreamUsage should not touch non-stream requests")
	}
}