package api

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/opencode-sw/gateway/config"
	"github.com/opencode-sw/gateway/pool"
	"github.com/opencode-sw/gateway/store"
)

func TestUpstreamAuthInjectionReplacesClientAuth(t *testing.T) {
	src := http.Header{}
	src.Set("Authorization", "Bearer gateway-token")
	src.Set("X-Api-Key", "gateway-x-api-key")
	src.Set("Api-Key", "gateway-api-key")
	src.Set("Content-Type", "application/json")

	dst := http.Header{}
	copyForwardHeaders(dst, src)
	injectUpstreamAuth(dst, "upstream-key")

	if got, want := dst.Get("Authorization"), "Bearer upstream-key"; got != want {
		t.Fatalf("Authorization = %q, want %q", got, want)
	}
	if got, want := dst.Get("X-Api-Key"), "upstream-key"; got != want {
		t.Fatalf("X-Api-Key = %q, want %q", got, want)
	}
	if got := dst.Get("Api-Key"); got != "" {
		t.Fatalf("Api-Key should not be forwarded, got %q", got)
	}
	if got, want := dst.Get("Content-Type"), "application/json"; got != want {
		t.Fatalf("Content-Type = %q, want %q", got, want)
	}
}

func TestShouldMarkUpstreamFailure(t *testing.T) {
	tests := []struct {
		status int
		want   bool
	}{
		{status: http.StatusOK, want: false},
		{status: http.StatusBadRequest, want: false},
		{status: http.StatusPaymentRequired, want: true},
		{status: http.StatusUnauthorized, want: true},
		{status: http.StatusForbidden, want: true},
		{status: http.StatusTooManyRequests, want: true},
		{status: http.StatusInternalServerError, want: true},
		{status: http.StatusBadGateway, want: true},
	}

	for _, tt := range tests {
		if got := shouldMarkUpstreamFailure(tt.status); got != tt.want {
			t.Fatalf("shouldMarkUpstreamFailure(%d) = %v, want %v", tt.status, got, tt.want)
		}
	}
}

func TestProxyGroupAuthorizationRunsAfterModelRouting(t *testing.T) {
	if err := store.InitForTest("file:api_group_auth?mode=memory&cache=shared"); err != nil {
		t.Fatalf("init test db: %v", err)
	}
	gin.SetMode(gin.TestMode)

	config.RegisterModel(config.ModelRoute{
		ID:        "go-auth-test-model",
		Name:      "Go Auth Test Model",
		Upstream:  config.UpstreamGo,
		Protocol:  config.ProtocolChat,
		RealModel: "real-test-model",
		Group:     "go",
	})
	tok, err := pool.CreateToken("go-only", "go", 0, nil)
	if err != nil {
		t.Fatalf("create token: %v", err)
	}

	r := NewRouter(pool.NewPicker())
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		bytes.NewBufferString(`{"model":"go-auth-test-model","messages":[]}`))
	req.Header.Set("Authorization", "Bearer "+tok.Token)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code == http.StatusForbidden {
		t.Fatalf("group auth ran before model routing: status=%d body=%s", w.Code, w.Body.String())
	}
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d because no upstream key exists; body=%s",
			w.Code, http.StatusServiceUnavailable, w.Body.String())
	}
}

func TestCORSHeadersOnRegisteredAPIRoute(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := NewRouter(pool.NewPicker())

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("Origin", "https://example.test")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if got := w.Header().Get("Access-Control-Allow-Origin"); got != "*" {
		t.Fatalf("Access-Control-Allow-Origin = %q, want *", got)
	}
}

func TestProxyRetriesNextKeyAndLogsUpstreamError(t *testing.T) {
	if err := store.InitForTest("file:api_retry_next_key?mode=memory&cache=shared"); err != nil {
		t.Fatalf("init test db: %v", err)
	}
	gin.SetMode(gin.TestMode)

	var auths []string
	upstreamSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		auths = append(auths, auth)
		w.Header().Set("Content-Type", "application/json")
		if auth == "Bearer bad-upstream-key" {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":{"message":"bad upstream key"}}`))
			return
		}
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer upstreamSrv.Close()

	cfg := config.Get()
	oldBaseURL := cfg.GoBaseURL
	cfg.GoBaseURL = upstreamSrv.URL
	defer func() { cfg.GoBaseURL = oldBaseURL }()

	config.RegisterModel(config.ModelRoute{
		ID:        "retry-next-key-model",
		Name:      "Retry Next Key Model",
		Upstream:  config.UpstreamGo,
		Protocol:  config.ProtocolChat,
		RealModel: "real-retry-model",
		Group:     "go",
	})
	tok, err := pool.CreateToken("retry-client", "", 0, nil)
	if err != nil {
		t.Fatalf("create token: %v", err)
	}
	badKey := &store.Key{Value: "bad-upstream-key", Group: "go", Label: "bad", Enabled: true, Weight: 1}
	goodKey := &store.Key{Value: "good-upstream-key", Group: "go", Label: "good", Enabled: true, Weight: 1}
	if err := store.DB().Create(badKey).Error; err != nil {
		t.Fatalf("create bad key: %v", err)
	}
	if err := store.DB().Create(goodKey).Error; err != nil {
		t.Fatalf("create good key: %v", err)
	}

	r := NewRouter(pool.NewPicker())
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		bytes.NewBufferString(`{"model":"retry-next-key-model","messages":[]}`))
	req.Header.Set("Authorization", "Bearer "+tok.Token)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 after fallback; body=%s", w.Code, w.Body.String())
	}
	if len(auths) != 2 {
		t.Fatalf("upstream attempts = %d, want 2; auths=%#v", len(auths), auths)
	}
	if auths[0] != "Bearer bad-upstream-key" || auths[1] != "Bearer good-upstream-key" {
		t.Fatalf("unexpected key order: %#v", auths)
	}

	var logs []store.UsageLog
	if err := store.DB().Order("id asc").Find(&logs).Error; err != nil {
		t.Fatalf("load logs: %v", err)
	}
	if len(logs) != 2 {
		t.Fatalf("usage logs = %d, want 2: %#v", len(logs), logs)
	}
	if logs[0].StatusCode != http.StatusUnauthorized {
		t.Fatalf("first log status = %d, want 401", logs[0].StatusCode)
	}
	if !strings.Contains(logs[0].Error, "bad upstream key") {
		t.Fatalf("first log error missing upstream message: %q", logs[0].Error)
	}
	if logs[1].StatusCode != http.StatusOK || logs[1].Error != "" {
		t.Fatalf("second log = %#v, want successful final attempt", logs[1])
	}

	var refreshedBad store.Key
	store.DB().First(&refreshedBad, badKey.ID)
	if refreshedBad.FailCount != 1 {
		t.Fatalf("bad key fail_count = %d, want 1", refreshedBad.FailCount)
	}
}

func TestProxyLogsFinalUpstreamErrorBody(t *testing.T) {
	if err := store.InitForTest("file:api_log_final_error?mode=memory&cache=shared"); err != nil {
		t.Fatalf("init test db: %v", err)
	}
	gin.SetMode(gin.TestMode)

	upstreamSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusPaymentRequired)
		_, _ = w.Write([]byte(`{"error":{"message":"quota exceeded"}}`))
	}))
	defer upstreamSrv.Close()

	cfg := config.Get()
	oldBaseURL := cfg.GoBaseURL
	cfg.GoBaseURL = upstreamSrv.URL
	defer func() { cfg.GoBaseURL = oldBaseURL }()

	config.RegisterModel(config.ModelRoute{
		ID:        "final-error-model",
		Name:      "Final Error Model",
		Upstream:  config.UpstreamGo,
		Protocol:  config.ProtocolChat,
		RealModel: "real-final-error-model",
		Group:     "go",
	})
	tok, err := pool.CreateToken("final-error-client", "", 0, nil)
	if err != nil {
		t.Fatalf("create token: %v", err)
	}
	key := &store.Key{Value: "only-upstream-key", Group: "go", Label: "only", Enabled: true, Weight: 1}
	if err := store.DB().Create(key).Error; err != nil {
		t.Fatalf("create key: %v", err)
	}

	r := NewRouter(pool.NewPicker())
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		bytes.NewBufferString(`{"model":"final-error-model","messages":[]}`))
	req.Header.Set("Authorization", "Bearer "+tok.Token)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusPaymentRequired {
		t.Fatalf("status = %d, want 402; body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "quota exceeded") {
		t.Fatalf("upstream error body was not passed through: %s", w.Body.String())
	}

	var logRow store.UsageLog
	if err := store.DB().First(&logRow).Error; err != nil {
		t.Fatalf("load usage log: %v", err)
	}
	if logRow.StatusCode != http.StatusPaymentRequired {
		t.Fatalf("log status = %d, want 402", logRow.StatusCode)
	}
	if !strings.Contains(logRow.Error, "quota exceeded") {
		t.Fatalf("log error missing upstream body summary: %q", logRow.Error)
	}
}
