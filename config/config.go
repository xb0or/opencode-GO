package config

import (
	"bufio"
	"encoding/json"
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
	UpstreamTimeout int // seconds, 0 = no gateway deadline

	// Go upstream base URL (without trailing slash).
	GoBaseURL string
	// Ollama Cloud API base URL (without trailing slash).
	// Ollama Cloud exposes an OpenAI-compatible /v1/chat/completions endpoint
	// which the gateway proxies to transparently.
	OllamaBaseURL string

	// ModelMappings is an optional JSON object that rewrites client-facing
	// model names before forwarding, for example {"gpt-5.5":"glm-5.1"}.
	ModelMappings string
	// ModelMappingFile optionally points to a JSON file with the same mapping
	// object shape as ModelMappings.
	ModelMappingFile string
	// GroupMultipliers optionally maps KEY/token groups to billing multipliers.
	// Supported formats:
	//   - JSON object: {"go":0.8,"default":1}
	//   - comma list:  go=0.8,default=1
	GroupMultipliers string

	// PassthroughMode controls how unknown (unregistered) models are handled:
	//   "go"       — forward to Go upstream (legacy behavior, NOT recommended)
	//   "disabled" — return 404 for unknown models (DEFAULT, strict mode)
	PassthroughMode string
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
			Port:             envStr("PORT", "9812"),
			AdminPassword:    envStr("ADMIN_PASSWORD", "admin"),
			JWTSecret:        envStr("JWT_SECRET", "opencode-go-default-secret-change-me"),
			DBPath:           envStr("DB_PATH", "./data/opencode-sw.db"),
			UpstreamTimeout:  envInt("UPSTREAM_TIMEOUT", 0),
			GoBaseURL:        strings.TrimRight(envStr("GO_BASE_URL", "https://opencode.ai/zen/go"), "/"),
			OllamaBaseURL:    strings.TrimRight(envStr("OLLAMA_BASE_URL", "https://ollama.com"), "/"),
			ModelMappings:    envStr("MODEL_MAPPINGS", ""),
			ModelMappingFile: envStr("MODEL_MAPPING_FILE", ""),
			GroupMultipliers: envStr("GROUP_MULTIPLIERS", ""),
			PassthroughMode:  envStr("PASSTHROUGH_MODE", "disabled"),
		}
	})
	return cfg
}

// ValidateSecurity checks for insecure default configuration and returns an
// error if the deployment is not production-safe. This should be called at
// startup; the process should refuse to start if any check fails.
//
// Checks:
//   - ADMIN_PASSWORD must not be the default "admin"
//   - JWT_SECRET must not be the built-in default
//   - JWT_SECRET must be at least 32 bytes
func ValidateSecurity() error {
	c := Get()
	if c.AdminPassword == "admin" {
		return fmt.Errorf("insecure configuration: ADMIN_PASSWORD is still the default 'admin' — set ADMIN_PASSWORD to a strong password")
	}
	if c.JWTSecret == "opencode-go-default-secret-change-me" {
		return fmt.Errorf("insecure configuration: JWT_SECRET is still the built-in default — set JWT_SECRET to a random string of at least 32 bytes")
	}
	if len(c.JWTSecret) < 32 {
		return fmt.Errorf("insecure configuration: JWT_SECRET must be at least 32 bytes (current: %d) — use a random string", len(c.JWTSecret))
	}
	return nil
}

// Get returns the already-loaded configuration (panics if Load was not called).
func Get() *Config {
	if cfg == nil {
		return Load()
	}
	return cfg
}

// GroupMultiplier returns the billing multiplier for a route/key group.
// Missing, malformed, zero, or negative values fall back to 1.0.
func GroupMultiplier(group string) float64 {
	group = strings.TrimSpace(group)
	if group == "" {
		group = "default"
	}
	multipliers := parseGroupMultipliers(Get().GroupMultipliers)
	for _, key := range []string{group, "default"} {
		if v, ok := multipliers[key]; ok && v > 0 {
			return v
		}
	}
	return 1
}

func parseGroupMultipliers(raw string) map[string]float64 {
	out := map[string]float64{}
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return out
	}
	var obj map[string]float64
	if err := json.Unmarshal([]byte(raw), &obj); err == nil {
		for k, v := range obj {
			k = strings.TrimSpace(k)
			if k != "" && v > 0 {
				out[k] = v
			}
		}
		return out
	}
	for _, part := range strings.Split(raw, ",") {
		key, val, ok := strings.Cut(part, "=")
		if !ok {
			key, val, ok = strings.Cut(part, ":")
		}
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		n, err := strconv.ParseFloat(strings.TrimSpace(val), 64)
		if key != "" && err == nil && n > 0 {
			out[key] = n
		}
	}
	return out
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
