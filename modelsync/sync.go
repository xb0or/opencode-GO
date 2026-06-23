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
)

// Options configures a model catalog synchronization run.
type Options struct {
	OpenCodeModelsURL   string
	OpenRouterModelsURL string
	Client              *http.Client
	Now                 func() time.Time
}

// Result summarizes a synchronization run.
type Result struct {
	OpenCodeCount   int      `json:"opencode_count"`
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

type openRouterPayload struct {
	Data []config.OpenRouterModel `json:"data"`
}

// Sync fetches the OpenCode Go model list, enriches it with OpenRouter metadata,
// persists the merged catalog, and refreshes the runtime route table. Admin
// customized fields are preserved according to each row's customized_fields set.
func Sync(ctx context.Context, opts Options) (Result, error) {
	opts = withDefaults(opts)
	client := opts.Client

	opencodeModels, err := fetchOpenCodeModels(ctx, client, opts.OpenCodeModelsURL)
	if err != nil {
		return Result{}, err
	}

	result := Result{OpenCodeCount: len(opencodeModels)}
	openrouterModels, err := fetchOpenRouterModels(ctx, client, opts.OpenRouterModelsURL)
	if err != nil {
		result.Warnings = append(result.Warnings, "openrouter: "+err.Error())
	} else {
		result.OpenRouterCount = len(openrouterModels)
	}

	existingRows, err := store.LoadModelRoutes()
	if err != nil {
		return result, fmt.Errorf("load model routes: %w", err)
	}
	existingByID := make(map[string]store.ModelRouteRow, len(existingRows))
	for _, row := range existingRows {
		existingByID[row.ID] = row
	}

	defaultByID := map[string]config.ModelRoute{}
	for _, route := range config.DefaultModels() {
		defaultByID[route.ID] = route
	}

	now := opts.Now()
	seen := map[string]bool{}
	for _, source := range opencodeModels {
		id := strings.TrimSpace(source.ID)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true

		row, existed := existingByID[id]
		route := buildSyncedRoute(source, row, existed, defaultByID)
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

// ReloadRuntimeFromStore replaces the in-memory model route table from SQLite.
func ReloadRuntimeFromStore() error {
	rows, err := store.LoadModelRoutes()
	if err != nil {
		return fmt.Errorf("load model routes: %w", err)
	}
	routes := make([]config.ModelRoute, 0, len(rows))
	for _, row := range rows {
		if row.Upstream != "" && row.Upstream != string(config.UpstreamGo) {
			continue
		}
		if row.Group != "" && row.Group != "go" {
			continue
		}
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

func buildSyncedRoute(source openCodeModel, existingRow store.ModelRouteRow, existed bool, defaults map[string]config.ModelRoute) config.ModelRoute {
	id := strings.TrimSpace(source.ID)
	fallback := config.ModelRoute{
		ID:        id,
		Name:      displayNameFor(source),
		Upstream:  config.UpstreamGo,
		Protocol:  inferProtocol(id, defaults),
		RealModel: id,
		Group:     "go",
		Status:    config.ModelStatusPtr(config.ModelStatusEnabled),
		Tags:      fallbackTags(config.ModelRoute{ID: id, Name: displayNameFor(source), RealModel: id}),
	}
	if def, ok := defaults[id]; ok {
		fallback.Name = def.Name
		fallback.Protocol = def.Protocol
		fallback.RealModel = def.RealModel
	}
	if !existed {
		return fallback
	}

	route := store.ModelRouteFromRow(existingRow)
	route.ID = id
	route.Upstream = config.UpstreamGo
	if strings.TrimSpace(route.Group) == "" {
		route.Group = "go"
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

func displayNameFor(source openCodeModel) string {
	if name := strings.TrimSpace(source.Name); name != "" {
		return name
	}
	parts := strings.FieldsFunc(source.ID, func(r rune) bool { return r == '-' || r == '_' })
	for i, part := range parts {
		lower := strings.ToLower(part)
		switch lower {
		case "glm", "kimi", "mimo", "qwen", "hy3":
			parts[i] = strings.ToUpper(part)
		default:
			if len(part) > 0 {
				parts[i] = strings.ToUpper(part[:1]) + part[1:]
			}
		}
	}
	if len(parts) == 0 {
		return source.ID
	}
	return strings.Join(parts, " ")
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
