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

// UsageLog records a single proxied request.
type UsageLog struct {
	ID         uint      `gorm:"primaryKey" json:"id"`
	TokenID    uint      `gorm:"index" json:"token_id"`
	TokenName  string    `gorm:"size:128;index" json:"token_name"`
	KeyID      uint      `gorm:"index" json:"key_id"`
	Model      string    `gorm:"size:128;index" json:"model"`
	Protocol   string    `gorm:"size:32" json:"protocol"`
	StatusCode int       `json:"status_code"`
	DurationMs int64     `json:"duration_ms"`
	Stream     bool      `json:"stream"`
	Error      string    `gorm:"type:text" json:"error,omitempty"`
	CreatedAt  time.Time `gorm:"index" json:"created_at"`
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
	if err := gdb.AutoMigrate(&Key{}, &Token{}, &UsageLog{}, &ModelRouteRow{}); err != nil {
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

// InitForTest opens an in-memory SQLite for testing. Not for production use.
func InitForTest(dsn string) error {
	gdb, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Warn),
	})
	if err != nil {
		return err
	}
	if err := gdb.AutoMigrate(&Key{}, &Token{}, &UsageLog{}, &ModelRouteRow{}); err != nil {
		return err
	}
	db = gdb
	return nil
}
