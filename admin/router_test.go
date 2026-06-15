package admin

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
	"github.com/opencode-sw/gateway/config"
	"github.com/opencode-sw/gateway/pool"
	"github.com/opencode-sw/gateway/store"
)

func TestMountWithPickerBindsPickerForHealth(t *testing.T) {
	if err := store.InitForTest("file::memory:?cache=shared"); err != nil {
		t.Fatalf("init test db: %v", err)
	}
	gin.SetMode(gin.TestMode)

	r := gin.New()
	MountWithPicker(r.Group("/admin"), pool.NewPicker())

	req := httptest.NewRequest(http.MethodGet, "/admin/health", nil)
	req.Header.Set("Authorization", "Bearer "+signedAdminToken(t))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("unexpected status: %d body=%s", w.Code, w.Body.String())
	}
}

func signedAdminToken(t *testing.T) string {
	t.Helper()
	claims := jwt.MapClaims{
		"role": "admin",
		"exp":  time.Now().Add(time.Hour).Unix(),
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	signed, err := tok.SignedString([]byte(config.Get().JWTSecret))
	if err != nil {
		t.Fatalf("sign token: %v", err)
	}
	return signed
}
