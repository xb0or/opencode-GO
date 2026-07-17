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
	UpstreamOllama Upstream = "ollama"
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
	Status              *int               `json:"status,omitempty"`                // 0 disabled, 1 enabled; nil defaults to enabled
	Priority            int                `json:"priority"`                        // optional admin-defined display/routing priority
	Tags                []string           `json:"tags,omitempty"`                  // normalized capability tags
	IsCustomized        bool               `json:"is_customized,omitempty"`         // true when admin edited protected fields
	CustomizedFields    []string           `json:"customized_fields,omitempty"`     // fields protected from automatic sync
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

const (
	ModelStatusDisabled = 0
	ModelStatusEnabled  = 1
)

// ModelStatusPtr returns a pointer suitable for ModelRoute.Status.
func ModelStatusPtr(status int) *int {
	v := status
	return &v
}

// IsEnabled reports whether a route should be visible and callable.
func (m ModelRoute) IsEnabled() bool {
	return m.Status == nil || *m.Status != ModelStatusDisabled
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

// ReplaceModels replaces the in-memory route table with the supplied routes.
func ReplaceModels(ms []ModelRoute) {
	next := make(map[string]ModelRoute, len(ms))
	for _, m := range ms {
		applyLocalModelDefaults(&m)
		next[m.ID] = m
	}
	routesMu.Lock()
	defer routesMu.Unlock()
	routes = next
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
	sortModelRoutes(out)
	return out
}

// AllEnabledModels returns all registered routes that are currently enabled.
func AllEnabledModels() []ModelRoute {
	routesMu.RLock()
	defer routesMu.RUnlock()
	out := make([]ModelRoute, 0, len(routes))
	for _, m := range routes {
		if m.IsEnabled() {
			out = append(out, m)
		}
	}
	sortModelRoutes(out)
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
	case UpstreamOllama:
		return c.OllamaBaseURL
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

		// --- Ollama Cloud models (group: ollama) ---
		// Ollama Cloud is a hosted service at https://ollama.com offering
		// an OpenAI-compatible /v1/chat/completions endpoint with Bearer auth.
		{ID: "gpt-oss:120b", Name: "GPT-OSS 120B", Upstream: UpstreamOllama, Protocol: ProtocolChat, RealModel: "gpt-oss:120b", Group: "ollama"},
		{ID: "gpt-oss:20b", Name: "GPT-OSS 20B", Upstream: UpstreamOllama, Protocol: ProtocolChat, RealModel: "gpt-oss:20b", Group: "ollama"},
		{ID: "qwen3.5:397b", Name: "Qwen3.5 397B", Upstream: UpstreamOllama, Protocol: ProtocolChat, RealModel: "qwen3.5:397b", Group: "ollama"},
		{ID: "gemma4:31b", Name: "Gemma4 31B", Upstream: UpstreamOllama, Protocol: ProtocolChat, RealModel: "gemma4:31b", Group: "ollama"},
		{ID: "mistral-large-3:675b", Name: "Mistral Large 3 675B", Upstream: UpstreamOllama, Protocol: ProtocolChat, RealModel: "mistral-large-3:675b", Group: "ollama"},
		{ID: "nemotron-3-ultra", Name: "Nemotron 3 Ultra", Upstream: UpstreamOllama, Protocol: ProtocolChat, RealModel: "nemotron-3-ultra", Group: "ollama"},
		{ID: "nemotron-3-super", Name: "Nemotron 3 Super", Upstream: UpstreamOllama, Protocol: ProtocolChat, RealModel: "nemotron-3-super", Group: "ollama"},
		{ID: "nemotron-3-nano:30b", Name: "Nemotron 3 Nano 30B", Upstream: UpstreamOllama, Protocol: ProtocolChat, RealModel: "nemotron-3-nano:30b", Group: "ollama"},
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
		Data []OpenRouterModel `json:"data"`
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
		if matched, matchedBy, ok := MatchOpenRouterModel(route, payload.Data); ok {
			ApplyOpenRouterMetadata(&route, matched, matchedBy)
			routes[id] = route
		}
	}
}

// OpenRouterModel is the subset of OpenRouter model metadata used for catalog
// enrichment.
type OpenRouterModel struct {
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

type openRouterModel = OpenRouterModel

func MatchOpenRouterModel(route ModelRoute, candidates []OpenRouterModel) (OpenRouterModel, string, bool) {
	keys := []string{route.RealModel, route.ID, route.Name}
	bestScore := 0
	bestBy := ""
	var best OpenRouterModel
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

func matchOpenRouterModel(route ModelRoute, candidates []openRouterModel) (openRouterModel, string, bool) {
	return MatchOpenRouterModel(route, candidates)
}

func matchScore(keys []string, candidate OpenRouterModel) (int, string) {
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

// ApplyOpenRouterMetadata attaches OpenRouter metadata to a route without
// overwriting admin-customized protected fields.
func ApplyOpenRouterMetadata(route *ModelRoute, metadata OpenRouterModel, matchedBy string) {
	route.OpenRouterID = metadata.ID
	route.OpenRouterName = metadata.Name
	route.OpenRouterMatchedBy = matchedBy
	if metadata.ContextLength > 0 && !IsModelFieldCustomized(*route, "context_len") {
		route.ContextLen = metadata.ContextLength
	}
	route.Architecture = metadata.Architecture
	if !IsModelFieldCustomized(*route, "pricing") {
		route.Pricing = copyStringMap(metadata.Pricing)
	}
	route.SupportedParameters = append([]string(nil), metadata.SupportedParameters...)
	sort.Strings(route.SupportedParameters)
	if !IsModelFieldCustomized(*route, "tags") {
		route.Tags = DeriveModelTags(*route, metadata)
	}
	route.Description = metadata.Description
	route.KnowledgeCutoff = metadata.KnowledgeCutoff
}

func applyOpenRouterMetadata(route *ModelRoute, metadata openRouterModel, matchedBy string) {
	ApplyOpenRouterMetadata(route, metadata, matchedBy)
}

func applyLocalModelDefaults(route *ModelRoute) {
	if route.Upstream == "" {
		route.Upstream = UpstreamGo
	}
	// Populate Upstreams from Upstream when empty, so the request phase always
	// has a complete list to iterate over. This keeps single-upstream configs
	// working identically while enabling multi-upstream failover.
	if len(route.Upstreams) == 0 {
		route.Upstreams = []Upstream{route.Upstream}
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
	if route.Status == nil {
		route.Status = ModelStatusPtr(ModelStatusEnabled)
	}
	if route.Tags != nil {
		route.Tags = NormalizeModelTags(route.Tags)
	}
	route.CustomizedFields = NormalizeCustomizedFields(route.CustomizedFields)
	route.IsCustomized = route.IsCustomized || len(route.CustomizedFields) > 0
}

// IsModelFieldCustomized reports whether automatic sync should preserve a
// locally edited field.
func IsModelFieldCustomized(route ModelRoute, field string) bool {
	field = normalizeCustomizedField(field)
	for _, f := range route.CustomizedFields {
		if normalizeCustomizedField(f) == field {
			return true
		}
	}
	return false
}

// NormalizeCustomizedFields returns a sorted, de-duplicated field list.
func NormalizeCustomizedFields(fields []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(fields))
	for _, field := range fields {
		field = normalizeCustomizedField(field)
		if field == "" || seen[field] {
			continue
		}
		seen[field] = true
		out = append(out, field)
	}
	sort.Strings(out)
	return out
}

func normalizeCustomizedField(field string) string {
	field = strings.TrimSpace(strings.ToLower(field))
	switch field {
	case "context_length":
		return "context_len"
	case "capabilities", "capability":
		return "tags"
	case "price", "prices", "billing", "billing_rates":
		return "pricing"
	case "display_name":
		return "name"
	default:
		return field
	}
}

// NormalizeModelTags returns sorted, de-duplicated capability tags.
func NormalizeModelTags(tags []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(tags))
	for _, tag := range tags {
		tag = strings.TrimSpace(strings.ToLower(tag))
		tag = strings.ReplaceAll(tag, "_", "-")
		if tag == "" || seen[tag] {
			continue
		}
		seen[tag] = true
		out = append(out, tag)
	}
	sort.Strings(out)
	return out
}

// DeriveModelTags turns OpenRouter metadata into simple user-facing capability
// tags used by the admin UI and public model catalog.
func DeriveModelTags(route ModelRoute, metadata OpenRouterModel) []string {
	var tags []string
	modalities := append([]string{}, metadata.ArchitectureInputModalities()...)
	if len(modalities) == 0 && route.Architecture != nil {
		modalities = append(modalities, route.Architecture.InputModalities...)
	}
	if len(modalities) == 0 {
		tags = append(tags, "text")
	}
	for _, modality := range modalities {
		switch strings.ToLower(strings.TrimSpace(modality)) {
		case "text":
			tags = append(tags, "text")
		case "image":
			tags = append(tags, "vision")
		case "video":
			tags = append(tags, "video")
		case "audio":
			tags = append(tags, "audio")
		}
	}
	params := append([]string{}, metadata.SupportedParameters...)
	if len(params) == 0 {
		params = append(params, route.SupportedParameters...)
	}
	for _, p := range params {
		switch strings.ToLower(strings.TrimSpace(p)) {
		case "tools", "tool_choice":
			tags = append(tags, "tools")
		case "structured_outputs", "response_format":
			tags = append(tags, "structured")
		case "reasoning", "include_reasoning":
			tags = append(tags, "reasoning")
		}
	}
	haystack := strings.ToLower(route.ID + " " + route.Name + " " + route.RealModel + " " + metadata.ID + " " + metadata.Name + " " + metadata.Description)
	if strings.Contains(haystack, "code") || strings.Contains(haystack, "coder") || strings.Contains(haystack, "coding") {
		tags = append(tags, "code")
	}
	if strings.Contains(haystack, "reason") || strings.Contains(haystack, "thinking") {
		tags = append(tags, "reasoning")
	}
	return NormalizeModelTags(tags)
}

// ArchitectureInputModalities safely returns OpenRouter input modalities.
func (m OpenRouterModel) ArchitectureInputModalities() []string {
	if m.Architecture == nil {
		return nil
	}
	return m.Architecture.InputModalities
}

func copyStringMap(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func sortModelRoutes(ms []ModelRoute) {
	sort.SliceStable(ms, func(i, j int) bool {
		if ms[i].Priority != ms[j].Priority {
			return ms[i].Priority > ms[j].Priority
		}
		if ms[i].Name != ms[j].Name {
			return strings.ToLower(ms[i].Name) < strings.ToLower(ms[j].Name)
		}
		return ms[i].ID < ms[j].ID
	})
}
