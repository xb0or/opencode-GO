package admin

import (
	"bytes"
	"encoding/json"
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

func TestUpdateKeySettingsMasksReturnedValue(t *testing.T) {
	if err := store.InitForTest("file:admin_update_key?mode=memory&cache=shared"); err != nil {
		t.Fatalf("init test db: %v", err)
	}
	gin.SetMode(gin.TestMode)

	key := &store.Key{Value: "original-secret", Group: "go", Label: "old", Enabled: true, Weight: 1}
	if err := store.DB().Create(key).Error; err != nil {
		t.Fatalf("create key: %v", err)
	}

	r := gin.New()
	MountWithPicker(r.Group("/admin"), pool.NewPicker())

	body := bytes.NewBufferString(`{"value":"new-secret-value","label":"new","weight":3,"proxy_url":"http://proxy:8080"}`)
	req := httptest.NewRequest(http.MethodPatch, "/admin/keys/1", body)
	req.Header.Set("Authorization", "Bearer "+signedAdminToken(t))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("unexpected status: %d body=%s", w.Code, w.Body.String())
	}
	var got store.Key
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if got.Value == "new-secret-value" {
		t.Fatal("updated key value should be masked in response")
	}

	var saved store.Key
	store.DB().First(&saved, key.ID)
	if saved.Value != "new-secret-value" || saved.Label != "new" || saved.Weight != 3 || saved.ProxyURL != "http://proxy:8080" {
		t.Fatalf("key not updated: %#v", saved)
	}
}

func TestListTokensReturnsCopyableSKToken(t *testing.T) {
	if err := store.InitForTest("file:admin_token_list?mode=memory&cache=shared"); err != nil {
		t.Fatalf("init test db: %v", err)
	}
	gin.SetMode(gin.TestMode)

	tok, err := pool.CreateToken("copyable", "", 0, nil)
	if err != nil {
		t.Fatalf("create token: %v", err)
	}
	if tok.Token[:3] != "sk-" {
		t.Fatalf("token prefix = %q, want sk-", tok.Token[:3])
	}

	r := gin.New()
	MountWithPicker(r.Group("/admin"), pool.NewPicker())

	req := httptest.NewRequest(http.MethodGet, "/admin/tokens", nil)
	req.Header.Set("Authorization", "Bearer "+signedAdminToken(t))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("unexpected status: %d body=%s", w.Code, w.Body.String())
	}
	var payload struct {
		Data []store.Token `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(payload.Data) != 1 || payload.Data[0].Token != tok.Token {
		t.Fatalf("token should be returned unmasked for copy: %#v", payload.Data)
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
