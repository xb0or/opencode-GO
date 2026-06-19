package store

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/opencode-sw/gateway/config"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// Key is an upstream OpenCode API key belonging to a pool group.
type Key struct {
	ID            uint       `gorm:"primaryKey" json:"id"`
	Value         string     `gorm:"uniqueIndex;size:255;not null" json:"value"`
	Group         string     `gorm:"index;size:32;not null" json:"group"` // go | custom
	Label         string     `gorm:"size:128" json:"label"`
	Enabled       bool       `gorm:"default:true" json:"enabled"`
	Weight        int        `gorm:"default:1" json:"weight"`
	ProxyURL      string     `gorm:"size:512" json:"proxy_url"`
	FailCount     int        `json:"fail_count"`
	CooldownUntil *time.Time `json:"cooldown_until,omitempty"`
	LastUsed      *time.Time `json:"last_used,omitempty"`
	UsageCount    int64      `gorm:"default:0" json:"usage_count"`
	CreatedAt     time.Time  `json:"created_at"`
	UpdatedAt     time.Time  `json:"updated_at"`
}

// Token is a gateway-facing credential a client uses to access the gateway.
type Token struct {
	ID            uint       `gorm:"primaryKey" json:"id"`
	Token         string     `gorm:"uniqueIndex;size:128;not null" json:"token"`
	Name          string     `gorm:"size:128" json:"name"`
	Enabled       bool       `gorm:"default:true" json:"enabled"`
	AllowedGroups string     `gorm:"size:255" json:"allowed_groups"` // comma-separated; empty = all
	RateLimit     int        `gorm:"default:0" json:"rate_limit"`    // req/min, 0 = unlimited
	ExpiresAt     *time.Time `json:"expires_at,omitempty"`
	CreatedAt     time.Time  `json:"created_at"`
	UpdatedAt     time.Time  `json:"updated_at"`
}

// ModelRouteRow persists a model route in the database.
type ModelRouteRow struct {
	ID           string `gorm:"primaryKey;size:128" json:"id"`
	Name         string `gorm:"size:128" json:"name"`
	Upstream     string `gorm:"size:32;not null" json:"upstream"`
	Protocol     string `gorm:"size:32;not null" json:"protocol"`
	RealModel    string `gorm:"size:255;not null" json:"real_model"`
	Group        string `gorm:"size:32;not null" json:"group"`
	ContextLen   int    `json:"context_len"`
	Capabilities string `gorm:"size:512" json:"capabilities"` // JSON array string
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
