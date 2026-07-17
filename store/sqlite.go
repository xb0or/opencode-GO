package store

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/xb0or/opencode-GO/config"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// Key is an upstream OpenCode API key belonging to a pool group.
type Key struct {
	ID             uint       `gorm:"primaryKey" json:"id"`
	Value          string     `gorm:"uniqueIndex;size:255;not null" json:"value"`
	Group          string     `gorm:"index;size:32;not null" json:"group"` // go | custom
	Label          string     `gorm:"size:128" json:"label"`
	Enabled        bool       `gorm:"default:true" json:"enabled"`
	Weight         int        `gorm:"default:1" json:"weight"`
	ProxyURL       string     `gorm:"size:512" json:"proxy_url"`
	Cookie         string     `gorm:"size:1024" json:"cookie"`      // opencode.ai session cookie for quota
	WorkspaceID    string     `gorm:"size:128" json:"workspace_id"` // opencode.ai workspace ID for quota
	QuotaSnapshot  string     `gorm:"type:text" json:"-"`           // last quota query payload, persisted for admin UI
	QuotaUpdatedAt *time.Time `json:"quota_updated_at,omitempty"`
	FailCount      int        `json:"fail_count"`
	CooldownUntil  *time.Time `json:"cooldown_until,omitempty"`
	LastUsed       *time.Time `json:"last_used,omitempty"`
	UsageCount     int64      `gorm:"default:0" json:"usage_count"`
	CreatedAt      time.Time  `json:"created_at"`
	UpdatedAt      time.Time  `json:"updated_at"`
}

// Token is a gateway-facing credential a client uses to access the gateway.
type Token struct {
	ID            uint       `gorm:"primaryKey" json:"id"`
	Token         string     `gorm:"uniqueIndex;size:128;not null" json:"token"`
	Name          string     `gorm:"size:128" json:"name"`
	Description   string     `gorm:"size:512" json:"description"`
	Enabled       bool       `gorm:"default:true" json:"enabled"`
	AllowedGroups string     `gorm:"size:255" json:"allowed_groups"` // comma-separated; empty = all
	RateLimit     int        `gorm:"default:0" json:"rate_limit"`    // req/min, 0 = unlimited
	MaxRequests   int        `gorm:"default:0" json:"max_requests"`  // total request cap, 0 = unlimited
	RequestsUsed  int        `gorm:"default:0" json:"requests_used"` // running count of proxied requests
	ExpiresAt     *time.Time `json:"expires_at,omitempty"`
	CreatedAt     time.Time  `json:"created_at"`
	UpdatedAt     time.Time  `json:"updated_at"`
}

// ModelRouteRow persists a model route in the database.
type ModelRouteRow struct {
	ID                      string     `gorm:"primaryKey;size:128" json:"id"`
	Name                    string     `gorm:"size:128" json:"name"`
	Upstream                string     `gorm:"size:32;not null" json:"upstream"`
	UpstreamsJSON           string     `gorm:"type:text" json:"upstreams_json,omitempty"`          // JSON array of Upstream for multi-upstream failover
	UpstreamGroupsJSON      string     `gorm:"type:text" json:"upstream_groups_json,omitempty"`    // JSON map[Upstream]string for per-upstream group overrides
	Protocol                string     `gorm:"size:32;not null" json:"protocol"`
	RealModel               string     `gorm:"size:255;not null" json:"real_model"`
	Group                   string     `gorm:"size:32;not null" json:"group"`
	ContextLen              int        `json:"context_len"`
	Capabilities            string     `gorm:"size:512" json:"capabilities,omitempty"` // legacy JSON array string
	Status                  int        `gorm:"default:1" json:"status"`                // 0 disabled, 1 enabled
	Priority                int        `gorm:"default:0" json:"priority"`              // admin-defined ordering hint
	TagsJSON                string     `gorm:"type:text" json:"tags_json,omitempty"`
	PricingJSON             string     `gorm:"type:text" json:"pricing_json,omitempty"`
	ArchitectureJSON        string     `gorm:"type:text" json:"architecture_json,omitempty"`
	SupportedParametersJSON string     `gorm:"type:text" json:"supported_parameters_json,omitempty"`
	OpenRouterID            string     `gorm:"size:255" json:"openrouter_id,omitempty"`
	OpenRouterName          string     `gorm:"size:255" json:"openrouter_name,omitempty"`
	OpenRouterMatchedBy     string     `gorm:"size:64" json:"openrouter_matched_by,omitempty"`
	Description             string     `gorm:"type:text" json:"description,omitempty"`
	KnowledgeCutoff         string     `gorm:"size:64" json:"knowledge_cutoff,omitempty"`
	IsCustomized            bool       `gorm:"default:false" json:"is_customized"`
	CustomizedFieldsJSON    string     `gorm:"type:text" json:"customized_fields_json,omitempty"`
	LastSyncedAt            *time.Time `json:"last_synced_at,omitempty"`
	CreatedAt               time.Time  `json:"created_at"`
	UpdatedAt               time.Time  `json:"updated_at"`
}

// ModelMappingRow persists a UI-managed model rewrite rule.
type ModelMappingRow struct {
	SourceModel string    `gorm:"primaryKey;size:255" json:"source_model"`
	TargetModel string    `gorm:"size:255;not null" json:"target_model"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// UsageLog records a single proxied request.
type UsageLog struct {
	ID                  uint      `gorm:"primaryKey" json:"id"`
	RequestID           string    `gorm:"size:128;index" json:"request_id"`
	TokenID             uint      `gorm:"index" json:"token_id"`
	TokenName           string    `gorm:"size:128;index" json:"token_name"`
	KeyID               uint      `gorm:"index" json:"key_id"`
	Model               string    `gorm:"size:128;index" json:"model"`
	Group               string    `gorm:"size:32;index" json:"group"`
	Protocol            string    `gorm:"size:32" json:"protocol"`
	IPAddress           string    `gorm:"size:64" json:"ip_address"`
	StatusCode          int       `json:"status_code"`
	DurationMs          int64     `json:"duration_ms"`
	FirstResponseMs     int64     `json:"first_response_ms"`
	Stream              bool      `json:"stream"`
	InputTokens         int       `json:"input_tokens"`
	OutputTokens        int       `json:"output_tokens"`
	ReasoningTokens     int       `json:"reasoning_tokens"`
	CacheTokens         int       `json:"cache_tokens"`
	CacheReadTokens     int       `json:"cache_read_tokens"`
	CacheCreationTokens int       `json:"cache_creation_tokens"`
	TotalTokens         int       `json:"total_tokens"`
	TotalCost           float64   `json:"total_cost"`
	ActualCost          float64   `json:"actual_cost"`
	AccountCost         float64   `json:"account_cost"`
	InputUnitPrice      float64   `json:"input_unit_price"`
	OutputUnitPrice     float64   `json:"output_unit_price"`
	CacheReadUnitPrice  float64   `json:"cache_read_unit_price"`
	CacheWriteUnitPrice float64   `json:"cache_write_unit_price"`
	GroupMultiplier     float64   `json:"group_multiplier"`
	BillingMode         string    `gorm:"size:32" json:"billing_mode"`
	Error               string    `gorm:"type:text" json:"error,omitempty"`
	CreatedAt           time.Time `gorm:"index" json:"created_at"`
}

var (
	db *gorm.DB
)

// Init opens the SQLite database (creating the parent dir) and auto-migrates
// the schema. It is meant to be called once at startup.
func Init() error {
	cfg := config.Get()
	if dir := filepath.Dir(cfg.DBPath); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("create db dir: %w", err)
		}
	}
	gdb, err := gorm.Open(sqlite.Open(cfg.DSN()), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Warn),
	})
	if err != nil {
		return fmt.Errorf("open sqlite: %w", err)
	}
	if err := gdb.AutoMigrate(&Key{}, &Token{}, &UsageLog{}, &ModelRouteRow{}, &ModelMappingRow{}); err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	db = gdb
	return nil
}

// DB returns the global gorm handle. Panics if Init was not called.
func DB() *gorm.DB {
	if db == nil {
		panic("store.Init() not called")
	}
	return db
}

// LoadModelRoutes loads all model routes from the database.
func LoadModelRoutes() ([]ModelRouteRow, error) {
	var rows []ModelRouteRow
	return rows, db.Order("id asc").Find(&rows).Error
}

// SaveModelRoute upserts a model route.
func SaveModelRoute(r *ModelRouteRow) error {
	return db.Save(r).Error
}

// ModelRouteFromRow converts a persisted route row into the runtime config
// representation, including JSON-encoded metadata fields.
func ModelRouteFromRow(r ModelRouteRow) config.ModelRoute {
	status := r.Status
	if status != config.ModelStatusDisabled {
		status = config.ModelStatusEnabled
	}
	route := config.ModelRoute{
		ID:                  strings.TrimSpace(r.ID),
		Name:                strings.TrimSpace(r.Name),
		Upstream:            config.Upstream(strings.TrimSpace(r.Upstream)),
		Upstreams:           decodeUpstreams(r.UpstreamsJSON),
		UpstreamGroups:      decodeUpstreamGroups(r.UpstreamGroupsJSON),
		Protocol:            config.Protocol(strings.TrimSpace(r.Protocol)),
		RealModel:           strings.TrimSpace(r.RealModel),
		Group:               strings.TrimSpace(r.Group),
		ContextLen:          r.ContextLen,
		Status:              config.ModelStatusPtr(status),
		Priority:            r.Priority,
		Tags:                routeTagsFromRow(r),
		Pricing:             decodeStringMap(r.PricingJSON),
		SupportedParameters: decodeStringSlice(r.SupportedParametersJSON),
		OpenRouterID:        r.OpenRouterID,
		OpenRouterName:      r.OpenRouterName,
		OpenRouterMatchedBy: r.OpenRouterMatchedBy,
		Description:         r.Description,
		KnowledgeCutoff:     r.KnowledgeCutoff,
		IsCustomized:        r.IsCustomized,
		CustomizedFields:    config.NormalizeCustomizedFields(decodeStringSlice(r.CustomizedFieldsJSON)),
	}
	if strings.TrimSpace(r.ArchitectureJSON) != "" {
		var arch config.ModelArchitecture
		if err := json.Unmarshal([]byte(r.ArchitectureJSON), &arch); err == nil {
			route.Architecture = &arch
		}
	}
	if route.Upstream == "" {
		route.Upstream = config.UpstreamGo
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
	route.IsCustomized = route.IsCustomized || len(route.CustomizedFields) > 0
	return route
}

func routeTagsFromRow(r ModelRouteRow) []string {
	tags := decodeStringSlice(r.TagsJSON)
	if len(tags) == 0 {
		tags = decodeStringSlice(r.Capabilities)
	}
	return config.NormalizeModelTags(tags)
}

// NewModelRouteRow converts a runtime route into a DB row.
func NewModelRouteRow(m config.ModelRoute) ModelRouteRow {
	status := config.ModelStatusEnabled
	if m.Status != nil && *m.Status == config.ModelStatusDisabled {
		status = config.ModelStatusDisabled
	}
	if m.Upstream == "" {
		m.Upstream = config.UpstreamGo
	}
	if m.Protocol == "" {
		m.Protocol = config.ProtocolChat
	}
	if strings.TrimSpace(m.Group) == "" {
		m.Group = "go"
	}
	if strings.TrimSpace(m.RealModel) == "" {
		m.RealModel = m.ID
	}
	if strings.TrimSpace(m.Name) == "" {
		m.Name = m.ID
	}
	return ModelRouteRow{
		ID:                      strings.TrimSpace(m.ID),
		Name:                    strings.TrimSpace(m.Name),
		Upstream:                string(m.Upstream),
		UpstreamsJSON:           encodeUpstreams(m.Upstreams),
		UpstreamGroupsJSON:      encodeUpstreamGroups(m.UpstreamGroups),
		Protocol:                string(m.Protocol),
		RealModel:               strings.TrimSpace(m.RealModel),
		Group:                   strings.TrimSpace(m.Group),
		ContextLen:              m.ContextLen,
		Status:                  status,
		Priority:                m.Priority,
		TagsJSON:                encodeJSON(config.NormalizeModelTags(m.Tags)),
		Capabilities:            encodeJSON(config.NormalizeModelTags(m.Tags)),
		PricingJSON:             encodeJSON(m.Pricing),
		ArchitectureJSON:        encodeJSON(m.Architecture),
		SupportedParametersJSON: encodeJSON(m.SupportedParameters),
		OpenRouterID:            m.OpenRouterID,
		OpenRouterName:          m.OpenRouterName,
		OpenRouterMatchedBy:     m.OpenRouterMatchedBy,
		Description:             m.Description,
		KnowledgeCutoff:         m.KnowledgeCutoff,
		IsCustomized:            m.IsCustomized || len(m.CustomizedFields) > 0,
		CustomizedFieldsJSON:    encodeJSON(config.NormalizeCustomizedFields(m.CustomizedFields)),
	}
}

// DeleteModelRoute deletes a model route by id.
func DeleteModelRoute(id string) error {
	return db.Delete(&ModelRouteRow{}, "id = ?", id).Error
}

// LoadModelMappings loads all persisted model rewrite rules from the database.
func LoadModelMappings() ([]ModelMappingRow, error) {
	var rows []ModelMappingRow
	return rows, db.Order("source_model asc").Find(&rows).Error
}

// SaveModelMapping upserts a model rewrite rule.
func SaveModelMapping(r *ModelMappingRow) error {
	return db.Save(r).Error
}

// DeleteModelMapping deletes a model rewrite rule by source model id.
func DeleteModelMapping(sourceModel string) error {
	return db.Delete(&ModelMappingRow{}, "source_model = ?", sourceModel).Error
}

func encodeJSON(v any) string {
	if v == nil {
		return ""
	}
	b, err := json.Marshal(v)
	if err != nil || string(b) == "null" || string(b) == "[]" || string(b) == "{}" {
		return ""
	}
	return string(b)
}

func decodeStringSlice(raw string) []string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	var out []string
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return nil
	}
	return out
}

func decodeStringMap(raw string) map[string]string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	var out map[string]string
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return nil
	}
	return out
}

func decodeUpstreams(raw string) []config.Upstream {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	var out []config.Upstream
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return nil
	}
	return out
}

func decodeUpstreamGroups(raw string) map[config.Upstream]string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	var out map[config.Upstream]string
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return nil
	}
	return out
}

func encodeUpstreams(upstreams []config.Upstream) string {
	if len(upstreams) == 0 {
		return ""
	}
	b, err := json.Marshal(upstreams)
	if err != nil {
		return ""
	}
	return string(b)
}

func encodeUpstreamGroups(groups map[config.Upstream]string) string {
	if len(groups) == 0 {
		return ""
	}
	b, err := json.Marshal(groups)
	if err != nil {
		return ""
	}
	return string(b)
}

// InitForTest opens an in-memory SQLite for testing. Not for production use.
func InitForTest(dsn string) error {
	gdb, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Warn),
	})
	if err != nil {
		return err
	}
	if err := gdb.AutoMigrate(&Key{}, &Token{}, &UsageLog{}, &ModelRouteRow{}, &ModelMappingRow{}); err != nil {
		return err
	}
	db = gdb
	return nil
}
