package pool

import (
	"crypto/rand"
	"encoding/hex"
	"strings"
	"time"

	"github.com/opencode-sw/gateway/store"
)

// GenerateToken returns a random gateway token with a recognizable prefix.
func GenerateToken() string {
	b := make([]byte, 20)
	_, _ = rand.Read(b)
	return "sk-" + hex.EncodeToString(b)
}

// FindToken loads a gateway token by its value; returns nil if not found.
func FindToken(value string) *store.Token {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}
	var t store.Token
	if err := store.DB().Where("token = ?", value).First(&t).Error; err != nil {
		return nil
	}
	return &t
}

// IsExpired reports whether a token has passed its expiry (false if no expiry).
func IsExpired(t *store.Token) bool {
	if t == nil || t.ExpiresAt == nil {
		return false
	}
	return time.Now().After(*t.ExpiresAt)
}

// GroupAllowed reports whether the token may use the given group. An empty
// AllowedGroups means "all groups allowed".
func GroupAllowed(t *store.Token, group string) bool {
	if t.AllowedGroups == "" {
		return true
	}
	for _, g := range strings.Split(t.AllowedGroups, ",") {
		if strings.TrimSpace(g) == group {
			return true
		}
	}
	return false
}

// AllTokens returns every gateway token (for admin UI).
func AllTokens() ([]store.Token, error) {
	var ts []store.Token
	return ts, store.DB().Order("id desc").Find(&ts).Error
}

// CreateToken inserts a new gateway token.
func CreateToken(name, allowedGroups string, rateLimit int, expiresAt *time.Time) (*store.Token, error) {
	t := &store.Token{
		Token:         GenerateToken(),
		Name:          name,
		Enabled:       true,
		AllowedGroups: allowedGroups,
		RateLimit:     rateLimit,
		ExpiresAt:     expiresAt,
	}
	if err := store.DB().Create(t).Error; err != nil {
		return nil, err
	}
	return t, nil
}
