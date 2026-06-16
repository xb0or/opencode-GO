package config

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
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

	// Go upstream base URL (without trailing slash).
	GoBaseURL string
}

var (
	cfg     *Config
	cfgOnce sync.Once
)

// Load reads configuration from environment variables with sensible defaults.
// It also loads .env file from the working directory (if exists) before reading env vars.
// It is safe to call multiple times; the first call wins.
func Load() *Config {
	cfgOnce.Do(func() {
		loadDotEnv()
		cfg = &Config{
			Port:            envStr("PORT", "9812"),
			AdminPassword:   envStr("ADMIN_PASSWORD", "admin"),
			JWTSecret:       envStr("JWT_SECRET", "opencode-sw-default-secret-change-me"),
			DBPath:          envStr("DB_PATH", "./data/opencode-sw.db"),
			UpstreamTimeout: envInt("UPSTREAM_TIMEOUT", 120),
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

// loadDotEnv reads .env file from the working directory and sets each
// KEY=VALUE line as an environment variable. Lines starting with # or
// empty lines are skipped. Existing env vars are NOT overridden.
func loadDotEnv() {
	dotPath := filepath.Join(".env")
	f, err := os.Open(dotPath)
	if err != nil {
		return // .env file not found, skip silently
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, val, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		val = strings.TrimSpace(val)
		val = strings.Trim(val, "\"'")
		if key != "" {
			// Only set if not already in environment
			if _, exists := os.LookupEnv(key); !exists {
				os.Setenv(key, val)
			}
		}
	}
}
