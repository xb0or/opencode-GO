package pool

import (
	"strings"
	"testing"
)

func TestGenerateTokenUsesSKPrefix(t *testing.T) {
	token := GenerateToken()
	if !strings.HasPrefix(token, "sk-") {
		t.Fatalf("token = %q, want sk- prefix", token)
	}
	if len(token) <= len("sk-") {
		t.Fatalf("token too short: %q", token)
	}
}
