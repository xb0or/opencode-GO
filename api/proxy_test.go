package api

import (
	"bytes"
	"encoding/json"
	"io"
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

func TestProxyAppliesModelMappingAndContentLength(t *testing.T) {
	if err := store.InitForTest("file:api_model_mapping?mode=memory&cache=shared"); err != nil {
		t.Fatalf("init test db: %v", err)
	}
	gin.SetMode(gin.TestMode)

	var upstreamBody []byte
	var upstreamContentLength int64
	var upstreamAuth, upstreamCustom string
	upstreamSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var err error
		upstreamBody, err = io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read upstream body: %v", err)
		}
		upstreamContentLength = r.ContentLength
		upstreamAuth = r.Header.Get("Authorization")
		upstreamCustom = r.Header.Get("X-Custom-Header")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer upstreamSrv.Close()

	cfg := config.Get()
	oldBaseURL := cfg.GoBaseURL
	cfg.GoBaseURL = upstreamSrv.URL
	defer func() { cfg.GoBaseURL = oldBaseURL }()
	config.RegisterModelMappings(map[string]string{"gpt-5.5": "glm-51"})
	defer config.RegisterModelMappings(map[string]string{})

	tok, err := pool.CreateToken("mapping-client", "", 0, nil)
	if err != nil {
		t.Fatalf("create token: %v", err)
	}
	if err := store.DB().Create(&store.Key{
		Value:   "upstream-key",
		Group:   "go",
		Label:   "test",
		Enabled: true,
		Weight:  1,
	}).Error; err != nil {
		t.Fatalf("create key: %v", err)
	}

	r := NewRouter(pool.NewPicker())
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		bytes.NewBufferString(`{"model":"gpt-5.5","messages":[]}`))
	req.Header.Set("Authorization", "Bearer "+tok.Token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Custom-Header", "kept")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var got map[string]any
	if err := json.Unmarshal(upstreamBody, &got); err != nil {
		t.Fatalf("upstream body is not JSON: %v body=%s", err, string(upstreamBody))
	}
	if got["model"] != "glm-51" {
		t.Fatalf("upstream model = %q, want glm-51; body=%s", got["model"], string(upstreamBody))
	}
	if upstreamContentLength != int64(len(upstreamBody)) {
		t.Fatalf("ContentLength = %d, want %d", upstreamContentLength, len(upstreamBody))
	}
	if upstreamAuth != "Bearer upstream-key" {
		t.Fatalf("Authorization = %q, want upstream key auth", upstreamAuth)
	}
	if upstreamCustom != "kept" {
		t.Fatalf("X-Custom-Header = %q, want kept", upstreamCustom)
	}
}

func TestProxyInvalidJSONIsForwardedUnchanged(t *testing.T) {
	if err := store.InitForTest("file:api_invalid_json_passthrough?mode=memory&cache=shared"); err != nil {
		t.Fatalf("init test db: %v", err)
	}
	gin.SetMode(gin.TestMode)

	var upstreamBody []byte
	upstreamSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var err error
		upstreamBody, err = io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read upstream body: %v", err)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"ok\":true}\n\n"))
	}))
	defer upstreamSrv.Close()

	cfg := config.Get()
	oldBaseURL := cfg.GoBaseURL
	cfg.GoBaseURL = upstreamSrv.URL
	defer func() { cfg.GoBaseURL = oldBaseURL }()

	tok, err := pool.CreateToken("invalid-json-client", "", 0, nil)
	if err != nil {
		t.Fatalf("create token: %v", err)
	}
	if err := store.DB().Create(&store.Key{
		Value:   "upstream-key",
		Group:   "go",
		Label:   "test",
		Enabled: true,
		Weight:  1,
	}).Error; err != nil {
		t.Fatalf("create key: %v", err)
	}

	r := NewRouter(pool.NewPicker())
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		bytes.NewBufferString(`{"model":`))
	req.Header.Set("Authorization", "Bearer "+tok.Token)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if string(upstreamBody) != `{"model":` {
		t.Fatalf("upstream body = %q, want original invalid JSON", string(upstreamBody))
	}
	if got := w.Header().Get("Content-Type"); got != "text/event-stream" {
		t.Fatalf("Content-Type = %q, want text/event-stream", got)
	}
}

func TestProxyMappedStreamResponseIsPassedThrough(t *testing.T) {
	if err := store.InitForTest("file:api_model_mapping_stream?mode=memory&cache=shared"); err != nil {
		t.Fatalf("init test db: %v", err)
	}
	gin.SetMode(gin.TestMode)

	var upstreamBody []byte
	upstreamSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var err error
		upstreamBody, err = io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read upstream body: %v", err)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"delta\":\"hi\"}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer upstreamSrv.Close()

	cfg := config.Get()
	oldBaseURL := cfg.GoBaseURL
	cfg.GoBaseURL = upstreamSrv.URL
	defer func() { cfg.GoBaseURL = oldBaseURL }()
	config.RegisterModelMappings(map[string]string{"gpt-5.5-stream": "glm-51"})
	defer config.RegisterModelMappings(map[string]string{})

	tok, err := pool.CreateToken("mapping-stream-client", "", 0, nil)
	if err != nil {
		t.Fatalf("create token: %v", err)
	}
	if err := store.DB().Create(&store.Key{
		Value:   "upstream-key",
		Group:   "go",
		Label:   "test",
		Enabled: true,
		Weight:  1,
	}).Error; err != nil {
		t.Fatalf("create key: %v", err)
	}

	r := NewRouter(pool.NewPicker())
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		bytes.NewBufferString(`{"model":"gpt-5.5-stream","messages":[],"stream":true}`))
	req.Header.Set("Authorization", "Bearer "+tok.Token)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if got := w.Header().Get("Content-Type"); got != "text/event-stream" {
		t.Fatalf("Content-Type = %q, want text/event-stream", got)
	}
	if !strings.Contains(w.Body.String(), "data: [DONE]") {
		t.Fatalf("SSE body was not passed through: %q", w.Body.String())
	}
	var got map[string]any
	if err := json.Unmarshal(upstreamBody, &got); err != nil {
		t.Fatalf("upstream body is not JSON: %v body=%s", err, string(upstreamBody))
	}
	if got["model"] != "glm-51" || got["stream"] != true {
		t.Fatalf("upstream body = %#v, want mapped stream request", got)
	}
}

func TestInspectAndMapRequestBodyKeepsUnmappedAndMissingModelBodies(t *testing.T) {
	config.RegisterModelMappings(map[string]string{"gpt-5.5": "glm-51"})
	defer config.RegisterModelMappings(map[string]string{})

	unmapped := []byte(`{"model":"unknown-model","messages":[]}`)
	head := inspectAndMapRequestBody("/v1/chat/completions", unmapped)
	if !head.Parsed || !head.HasModel || head.Mapped {
		t.Fatalf("unexpected unmapped head: %#v", head)
	}
	if string(head.Body) != string(unmapped) || head.Model != "unknown-model" {
		t.Fatalf("unmapped body/model changed: head=%#v body=%s", head, string(head.Body))
	}

	missing := []byte(`{"messages":[]}`)
	head = inspectAndMapRequestBody("/v1/chat/completions", missing)
	if !head.Parsed || head.HasModel || head.Mapped {
		t.Fatalf("unexpected missing-model head: %#v", head)
	}
	if string(head.Body) != string(missing) {
		t.Fatalf("missing-model body changed: %s", string(head.Body))
	}
}
