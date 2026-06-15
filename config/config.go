package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
)

// Config holds all runtime configuration loaded from environment / defaults.
type Config struct {
	Port            string
	AdminPassword   string
	JWTSecret       string
	DBPath          string
	UpstreamTimeout int // seconds

	// Zen / Go upstream base URLs (without trailing slash).
	ZenBaseURL string
	GoBaseURL  string
}

var (
	cfg     *Config
	cfgOnce sync.Once
)

// Load reads configuration from environment variables with sensible defaults.
// It is safe to call multiple times; the first call wins.
func Load() *Config {
	cfgOnce.Do(func() {
		cfg = &Config{
			Port:            envStr("PORT", "3000"),
			AdminPassword:   envStr("ADMIN_PASSWORD", "admin"),
			JWTSecret:       envStr("JWT_SECRET", "opencode-sw-default-secret-change-me"),
			DBPath:          envStr("DB_PATH", "./data/opencode-sw.db"),
			UpstreamTimeout: envInt("UPSTREAM_TIMEOUT", 120),
			ZenBaseURL:      strings.TrimRight(envStr("ZEN_BASE_URL", "https://opencode.ai/zen"), "/"),
			GoBaseURL:       strings.TrimRight(envStr("GO_BASE_URL", "https://opencode.ai/zen/go"), "/"),
		}
	})
	return cfg
}

// Get returns the already-loaded configuration (panics if Load was not called).
func Get() *Config {
	if cfg == nil {
		return Load()
	}
	return cfg
}

func envStr(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return def
}

func envInt(key string, def int) int {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

// DSN returns the SQLite connection string.
func (c *Config) DSN() string {
	return fmt.Sprintf("%s?_journal_mode=WAL&_busy_timeout=5000", c.DBPath)
}
