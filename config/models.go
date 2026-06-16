package config

import (
	"encoding/json"
	"log"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
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
	ID                  string             `json:"id"`                              // gateway-facing model id, e.g. "glm-5.1"
	Name                string             `json:"name"`                            // display name
	Upstream            Upstream           `json:"upstream"`                        // go
	Protocol            Protocol           `json:"protocol"`                        // chat | messages | responses | google
	RealModel           string             `json:"real_model"`                      // upstream model id, e.g. "glm-5.1"
	Group               string             `json:"group"`                           // logical KEY-pool group, e.g. "go"
	ContextLen          int                `json:"context_len"`                     // optional context window hint
	OpenRouterID        string             `json:"openrouter_id,omitempty"`         // matched OpenRouter model id
	OpenRouterName      string             `json:"openrouter_name,omitempty"`       // matched OpenRouter display name
	OpenRouterMatchedBy string             `json:"openrouter_matched_by,omitempty"` // matching strategy used during enrichment
	Architecture        *ModelArchitecture `json:"architecture,omitempty"`
	Pricing             map[string]string  `json:"pricing,omitempty"` // per-token OpenRouter prices as strings
	SupportedParameters []string           `json:"supported_parameters,omitempty"`
	Description         string             `json:"description,omitempty"`
	KnowledgeCutoff     string             `json:"knowledge_cutoff,omitempty"`
}

// ModelArchitecture describes OpenRouter modality/tokenizer metadata.
type ModelArchitecture struct {
	Modality         string   `json:"modality,omitempty"`
	InputModalities  []string `json:"input_modalities,omitempty"`
	OutputModalities []string `json:"output_modalities,omitempty"`
	Tokenizer        string   `json:"tokenizer,omitempty"`
	InstructType     string   `json:"instruct_type,omitempty"`
}

var (
	routes   = map[string]ModelRoute{}
	routesMu sync.RWMutex
)

// RegisterModel adds or replaces a model route.
func RegisterModel(m ModelRoute) {
	applyLocalModelDefaults(&m)
	routesMu.Lock()
	defer routesMu.Unlock()
	routes[m.ID] = m
}

// RegisterModels bulk-registers model routes.
func RegisterModels(ms []ModelRoute) {
	routesMu.Lock()
	defer routesMu.Unlock()
	for _, m := range ms {
		applyLocalModelDefaults(&m)
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

// DefaultModels is the OpenCode Go seed catalog.
// The group field doubles as the KEY-pool group name used by the pool package.
func DefaultModels() []ModelRoute {
	return []ModelRoute{
		// --- OpenAI-compatible Chat Completions ---
		{ID: "glm-5.1", Name: "GLM-5.1", Upstream: UpstreamGo, Protocol: ProtocolChat, RealModel: "glm-5.1", Group: "go"},
		{ID: "glm-5", Name: "GLM-5", Upstream: UpstreamGo, Protocol: ProtocolChat, RealModel: "glm-5", Group: "go"},
		{ID: "kimi-k2.7-code", Name: "Kimi K2.7 Code", Upstream: UpstreamGo, Protocol: ProtocolChat, RealModel: "kimi-k2.7-code", Group: "go"},
		{ID: "kimi-k2.6", Name: "Kimi K2.6", Upstream: UpstreamGo, Protocol: ProtocolChat, RealModel: "kimi-k2.6", Group: "go"},
		{ID: "mimo-v2.5", Name: "MiMo-V2.5", Upstream: UpstreamGo, Protocol: ProtocolChat, RealModel: "mimo-v2.5", Group: "go"},
		{ID: "mimo-v2.5-pro", Name: "MiMo-V2.5-Pro", Upstream: UpstreamGo, Protocol: ProtocolChat, RealModel: "mimo-v2.5-pro", Group: "go"},
		{ID: "deepseek-v4-pro", Name: "DeepSeek V4 Pro", Upstream: UpstreamGo, Protocol: ProtocolChat, RealModel: "deepseek-v4-pro", Group: "go"},
		{ID: "deepseek-v4-flash", Name: "DeepSeek V4 Flash", Upstream: UpstreamGo, Protocol: ProtocolChat, RealModel: "deepseek-v4-flash", Group: "go"},

		// --- Anthropic-compatible Messages ---
		{ID: "minimax-m3", Name: "MiniMax M3", Upstream: UpstreamGo, Protocol: ProtocolMessages, RealModel: "minimax-m3", Group: "go"},
		{ID: "minimax-m2.7", Name: "MiniMax M2.7", Upstream: UpstreamGo, Protocol: ProtocolMessages, RealModel: "minimax-m2.7", Group: "go"},
		{ID: "minimax-m2.5", Name: "MiniMax M2.5", Upstream: UpstreamGo, Protocol: ProtocolMessages, RealModel: "minimax-m2.5", Group: "go"},
		{ID: "qwen3.7-max", Name: "Qwen3.7 Max", Upstream: UpstreamGo, Protocol: ProtocolMessages, RealModel: "qwen3.7-max", Group: "go"},
		{ID: "qwen3.7-plus", Name: "Qwen3.7 Plus", Upstream: UpstreamGo, Protocol: ProtocolMessages, RealModel: "qwen3.7-plus", Group: "go"},
		{ID: "qwen3.6-plus", Name: "Qwen3.6 Plus", Upstream: UpstreamGo, Protocol: ProtocolMessages, RealModel: "qwen3.6-plus", Group: "go"},
	}
}

// EnrichModelsFromOpenRouter fetches OpenRouter metadata and best-effort
// attaches context length, pricing, modality, and supported-parameter data to
// currently registered routes. Network failures are logged and ignored.
func EnrichModelsFromOpenRouter() {
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Get("https://openrouter.ai/api/v1/models")
	if err != nil {
		log.Printf("warn: cannot fetch OpenRouter model metadata: %v", err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		log.Printf("warn: OpenRouter model metadata returned status %d", resp.StatusCode)
		return
	}

	var payload struct {
		Data []openRouterModel `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		log.Printf("warn: cannot decode OpenRouter model metadata: %v", err)
		return
	}
	if len(payload.Data) == 0 {
		return
	}

	routesMu.Lock()
	defer routesMu.Unlock()
	for id, route := range routes {
		if matched, matchedBy, ok := matchOpenRouterModel(route, payload.Data); ok {
			applyOpenRouterMetadata(&route, matched, matchedBy)
			routes[id] = route
		}
	}
}

type openRouterModel struct {
	ID                  string             `json:"id"`
	CanonicalSlug       string             `json:"canonical_slug"`
	Name                string             `json:"name"`
	Description         string             `json:"description"`
	ContextLength       int                `json:"context_length"`
	Architecture        *ModelArchitecture `json:"architecture"`
	Pricing             map[string]string  `json:"pricing"`
	SupportedParameters []string           `json:"supported_parameters"`
	KnowledgeCutoff     string             `json:"knowledge_cutoff"`
}

func matchOpenRouterModel(route ModelRoute, candidates []openRouterModel) (openRouterModel, string, bool) {
	keys := []string{route.RealModel, route.ID, route.Name}
	bestScore := 0
	bestBy := ""
	var best openRouterModel
	for _, candidate := range candidates {
		score, matchedBy := matchScore(keys, candidate)
		if score > bestScore || (score == bestScore && score > 0 && candidate.ID < best.ID) {
			bestScore = score
			bestBy = matchedBy
			best = candidate
		}
	}
	return best, bestBy, bestScore >= 80
}

func matchScore(keys []string, candidate openRouterModel) (int, string) {
	fields := []struct {
		name  string
		value string
	}{
		{name: "id", value: candidate.ID},
		{name: "canonical_slug", value: candidate.CanonicalSlug},
		{name: "name", value: candidate.Name},
	}
	for _, key := range keys {
		nk := normalizeModelKey(key)
		if nk == "" {
			continue
		}
		for _, field := range fields {
			nv := normalizeModelKey(field.value)
			if nv == "" {
				continue
			}
			if nv == nk || strings.HasSuffix(nv, nk) {
				return 100, field.name
			}
		}
	}
	for _, key := range keys {
		nk := normalizeModelKey(key)
		if nk == "" {
			continue
		}
		for _, field := range fields {
			nv := normalizeModelKey(field.value)
			if strings.Contains(nv, nk) {
				return 80, field.name + "_contains"
			}
		}
	}
	return 0, ""
}

var modelKeyCleaner = regexp.MustCompile(`[^a-z0-9]+`)

func normalizeModelKey(s string) string {
	return modelKeyCleaner.ReplaceAllString(strings.ToLower(s), "")
}

func applyOpenRouterMetadata(route *ModelRoute, metadata openRouterModel, matchedBy string) {
	route.OpenRouterID = metadata.ID
	route.OpenRouterName = metadata.Name
	route.OpenRouterMatchedBy = matchedBy
	if metadata.ContextLength > 0 {
		route.ContextLen = metadata.ContextLength
	}
	route.Architecture = metadata.Architecture
	route.Pricing = metadata.Pricing
	route.SupportedParameters = append([]string(nil), metadata.SupportedParameters...)
	sort.Strings(route.SupportedParameters)
	route.Description = metadata.Description
	route.KnowledgeCutoff = metadata.KnowledgeCutoff
}

func applyLocalModelDefaults(route *ModelRoute) {
	if route.Upstream == "" {
		route.Upstream = UpstreamGo
	}
	if route.Group == "" {
		route.Group = "go"
	}
	if route.RealModel == "" {
		route.RealModel = route.ID
	}
	if route.Name == "" {
		route.Name = route.ID
	}
}
