package modelsync

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/xb0or/opencode-GO/config"
	"github.com/xb0or/opencode-GO/store"
)

const (
	defaultOpenCodeModelsURL   = "https://opencode.ai/zen/go/v1/models"
	defaultOpenRouterModelsURL = "https://openrouter.ai/api/v1/models"
	defaultOllamaModelsURL     = "https://ollama.com/api/tags"
)

// Options configures a model catalog synchronization run.
type Options struct {
	OpenCodeModelsURL   string
	OpenRouterModelsURL string
	OllamaModelsURL     string
	Client              *http.Client
	Now                 func() time.Time
}

// Result summarizes a synchronization run.
type Result struct {
	OpenCodeCount   int      `json:"opencode_count"`
	OllamaCount     int      `json:"ollama_count"`
	OpenRouterCount int      `json:"openrouter_count"`
	MatchedCount    int      `json:"matched_count"`
	CreatedCount    int      `json:"created_count"`
	UpdatedCount    int      `json:"updated_count"`
	TotalCount      int      `json:"total_count"`
	Warnings        []string `json:"warnings,omitempty"`
}

type openCodePayload struct {
	Data []openCodeModel `json:"data"`
}

type openCodeModel struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Object  string `json:"object"`
	OwnedBy string `json:"owned_by"`
}

type ollamaTagsPayload struct {
	Models []ollamaModel `json:"models"`
}

type ollamaModel struct {
	Name string `json:"name"`
}

type openRouterPayload struct {
	Data []config.OpenRouterModel `json:"data"`
}

// sourceModel is a unified representation of a model from any upstream source.
type sourceModel struct {
	ID       string
	Name     string
	Upstream config.Upstream
	Upstreams []config.Upstream
}

// Sync fetches models from both the OpenCode Go and Ollama Cloud APIs, merges
// them by ID (overlapping models get both upstreams), enriches with OpenRouter
// metadata, persists the merged catalog, and refreshes the runtime route table.
// Admin customized fields are preserved according to each row's customized_fields set.
func Sync(ctx context.Context, opts Options) (Result, error) {
	opts = withDefaults(opts)
	client := opts.Client

	// --- Fetch all upstream sources concurrently ---
	type fetchResult struct {
		source  string
		models  []sourceModel
		warning string
	}

	results := make(chan fetchResult, 3)
	// Go upstream
	go func() {
		models, err := fetchOpenCodeModels(ctx, client, opts.OpenCodeModelsURL)
		if err != nil {
			results <- fetchResult{source: "go", warning: "opencode: " + err.Error()}
			return
		}
		out := make([]sourceModel, 0, len(models))
		for _, m := range models {
			out = append(out, sourceModel{ID: m.ID, Name: m.Name, Upstream: config.UpstreamGo})
		}
		results <- fetchResult{source: "go", models: out}
	}()

	// Ollama upstream
	go func() {
		models, err := fetchOllamaModels(ctx, client, opts.OllamaModelsURL)
		if err != nil {
			results <- fetchResult{source: "ollama", warning: "ollama: " + err.Error()}
			return
		}
		out := make([]sourceModel, 0, len(models))
		for _, m := range models {
			out = append(out, sourceModel{ID: m.Name, Name: m.Name, Upstream: config.UpstreamOllama})
		}
		results <- fetchResult{source: "ollama", models: out}
	}()

	// OpenRouter (for metadata enrichment, non-blocking)
	var openrouterModels []config.OpenRouterModel
	go func() {
		orm, err := fetchOpenRouterModels(ctx, client, opts.OpenRouterModelsURL)
		if err != nil {
			results <- fetchResult{source: "openrouter", warning: "openrouter: " + err.Error()}
			return
		}
		results <- fetchResult{source: "openrouter", models: nil}
		openrouterModels = orm
	}()

	// Collect results
	var goModels, ollamaModels []sourceModel
	result := Result{}
	for i := 0; i < 3; i++ {
		r := <-results
		switch r.source {
		case "go":
			if r.warning != "" {
				result.Warnings = append(result.Warnings, r.warning)
			} else {
				goModels = r.models
				result.OpenCodeCount = len(goModels)
			}
		case "ollama":
			if r.warning != "" {
				result.Warnings = append(result.Warnings, r.warning)
			} else {
				ollamaModels = r.models
				result.OllamaCount = len(ollamaModels)
			}
		case "openrouter":
			if r.warning != "" {
				result.Warnings = append(result.Warnings, r.warning)
			} else {
				result.OpenRouterCount = len(openrouterModels)
			}
		}
	}

	// --- Merge by model ID ---
	merged := mergeModels(goModels, ollamaModels)

	// --- Load existing DB rows ---
	existingRows, err := store.LoadModelRoutes()
	if err != nil {
		return result, fmt.Errorf("load model routes: %w", err)
	}
	existingByID := make(map[string]store.ModelRouteRow, len(existingRows))
	for _, row := range existingRows {
		existingByID[row.ID] = row
	}

	now := opts.Now()
	seen := map[string]bool{}
	for _, sm := range merged {
		id := strings.TrimSpace(sm.ID)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true

		row, existed := existingByID[id]
		route := buildMergedRoute(sm, row, existed)

		if matched, matchedBy, ok := config.MatchOpenRouterModel(route, openrouterModels); ok {
			config.ApplyOpenRouterMetadata(&route, matched, matchedBy)
			result.MatchedCount++
		} else if len(route.Tags) == 0 && !config.IsModelFieldCustomized(route, "tags") {
			route.Tags = fallbackTags(route)
		}

		nextRow := store.NewModelRouteRow(route)
		nextRow.LastSyncedAt = &now
		if existed {
			nextRow.CreatedAt = row.CreatedAt
			result.UpdatedCount++
		} else {
			result.CreatedCount++
		}
		if err := store.SaveModelRoute(&nextRow); err != nil {
			return result, fmt.Errorf("save model route %s: %w", id, err)
		}
	}

	if err := ReloadRuntimeFromStore(); err != nil {
		return result, err
	}
	result.TotalCount = len(config.AllModels())
	return result, nil
}

// mergeModels combines Go and Ollama model lists by ID, producing a single
// deduplicated list where overlapping models carry both upstreams.
func mergeModels(goModels, ollamaModels []sourceModel) []sourceModel {
	byID := make(map[string]sourceModel)
	order := []string{}

	addUpstream := func(sm sourceModel) {
		existing, ok := byID[sm.ID]
		if !ok {
			byID[sm.ID] = sm
			order = append(order, sm.ID)
			return
		}
		// Merge: add this upstream to the existing entry
		existing.Upstreams = appendUniqueUpstream(existing.Upstreams, sm.Upstream)
		byID[sm.ID] = existing
	}

	for _, m := range goModels {
		m.Upstreams = []config.Upstream{config.UpstreamGo}
		addUpstream(m)
	}
	for _, m := range ollamaModels {
		existing, ok := byID[m.ID]
		if ok {
			// Overlap: merge upstreams
			existing.Upstreams = appendUniqueUpstream(existing.Upstreams, config.UpstreamOllama)
			byID[m.ID] = existing
		} else {
			m.Upstreams = []config.Upstream{config.UpstreamOllama}
			byID[m.ID] = m
			order = append(order, m.ID)
		}
	}

	out := make([]sourceModel, 0, len(order))
	for _, id := range order {
		out = append(out, byID[id])
	}
	return out
}

// appendUniqueUpstream adds an upstream to the slice if not already present.
func appendUniqueUpstream(slice []config.Upstream, u config.Upstream) []config.Upstream {
	for _, existing := range slice {
		if existing == u {
			return slice
		}
	}
	return append(slice, u)
}

// buildMergedRoute constructs a ModelRoute from a merged sourceModel entry,
// preserving admin-customized fields from the existing DB row.
func buildMergedRoute(sm sourceModel, existingRow store.ModelRouteRow, existed bool) config.ModelRoute {
	// Determine primary upstream and group
	primaryUpstream := sm.Upstreams[0]
	// Prefer Go as primary when available (Go has richer metadata)
	for _, u := range sm.Upstreams {
		if u == config.UpstreamGo {
			primaryUpstream = config.UpstreamGo
			break
		}
	}
	group := "go"
	if primaryUpstream == config.UpstreamOllama {
		group = "ollama"
	}
	// If Go is in the list, group should be "go" (it has keys)
	for _, u := range sm.Upstreams {
		if u == config.UpstreamGo {
			group = "go"
			break
		}
	}

	fallback := config.ModelRoute{
		ID:        sm.ID,
		Name:      sm.Name,
		Upstream:  primaryUpstream,
		Upstreams: sm.Upstreams,
		Protocol:  inferProtocol(sm.ID, nil),
		RealModel: sm.ID,
		Group:     group,
		Status:    config.ModelStatusPtr(config.ModelStatusEnabled),
		Tags:      fallbackTags(config.ModelRoute{ID: sm.ID, Name: sm.Name, RealModel: sm.ID}),
	}
	if fallback.Name == "" {
		fallback.Name = sm.ID
	}

	if !existed {
		return fallback
	}

	route := store.ModelRouteFromRow(existingRow)
	route.ID = sm.ID
	// Preserve the existing Upstream and Upstreams — do NOT unconditionally
	// overwrite them with the merged source. An admin may have configured
	// a multi-upstream route (e.g. [Ollama, Go]) or a non-Go primary
	// upstream. Only set a default if the existing route has no upstream.
	if strings.TrimSpace(string(route.Upstream)) == "" {
		route.Upstream = primaryUpstream
	}
	if len(route.Upstreams) == 0 {
		route.Upstreams = sm.Upstreams
	}
	if strings.TrimSpace(route.Group) == "" {
		route.Group = group
	}
	if route.Status == nil {
		route.Status = config.ModelStatusPtr(config.ModelStatusEnabled)
	}
	if !config.IsModelFieldCustomized(route, "name") {
		route.Name = fallback.Name
	}
	if !config.IsModelFieldCustomized(route, "protocol") {
		route.Protocol = fallback.Protocol
	}
	if !config.IsModelFieldCustomized(route, "real_model") {
		route.RealModel = fallback.RealModel
	}
	if !config.IsModelFieldCustomized(route, "tags") && len(route.Tags) == 0 {
		route.Tags = fallback.Tags
	}
	if route.Name == "" {
		route.Name = fallback.Name
	}
	if route.Protocol == "" {
		route.Protocol = fallback.Protocol
	}
	if route.RealModel == "" {
		route.RealModel = fallback.RealModel
	}
	return route
}

// ReloadRuntimeFromStore replaces the in-memory model route table from SQLite.
func ReloadRuntimeFromStore() error {
	rows, err := store.LoadModelRoutes()
	if err != nil {
		return fmt.Errorf("load model routes: %w", err)
	}
	routes := make([]config.ModelRoute, 0, len(rows))
	for _, row := range rows {
		routes = append(routes, store.ModelRouteFromRow(row))
	}
	config.ReplaceModels(routes)
	return nil
}

// StartBackground periodically syncs the model catalog until ctx is cancelled.
func StartBackground(ctx context.Context, interval time.Duration, opts Options, onResult func(Result, error)) {
	if interval <= 0 {
		return
	}
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				result, err := Sync(ctx, opts)
				if onResult != nil {
					onResult(result, err)
				}
			}
		}
	}()
}

func withDefaults(opts Options) Options {
	if strings.TrimSpace(opts.OpenCodeModelsURL) == "" {
		opts.OpenCodeModelsURL = defaultOpenCodeModelsURL
	}
	if strings.TrimSpace(opts.OpenRouterModelsURL) == "" {
		opts.OpenRouterModelsURL = defaultOpenRouterModelsURL
	}
	if strings.TrimSpace(opts.OllamaModelsURL) == "" {
		opts.OllamaModelsURL = defaultOllamaModelsURL
	}
	if opts.Client == nil {
		opts.Client = &http.Client{Timeout: 15 * time.Second}
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	return opts
}

func fetchOpenCodeModels(ctx context.Context, client *http.Client, url string) ([]openCodeModel, error) {
	var payload openCodePayload
	if err := fetchJSON(ctx, client, url, &payload); err != nil {
		return nil, fmt.Errorf("fetch opencode models: %w", err)
	}
	models := payload.Data
	sort.SliceStable(models, func(i, j int) bool { return models[i].ID < models[j].ID })
	return models, nil
}

func fetchOllamaModels(ctx context.Context, client *http.Client, url string) ([]ollamaModel, error) {
	var payload ollamaTagsPayload
	if err := fetchJSON(ctx, client, url, &payload); err != nil {
		return nil, fmt.Errorf("fetch ollama models: %w", err)
	}
	models := payload.Models
	sort.SliceStable(models, func(i, j int) bool { return models[i].Name < models[j].Name })
	return models, nil
}

func fetchOpenRouterModels(ctx context.Context, client *http.Client, url string) ([]config.OpenRouterModel, error) {
	var payload openRouterPayload
	if err := fetchJSON(ctx, client, url, &payload); err != nil {
		return nil, fmt.Errorf("fetch OpenRouter models: %w", err)
	}
	return payload.Data, nil
}

func fetchJSON(ctx context.Context, client *http.Client, url string, dst any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	if err := json.NewDecoder(resp.Body).Decode(dst); err != nil {
		return err
	}
	return nil
}

// inferProtocol guesses the upstream protocol from the model id when no
// DefaultModels seed is available. minimax-* and qwen* use Messages; all
// others use Chat.
func inferProtocol(id string, defaults map[string]config.ModelRoute) config.Protocol {
	if def, ok := defaults[id]; ok && def.Protocol != "" {
		return def.Protocol
	}
	lower := strings.ToLower(id)
	if strings.HasPrefix(lower, "minimax-") || strings.HasPrefix(lower, "qwen") {
		return config.ProtocolMessages
	}
	return config.ProtocolChat
}

func fallbackTags(route config.ModelRoute) []string {
	tags := []string{"text"}
	haystack := strings.ToLower(route.ID + " " + route.Name + " " + route.RealModel)
	if strings.Contains(haystack, "code") || strings.Contains(haystack, "coder") || strings.Contains(haystack, "coding") {
		tags = append(tags, "code")
	}
	if strings.Contains(haystack, "reason") || strings.Contains(haystack, "think") {
		tags = append(tags, "reasoning")
	}
	if strings.Contains(haystack, "omni") || strings.Contains(haystack, "vision") || strings.Contains(haystack, "vl") {
		tags = append(tags, "vision")
	}
	return config.NormalizeModelTags(tags)
}
