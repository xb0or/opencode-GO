package router

import (
	"encoding/json"
	"log"
	"strings"

	"github.com/xb0or/opencode-GO/config"
)

// Resolution is the result of resolving a client-facing model id to its
// upstream route. It is the single object the handler layer needs to know
// about — it never touches Provider details directly.
type Resolution struct {
	Route         config.ModelRoute
	IsPassthrough bool // true when the model was not in the registry
	NotFound      bool // true when the model is unknown and passthrough is disabled
}

// Resolve maps a client-facing model id to its upstream route.
//
// The resolution pipeline:
//  1. Apply model alias/mapping (config.LookupModelMapping) if configured.
//  2. Look up the (possibly remapped) model in the registry.
//  3. If not found, check passthrough mode:
//     - "go" (legacy): return a passthrough route to the Go upstream.
//     - "disabled" (default/strict): return NotFound=true so the handler
//       returns 404 to the client.
//
// The caller never needs to know which Provider serves the model — that
// information lives inside config.ModelRoute and is consumed later by the
// Provider layer.
func Resolve(model string, inbound config.Protocol) Resolution {
	// Step 1: model alias/mapping
	if mapped, ok := config.LookupModelMapping(model); ok {
		log.Printf("model mapping applied: %q -> %q", model, mapped)
		model = mapped
	}

	// Step 2: registry lookup
	route, found := config.LookupModel(model)
	if found {
		return Resolution{Route: route}
	}

	// Step 3: passthrough or 404
	mode := config.Get().PassthroughMode
	if mode == "go" {
		return Resolution{Route: passthroughRoute(model, inbound), IsPassthrough: true}
	}
	// Strict mode (default): model not found.
	return Resolution{NotFound: true}
}

// passthroughRoute builds a default route for models not in the registry.
// The model name is forwarded as-is to the default upstream.
func passthroughRoute(model string, inbound config.Protocol) config.ModelRoute {
	id := strings.TrimSpace(model)
	if id == "" {
		id = "passthrough"
	}
	return config.ModelRoute{
		ID:        id,
		Name:      id,
		Upstream:  config.UpstreamGo,
		Protocol:  inbound,
		RealModel: model,
		Group:     "go",
	}
}

// RewriteRequestModel returns body with the top-level "model" field replaced by the
// upstream real model id. It re-marshals compact JSON; on any failure the
// original body is returned with ok=false.
func RewriteRequestModel(body []byte, realModel string) ([]byte, bool) {
	if realModel == "" {
		return body, false
	}
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		return body, false
	}
	m["model"] = realModel
	out, err := json.Marshal(m)
	if err != nil {
		return body, false
	}
	return out, true
}

// EnableRequestStreamUsage asks upstream protocols that support it to include final
// usage accounting in SSE streams so admin usage logs can record token counts.
func EnableRequestStreamUsage(body []byte, proto config.Protocol, stream bool) ([]byte, bool) {
	if !stream {
		return body, false
	}
	if proto != config.ProtocolChat {
		return body, false
	}
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		return body, false
	}
	opts := objectField(m, "stream_options")
	if opts == nil {
		opts = map[string]any{}
	}
	opts["include_usage"] = true
	m["stream_options"] = opts
	out, err := json.Marshal(m)
	if err != nil {
		return body, false
	}
	return out, true
}

// --- helpers (copied from decode.go to keep router self-contained) ---

func objectField(m map[string]any, key string) map[string]any {
	if v, ok := m[key].(map[string]any); ok {
		return v
	}
	return nil
}