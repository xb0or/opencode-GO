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
		success bool
		warning string
	}

	results := make(chan fetchResult, 3)
	// Go upstream
	go func() {
		models, err := fetchOpenCodeModels(ctx, client, opts.OpenCodeModelsURL)
		if err != nil {
			results <- fetchResult{source: "go", success: false, warning: "opencode: " + err.Error()}
			return
		}
		out := make([]sourceModel, 0, len(models))
		for _, m := range models {
			out = append(out, sourceModel{ID: m.ID, Name: m.Name, Upstream: config.UpstreamGo})
		}
		results <- fetchResult{source: "go", success: true, models: out}
	}()

	// Ollama upstream
	go func() {
		models, err := fetchOllamaModels(ctx, client, opts.OllamaModelsURL)
		if err != nil {
			results <- fetchResult{source: "ollama", success: false, warning: "ollama: " + err.Error()}
			return
		}
		out := make([]sourceModel, 0, len(models))
		for _, m := range models {
			out = append(out, sourceModel{ID: m.Name, Name: m.Name, Upstream: config.UpstreamOllama})
		}
		results <- fetchResult{source: "ollama", success: true, models: out}
	}()

	// OpenRouter (for metadata enrichment, non-blocking)
	var openrouterModels []config.OpenRouterModel
	go func() {
		orm, err := fetchOpenRouterModels(ctx, client, opts.OpenRouterModelsURL)
		if err != nil {
			results <- fetchResult{source: "openrouter", success: false, warning: "openrouter: " + err.Error()}
			return
		}
		results <- fetchResult{source: "openrouter", success: true, models: nil}
		openrouterModels = orm
	}()

	// Collect results
	var goModels, ollamaModels []sourceModel
	var goFetched, ollamaFetched bool
	result := Result{}
	for i := 0; i < 3; i++ {
		r := <-results
		switch r.source {
		case "go":
			if !r.success {
				result.Warnings = append(result.Warnings, r.warning)
			} else {
				goModels = r.models
				result.OpenCodeCount = len(goModels)
			}
			goFetched = r.success
		case "ollama":
			if !r.success {
				result.Warnings = append(result.Warnings, r.warning)
			} else {
				ollamaModels = r.models
				result.OllamaCount = len(ollamaModels)
			}
			ollamaFetched = r.success
		case "openrouter":
			if !r.success {
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
		route := buildMergedRoute(sm, row, existed, goFetched, ollamaFetched)

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
// When a provider's catalog fetch failed (goFetched/ollamaFetched false),
// the existing upstream membership for that provider is preserved to
// prevent temporary fetch failures from deleting providers.
func buildMergedRoute(sm sourceModel, existingRow store.ModelRouteRow, existed bool, goFetched, ollamaFetched bool) config.ModelRoute {
	// Determine primary upstream and group from the merged source.
	primaryUpstream := config.UpstreamGo
	group := "go"
	if len(sm.Upstreams) > 0 {
		primaryUpstream = sm.Upstreams[0]
	}
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
	// Update Upstream/Upstreams from the merged source unless the admin
	// explicitly customized them. This allows automatic discovery of new
	// providers (e.g. a model that was Go-only is now also on Ollama) and
	// removal of providers that no longer serve the model.
	// When a provider's catalog fetch failed, preserve the existing
	// membership for that provider — a temporary fetch failure should
	// not be treated as authoritative model removal.
	if !config.IsModelFieldCustomized(route, "upstream") {
		if !upstreamFetchFailed(primaryUpstream, goFetched, ollamaFetched) {
			route.Upstream = primaryUpstream
		}
		// When the primary provider's fetch failed, keep existing upstream.
	}
	if !config.IsModelFieldCustomized(route, "upstreams") {
		// Per-provider membership: each provider's presence in the merged
		// result is authoritative only when its catalog fetch succeeded.
		// When a fetch failed, preserve the existing membership for that
		// provider — a temporary fetch failure should not be treated as
		// authoritative model removal.
		mergedHasGo := false
		mergedHasOllama := false
		for _, u := range sm.Upstreams {
			if u == config.UpstreamGo {
				mergedHasGo = true
			}
			if u == config.UpstreamOllama {
				mergedHasOllama = true
			}
		}
		existingHasGo := false
		existingHasOllama := false
		for _, u := range route.Upstreams {
			if u == config.UpstreamGo {
				existingHasGo = true
			}
			if u == config.UpstreamOllama {
				existingHasOllama = true
			}
		}

		// Build the final upstreams list: for each provider, use the merged
		// result if its fetch succeeded, otherwise preserve the existing state.
		var finalUpstreams []config.Upstream
		for _, candidate := range []config.Upstream{config.UpstreamGo, config.UpstreamOllama} {
			mergedHas := false
			existingHas := false
			fetchSucceeded := false
			switch candidate {
			case config.UpstreamGo:
				mergedHas = mergedHasGo
				existingHas = existingHasGo
				fetchSucceeded = goFetched
			case config.UpstreamOllama:
				mergedHas = mergedHasOllama
				existingHas = existingHasOllama
				fetchSucceeded = ollamaFetched
			}
			if fetchSucceeded {
				// Authoritative: include if merged result says so.
				if mergedHas {
					finalUpstreams = append(finalUpstreams, candidate)
				}
			} else {
				// Fetch failed: preserve existing membership.
				if existingHas {
					finalUpstreams = append(finalUpstreams, candidate)
				}
			}
		}
		route.Upstreams = finalUpstreams
	}
	if !config.IsModelFieldCustomized(route, "upstream_groups") {
		route.UpstreamGroups = nil
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

// upstreamFetchFailed returns true when the given upstream's catalog fetch
// did not succeed, meaning the merged result is incomplete for that provider.
func upstreamFetchFailed(upstream config.Upstream, goFetched, ollamaFetched bool) bool {
	switch upstream {
	case config.UpstreamGo:
		return !goFetched
	case config.UpstreamOllama:
		return !ollamaFetched
	default:
		return true
	}
}
