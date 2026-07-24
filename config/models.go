package config

import (
	"encoding/json"
	"fmt"
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
	UpstreamGo     Upstream = "go"
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

// UpstreamTarget holds per-upstream overrides for a model route.
// When set, these values take precedence over the route-level fields
// for the specific upstream. This allows Go and Ollama to use different
// real_model IDs, protocols, and key-pool groups for the same gateway model.
type UpstreamTarget struct {
	RealModel string   `json:"real_model,omitempty"`
	Protocol  Protocol `json:"protocol,omitempty"`
	Group     string   `json:"group,omitempty"`
}

// ModelRoute maps a gateway-facing model id to its real upstream location.
type ModelRoute struct {
	ID                  string                      `json:"id"`                              // gateway-facing model id, e.g. "glm-5.1"
	Name                string                      `json:"name"`                            // display name
	Upstream            Upstream                    `json:"upstream"`                        // primary upstream (used for routing); go | ollama
	Upstreams           []Upstream                  `json:"upstreams,omitempty"`             // all upstreams that serve this model, e.g. ["go","ollama"]
	UpstreamGroups      map[Upstream]string         `json:"upstream_groups,omitempty"`       // per-upstream key-pool group override; empty = use upstream name
	Targets             map[Upstream]UpstreamTarget `json:"targets,omitempty"`               // per-upstream real_model/protocol/group overrides
	Protocol            Protocol                    `json:"protocol"`                        // chat | messages | responses | google
	RealModel           string                      `json:"real_model"`                      // upstream model id, e.g. "glm-5.1"
	Group               string                      `json:"group"`                           // logical KEY-pool group name, e.g. "go"
	ContextLen          int                         `json:"context_len"`                     // optional context window hint
	Status              *int                        `json:"status,omitempty"`                // 0 disabled, 1 enabled; nil defaults to enabled
	Priority            int                         `json:"priority"`                        // optional admin-defined display/routing priority
	Tags                []string                    `json:"tags,omitempty"`                  // normalized capability tags
	IsCustomized        bool                        `json:"is_customized,omitempty"`         // true when admin edited protected fields
	CustomizedFields    []string                    `json:"customized_fields,omitempty"`     // fields protected from automatic sync
	OpenRouterID        string                      `json:"openrouter_id,omitempty"`         // matched OpenRouter model id
	OpenRouterName      string                      `json:"openrouter_name,omitempty"`       // matched OpenRouter display name
	OpenRouterMatchedBy string                      `json:"openrouter_matched_by,omitempty"` // matching strategy used during enrichment
	Architecture        *ModelArchitecture          `json:"architecture,omitempty"`
	Pricing             map[string]string           `json:"pricing,omitempty"` // per-token OpenRouter prices as strings
	SupportedParameters []string                    `json:"supported_parameters,omitempty"`
	Description         string                      `json:"description,omitempty"`
	KnowledgeCutoff     string                      `json:"knowledge_cutoff,omitempty"`
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

// UpstreamGroup returns the key-pool group for the given upstream.
// When an explicit UpstreamGroups mapping exists, it takes precedence.
// Otherwise, the upstream name itself is used as the group name.
func (m ModelRoute) UpstreamGroup(u Upstream) string {
	if m.UpstreamGroups != nil {
		if g, ok := m.UpstreamGroups[u]; ok && g != "" {
			return g
		}
	}
	return string(u)
}

// ResolveUpstreamGroup is the single authoritative group resolver. It must
// be called once by the outer failover loop and the result passed to every
// downstream consumer (token permission, PickAttempts, Go/Ollama request
// handlers, usage log, billing).
//
// Priority (highest first):
//  1. Targets[upstream].Group — per-upstream explicit override
//  2. UpstreamGroups[upstream] — per-upstream group mapping
//  3. route.Group — legacy route-level group (backward compat for configs
//     like {Upstream:"ollama", Group:"premium"})
//  4. string(upstream) — default: upstream name as group
func (m ModelRoute) ResolveUpstreamGroup(u Upstream) string {
	if m.Targets != nil {
		if t, ok := m.Targets[u]; ok && t.Group != "" {
			return t.Group
		}
	}
	if m.UpstreamGroups != nil {
		if g, ok := m.UpstreamGroups[u]; ok && g != "" {
			return g
		}
	}
	if u == m.Upstream && m.Group != "" {
		return m.Group
	}
	return string(u)
}

// TargetRealModel returns the real_model to use for the given upstream.
// When a per-upstream Target exists with a non-empty RealModel, it takes
// precedence. Otherwise, the route-level RealModel is used.
func (m ModelRoute) TargetRealModel(u Upstream) string {
	if m.Targets != nil {
		if t, ok := m.Targets[u]; ok && t.RealModel != "" {
			return t.RealModel
		}
	}
	return m.RealModel
}

// TargetProtocol returns the protocol to use for the given upstream.
// When a per-upstream Target exists with a non-empty Protocol, it takes
// precedence. Otherwise, the route-level Protocol is used.
func (m ModelRoute) TargetProtocol(u Upstream) Protocol {
	if m.Targets != nil {
		if t, ok := m.Targets[u]; ok && t.Protocol != "" {
			return t.Protocol
		}
	}
	return m.Protocol
}

// TargetGroup returns the key-pool group to use for the given upstream.
// When a per-upstream Target exists with a non-empty Group, it takes
// precedence. Otherwise, UpstreamGroup is used.
func (m ModelRoute) TargetGroup(u Upstream) string {
	if m.Targets != nil {
		if t, ok := m.Targets[u]; ok && t.Group != "" {
			return t.Group
		}
	}
	return m.UpstreamGroup(u)
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

// DefaultModels returns an empty seed catalog. All models are now discovered
// automatically by the modelsync package from upstream APIs (OpenCode Go and
// Ollama Cloud). This function is kept for backward compatibility with callers
// that reference it during startup; it returns nil.
func DefaultModels() []ModelRoute {
	return nil
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

// UnmarshalJSON accepts OpenRouter's extensible pricing object without
// allowing a structured extension (for example the "overrides" array) to
// discard the entire metadata catalog. Scalar string/number prices are kept;
// arrays, objects, booleans, and null are metadata extensions and ignored.
func (m *OpenRouterModel) UnmarshalJSON(data []byte) error {
	type modelAlias OpenRouterModel
	aux := struct {
		Pricing map[string]json.RawMessage `json:"pricing"`
		*modelAlias
	}{modelAlias: (*modelAlias)(m)}
	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}
	m.Pricing = make(map[string]string)
	for key, raw := range aux.Pricing {
		value := strings.TrimSpace(string(raw))
		if value == "" {
			continue
		}
		if value[0] == '"' {
			var decoded string
			if err := json.Unmarshal(raw, &decoded); err == nil {
				m.Pricing[key] = decoded
			}
			continue
		}
		if (value[0] >= '0' && value[0] <= '9') || value[0] == '-' {
			m.Pricing[key] = value
		}
	}
	if len(m.Pricing) == 0 {
		m.Pricing = nil
	}
	return nil
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
	if len(route.Upstreams) == 0 {
		route.Upstreams = []Upstream{route.Upstream}
	} else {
		// Persisted routes from older versions could contain the same channel
		// more than once. Normalize it at registration time so a failover loop
		// can never send the same client request twice to one channel.
		route.Upstreams = deduplicateUpstreams(route.Upstreams)
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
	// Validate invariants after all defaults are applied. Log warnings
	// for invalid routes rather than panicking — the caller (RegisterModel,
	// ReplaceModels) can decide whether to reject the route.
	if err := ValidateModelRoute(*route); err != nil {
		log.Printf("warn: model route %q failed validation: %v", route.ID, err)
	}
}

func deduplicateUpstreams(upstreams []Upstream) []Upstream {
	seen := make(map[Upstream]struct{}, len(upstreams))
	out := make([]Upstream, 0, len(upstreams))
	for _, u := range upstreams {
		if _, ok := seen[u]; ok {
			continue
		}
		seen[u] = struct{}{}
		out = append(out, u)
	}
	return out
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

// ValidUpstreams contains all recognized upstream names.
var ValidUpstreams = []Upstream{UpstreamGo, UpstreamOllama}

// IsValidUpstream returns true if the given upstream is recognized.
func IsValidUpstream(u Upstream) bool {
	return u == UpstreamGo || u == UpstreamOllama
}

// ValidateModelRoute checks invariants on a fully-merged ModelRoute.
// It should be called after all field merging (defaults, sync, admin edits)
// to ensure the route is internally consistent.
//
// Invariants checked:
//   - Upstream must be a valid upstream name.
//   - Upstreams must not be empty.
//   - Upstream must appear in Upstreams.
//   - All Upstreams entries must be valid upstream names.
//   - All UpstreamGroups keys must be valid and appear in Upstreams.
//   - All Targets keys must be valid and appear in Upstreams.
//
// Returns nil if valid, or an error describing the first violation.
func ValidateModelRoute(m ModelRoute) error {
	if !IsValidUpstream(m.Upstream) {
		return fmt.Errorf("invalid primary upstream %q (valid: go, ollama)", m.Upstream)
	}
	if len(m.Upstreams) == 0 {
		return fmt.Errorf("upstreams list must not be empty")
	}
	upstreamSet := make(map[Upstream]bool, len(m.Upstreams))
	for _, u := range m.Upstreams {
		if !IsValidUpstream(u) {
			return fmt.Errorf("invalid upstream %q in upstreams list (valid: go, ollama)", u)
		}
		upstreamSet[u] = true
	}
	if !upstreamSet[m.Upstream] {
		return fmt.Errorf("primary upstream %q not found in upstreams list %v", m.Upstream, m.Upstreams)
	}
	for k := range m.UpstreamGroups {
		if !IsValidUpstream(k) {
			return fmt.Errorf("invalid upstream %q in upstream_groups keys (valid: go, ollama)", k)
		}
		if !upstreamSet[k] {
			return fmt.Errorf("upstream_groups key %q not found in upstreams list %v", k, m.Upstreams)
		}
	}
	for k := range m.Targets {
		if !IsValidUpstream(k) {
			return fmt.Errorf("invalid upstream %q in targets keys (valid: go, ollama)", k)
		}
		if !upstreamSet[k] {
			return fmt.Errorf("targets key %q not found in upstreams list %v", k, m.Upstreams)
		}
	}
	return nil
}

// ValidateAndNormalizeUpstreams validates and deduplicates an upstream list.
// Returns the deduplicated list (preserving original order) or an error.
func ValidateAndNormalizeUpstreams(upstreams []Upstream, groups map[Upstream]string) ([]Upstream, error) {
	seen := make(map[Upstream]bool)
	out := make([]Upstream, 0, len(upstreams))
	for _, u := range upstreams {
		if !IsValidUpstream(u) {
			return nil, fmt.Errorf("unknown upstream %q (valid: go, ollama)", u)
		}
		if seen[u] {
			continue
		}
		seen[u] = true
		out = append(out, u)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("at least one upstream is required")
	}
	for k, v := range groups {
		if !IsValidUpstream(k) {
			return nil, fmt.Errorf("unknown upstream group key %q (valid: go, ollama)", k)
		}
		if strings.TrimSpace(v) == "" {
			return nil, fmt.Errorf("upstream group %q has empty value", k)
		}
	}
	return out, nil
}
