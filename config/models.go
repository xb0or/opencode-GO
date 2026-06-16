package config

import (
	"sync"
)

// Upstream identifies which OpenCode product line a model is served from.
type Upstream string

const (
	UpstreamGo Upstream = "go"
)

// Protocol identifies the wire format a model speaks upstream.
type Protocol string

const (
	ProtocolChat      Protocol = "chat"      // OpenAI /v1/chat/completions
	ProtocolMessages  Protocol = "messages"  // Anthropic /v1/messages
	ProtocolResponses Protocol = "responses" // OpenAI /v1/responses
	ProtocolGoogle    Protocol = "google"    // Google (not implemented in phase 1)
)

// ModelRoute maps a gateway-facing model id to its real upstream location.
type ModelRoute struct {
	ID         string   `json:"id"`          // gateway-facing model id, e.g. "glm-4.6"
	Name       string   `json:"name"`        // display name
	Upstream   Upstream `json:"upstream"`    // go
	Protocol   Protocol `json:"protocol"`    // chat | messages | responses | google
	RealModel  string   `json:"real_model"`  // upstream model id, e.g. "glm/glm-4.6"
	Group      string   `json:"group"`       // logical KEY-pool group, e.g. "go"
	ContextLen int      `json:"context_len"` // optional context window hint
}

var (
	routes   = map[string]ModelRoute{}
	routesMu sync.RWMutex
)

// RegisterModel adds or replaces a model route.
func RegisterModel(m ModelRoute) {
	routesMu.Lock()
	defer routesMu.Unlock()
	routes[m.ID] = m
}

// RegisterModels bulk-registers model routes.
func RegisterModels(ms []ModelRoute) {
	routesMu.Lock()
	defer routesMu.Unlock()
	for _, m := range ms {
		routes[m.ID] = m
	}
}

// LookupModel returns the route for a model id and whether it existed.
func LookupModel(id string) (ModelRoute, bool) {
	routesMu.RLock()
	defer routesMu.RUnlock()
	m, ok := routes[id]
	return m, ok
}

// AllModels returns a snapshot of all registered routes.
func AllModels() []ModelRoute {
	routesMu.RLock()
	defer routesMu.RUnlock()
	out := make([]ModelRoute, 0, len(routes))
	for _, m := range routes {
		out = append(out, m)
	}
	return out
}

// RemoveModel deletes a model route.
func RemoveModel(id string) {
	routesMu.Lock()
	defer routesMu.Unlock()
	delete(routes, id)
}

// BaseURLFor returns the upstream base URL for a given upstream kind.
func BaseURLFor(u Upstream) string {
	c := Get()
	switch u {
	case UpstreamGo:
		return c.GoBaseURL
	default:
		return c.GoBaseURL
	}
}

// DefaultModels is the seed catalog (sourced from models.dev/providers/opencode).
// The group field doubles as the KEY-pool group name used by the pool package.
func DefaultModels() []ModelRoute {
	return []ModelRoute{
		// --- OpenAI Chat Completions (Go subscription) ---
		{ID: "go-glm-4.6", Name: "GLM 4.6 (Go)", Upstream: UpstreamGo, Protocol: ProtocolChat, RealModel: "zai/glm-4.6", Group: "go"},
		{ID: "go-deepseek-v3.2", Name: "DeepSeek V3.2 (Go)", Upstream: UpstreamGo, Protocol: ProtocolChat, RealModel: "deepseek/deepseek-v3.2", Group: "go"},
		{ID: "go-kimi-k2", Name: "Kimi K2 (Go)", Upstream: UpstreamGo, Protocol: ProtocolChat, RealModel: "moonshot/kimi-k2", Group: "go"},
	}
}
