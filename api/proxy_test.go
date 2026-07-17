package api

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/xb0or/opencode-GO/config"
	"github.com/xb0or/opencode-GO/internal/router"
	"github.com/xb0or/opencode-GO/pool"
	"github.com/xb0or/opencode-GO/store"
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

func TestCopyForwardHeadersDropsAcceptEncoding(t *testing.T) {
	src := http.Header{}
	src.Set("Accept-Encoding", "gzip, br")
	src.Set("Content-Type", "application/json")

	dst := http.Header{}
	copyForwardHeaders(dst, src)

	if got := dst.Get("Accept-Encoding"); got != "" {
		t.Fatalf("Accept-Encoding should not be forwarded, got %q", got)
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

func TestUpstreamRequestContextSkipsTimeoutForStreams(t *testing.T) {
	cfg := config.Get()
	oldTimeout := cfg.UpstreamTimeout
	cfg.UpstreamTimeout = 1
	defer func() { cfg.UpstreamTimeout = oldTimeout }()

	streamCtx, streamCancel := upstreamRequestContext(context.Background(), true)
	defer streamCancel()
	if deadline, ok := streamCtx.Deadline(); ok {
		t.Fatalf("streaming context should not have gateway deadline, got %s", deadline)
	}

	nonStreamCtx, nonStreamCancel := upstreamRequestContext(context.Background(), false)
	defer nonStreamCancel()
	deadline, ok := nonStreamCtx.Deadline()
	if !ok {
		t.Fatal("non-streaming context should have a deadline when UPSTREAM_TIMEOUT > 0")
	}
	remaining := time.Until(deadline)
	if remaining <= 0 || remaining > 2*time.Second {
		t.Fatalf("non-streaming deadline remaining = %s, want about 1s", remaining)
	}
}

func TestUsageFromResponseIncludesCacheTokens(t *testing.T) {
	chat := usageFromResponse(config.ProtocolChat, []byte(`{
		"usage":{
			"prompt_tokens":120,
			"completion_tokens":30,
			"total_tokens":150,
			"prompt_tokens_details":{"cached_tokens":40}
		}
	}`))
	if chat == nil {
		t.Fatal("chat usage should be parsed")
	}
	if chat.InputTokens != 80 || chat.OutputTokens != 30 || chat.CacheTokens != 40 || chat.CacheReadTokens != 40 || chat.CacheCreationTokens != 0 || !chat.CacheIncludedInInput || chat.TotalTokens != 150 {
		t.Fatalf("unexpected chat usage: %#v", chat)
	}

	messages := usageFromResponse(config.ProtocolMessages, []byte(`{
		"usage":{
			"input_tokens":100,
			"output_tokens":25,
			"cache_read_input_tokens":60,
			"cache_creation_input_tokens":10
		}
	}`))
	if messages == nil {
		t.Fatal("messages usage should be parsed")
	}
	if messages.InputTokens != 110 || messages.OutputTokens != 25 || messages.CacheTokens != 60 || messages.CacheReadTokens != 60 || messages.CacheCreationTokens != 10 || messages.TotalTokens != 195 {
		t.Fatalf("unexpected messages usage: %#v", messages)
	}
}

func TestUsageFromResponseAcceptsAlternateTokenFieldNames(t *testing.T) {
	chat := usageFromResponse(config.ProtocolChat, []byte(`{
		"usage":{
			"input_tokens":7,
			"output_tokens":2,
			"input_tokens_details":{"cache_creation_tokens":4}
		}
	}`))
	if chat == nil {
		t.Fatal("chat usage should be parsed from input/output token fields")
	}
	if chat.InputTokens != 7 || chat.OutputTokens != 2 || chat.CacheCreationTokens != 4 || chat.CacheTokens != 0 || chat.TotalTokens != 9 {
		t.Fatalf("unexpected alternate chat usage: %#v", chat)
	}

	zero := usageFromResponse(config.ProtocolChat, []byte(`{
		"usage":{"prompt_tokens":0,"completion_tokens":0,"total_tokens":0}
	}`))
	if zero == nil {
		t.Fatal("explicit zero usage should be preserved instead of treated as missing")
	}
	if zero.TotalTokens != 0 {
		t.Fatalf("zero usage total = %d, want 0", zero.TotalTokens)
	}
}

func TestUsageFromResponseTreatsCacheCreationAsRegularInput(t *testing.T) {
	usage := usageFromResponse(config.ProtocolMessages, []byte(`{
		"usage":{
			"input_tokens":0,
			"output_tokens":0,
			"cache_creation_input_tokens":201312
		}
	}`))
	if usage == nil {
		t.Fatal("usage should be parsed")
	}
	if usage.InputTokens != 201312 || usage.CacheReadTokens != 0 || usage.CacheTokens != 0 || usage.CacheCreationTokens != 201312 || usage.TotalTokens != 201312 {
		t.Fatalf("cache creation must be regular input, got %#v", usage)
	}
}

func TestUsageFromResponseDeepSeekCacheHitAndMiss(t *testing.T) {
	usage := usageFromResponse(config.ProtocolChat, []byte(`{
		"usage":{
			"prompt_tokens":201312,
			"completion_tokens":88,
			"prompt_cache_hit_tokens":200000,
			"prompt_cache_miss_tokens":1312
		}
	}`))
	if usage == nil {
		t.Fatal("usage should be parsed")
	}
	if usage.InputTokens != 1312 || usage.CacheReadTokens != 200000 || usage.CacheTokens != 200000 || usage.CacheCreationTokens != 1312 || usage.TotalTokens != 201400 {
		t.Fatalf("deepseek cache hit/miss mapping incorrect: %#v", usage)
	}
}

func TestUsageFromResponseCapturesReasoningTokens(t *testing.T) {
	// OpenAI-style providers expose reasoning tokens inside completion_tokens_details.
	nested := usageFromResponse(config.ProtocolChat, []byte(`{
		"usage":{
			"prompt_tokens":120,
			"completion_tokens":80,
			"total_tokens":200,
			"completion_tokens_details":{"reasoning_tokens":50}
		}
	}`))
	if nested == nil {
		t.Fatal("usage should be parsed")
	}
	if nested.ReasoningTokens != 50 || nested.OutputTokens != 80 || nested.TotalTokens != 200 {
		t.Fatalf("nested reasoning tokens not captured: %#v", nested)
	}

	// Some providers emit reasoning tokens at the top level.
	topLevel := usageFromResponse(config.ProtocolChat, []byte(`{
		"usage":{
			"prompt_tokens":10,
			"completion_tokens":20,
			"total_tokens":30,
			"reasoning_tokens":5
		}
	}`))
	if topLevel == nil || topLevel.ReasoningTokens != 5 {
		t.Fatalf("top-level reasoning tokens not captured: %#v", topLevel)
	}

	// Reasoning tokens must not be added into totals (already included in completion).
	if topLevel.TotalTokens != 30 {
		t.Fatalf("reasoning tokens leaked into total: %#v", topLevel)
	}
}

func TestUsageFromSSELineMergesReasoningTokens(t *testing.T) {
	merged := mergeUsageAccounting(
		&usageAccounting{InputTokens: 10, OutputTokens: 20, ReasoningTokens: 5},
		&usageAccounting{OutputTokens: 20, ReasoningTokens: 8},
	)
	if merged.ReasoningTokens != 8 {
		t.Fatalf("reasoning tokens should be overwritten with the latest non-zero value, got %d", merged.ReasoningTokens)
	}
}

func TestEnableRequestStreamUsageForOpenAIProtocols(t *testing.T) {
	body := []byte(`{"model":"m","messages":[],"stream":true}`)
	out, ok := router.EnableRequestStreamUsage(body, config.ProtocolChat, true)
	if !ok {
		t.Fatal("EnableRequestStreamUsage should rewrite chat stream request")
	}
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("rewritten body is not JSON: %v", err)
	}
	opts, _ := got["stream_options"].(map[string]any)
	if opts["include_usage"] != true {
		t.Fatalf("stream_options.include_usage = %#v, want true", opts["include_usage"])
	}

	out, ok = router.EnableRequestStreamUsage(body, config.ProtocolMessages, true)
	if ok || string(out) != string(body) {
		t.Fatalf("messages stream request should not be rewritten: ok=%v body=%s", ok, string(out))
	}

	out, ok = router.EnableRequestStreamUsage([]byte(`{"model":"m","input":"hi","stream":true}`), config.ProtocolResponses, true)
	if ok {
		t.Fatalf("responses stream request should not be rewritten because Responses API usage is emitted by default: body=%s", string(out))
	}
}

func TestUsageFromSSELineParsesStreamUsage(t *testing.T) {
	chat := usageFromSSELine(config.ProtocolChat, []byte(`data: {"choices":[],"usage":{"prompt_tokens":11,"completion_tokens":7,"total_tokens":18}}`))
	if chat == nil || chat.InputTokens != 11 || chat.OutputTokens != 7 || chat.TotalTokens != 18 {
		t.Fatalf("unexpected chat stream usage: %#v", chat)
	}

	responses := usageFromSSELine(config.ProtocolResponses, []byte(`data: {"type":"response.completed","response":{"usage":{"input_tokens":13,"output_tokens":5,"total_tokens":18}}}`))
	if responses == nil || responses.InputTokens != 13 || responses.OutputTokens != 5 || responses.TotalTokens != 18 {
		t.Fatalf("unexpected responses stream usage: %#v", responses)
	}

	messages := usageFromSSELine(config.ProtocolMessages, []byte(`data: {"type":"message_stop","usage":{"input_tokens":17,"output_tokens":9}}`))
	if messages == nil || messages.InputTokens != 17 || messages.OutputTokens != 9 || messages.TotalTokens != 26 {
		t.Fatalf("unexpected messages stream usage: %#v", messages)
	}

	messagesCache := usageFromSSELine(config.ProtocolMessages, []byte(`data: {"type":"message_delta","usage":{"output_tokens":4,"cache_read_input_tokens":7,"cache_creation_input_tokens":3}}`))
	if messagesCache == nil || messagesCache.InputTokens != 3 || messagesCache.OutputTokens != 4 || messagesCache.CacheReadTokens != 7 || messagesCache.CacheCreationTokens != 3 || messagesCache.CacheTokens != 7 || messagesCache.TotalTokens != 14 {
		t.Fatalf("unexpected messages cache stream usage: %#v", messagesCache)
	}

	chatCache := usageFromSSELine(config.ProtocolChat, []byte(`data: {"choices":[],"usage":{"prompt_tokens":5,"completion_tokens":1,"prompt_tokens_details":{"cached_tokens":2}}}`))
	if chatCache == nil || chatCache.InputTokens != 3 || chatCache.OutputTokens != 1 || chatCache.CacheReadTokens != 2 || chatCache.CacheTokens != 2 || chatCache.TotalTokens != 6 {
		t.Fatalf("unexpected chat cache stream usage: %#v", chatCache)
	}
}

func TestEstimateUsageCostSeparatesCachedPromptTokens(t *testing.T) {
	route := config.ModelRoute{
		Pricing: map[string]string{
			"prompt":            "0.01",
			"completion":        "0.02",
			"input_cache_read":  "0.001",
			"input_cache_write": "0.004",
		},
	}
	usage := &usageAccounting{
		InputTokens:         80,
		OutputTokens:        30,
		CacheTokens:         40,
		CacheReadTokens:     40,
		CacheCreationTokens: 10,
	}
	got := estimateUsageCost(route, usage)
	want := float64(80)*0.01 + float64(30)*0.02 + float64(40)*0.001
	if got != want {
		t.Fatalf("cost = %v, want %v", got, want)
	}
}

func TestProxyLogsFinalCostWithGroupMultiplier(t *testing.T) {
	if err := store.InitForTest("file:api_group_multiplier_cost?mode=memory&cache=shared"); err != nil {
		t.Fatalf("init test db: %v", err)
	}
	gin.SetMode(gin.TestMode)

	upstreamSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl-1","model":"m","choices":[],"usage":{"prompt_tokens":100,"completion_tokens":50,"total_tokens":150}}`))
	}))
	defer upstreamSrv.Close()

	cfg := config.Get()
	oldBaseURL := cfg.GoBaseURL
	oldMultipliers := cfg.GroupMultipliers
	cfg.GoBaseURL = upstreamSrv.URL
	cfg.GroupMultipliers = "go=0.8,default=1"
	defer func() {
		cfg.GoBaseURL = oldBaseURL
		cfg.GroupMultipliers = oldMultipliers
	}()

	config.RegisterModel(config.ModelRoute{
		ID:        "cost-multiplier-model",
		Name:      "Cost Multiplier Model",
		Upstream:  config.UpstreamGo,
		Protocol:  config.ProtocolChat,
		RealModel: "m",
		Group:     "go",
		Pricing: map[string]string{
			"prompt":     "0.01",
			"completion": "0.02",
		},
	})
	tok, err := pool.CreateToken("cost-client", "", 0, nil)
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
		bytes.NewBufferString(`{"model":"cost-multiplier-model","messages":[]}`))
	req.Header.Set("Authorization", "Bearer "+tok.Token)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}

	var logRow store.UsageLog
	if err := store.DB().First(&logRow).Error; err != nil {
		t.Fatalf("load usage log: %v", err)
	}
	if logRow.TotalCost != 2.0 || logRow.ActualCost != 1.6 || logRow.AccountCost != 1.6 || logRow.GroupMultiplier != 0.8 {
		t.Fatalf("unexpected cost fields: %#v", logRow)
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

func TestDisabledModelHiddenAndRejected(t *testing.T) {
	if err := store.InitForTest("file:api_disabled_model?mode=memory&cache=shared"); err != nil {
		t.Fatalf("init test db: %v", err)
	}
	gin.SetMode(gin.TestMode)
	config.ReplaceModels([]config.ModelRoute{
		{
			ID:        "enabled-model",
			Name:      "Enabled Model",
			Upstream:  config.UpstreamGo,
			Protocol:  config.ProtocolChat,
			RealModel: "enabled-model",
			Group:     "go",
			Status:    config.ModelStatusPtr(config.ModelStatusEnabled),
		},
		{
			ID:        "disabled-model",
			Name:      "Disabled Model",
			Upstream:  config.UpstreamGo,
			Protocol:  config.ProtocolChat,
			RealModel: "disabled-model",
			Group:     "go",
			Status:    config.ModelStatusPtr(config.ModelStatusDisabled),
		},
	})
	defer config.ReplaceModels(nil)

	r := NewRouter(pool.NewPicker())
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("list status = %d body=%s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "disabled-model") {
		t.Fatalf("disabled model should be hidden from /v1/models: %s", w.Body.String())
	}

	tok, err := pool.CreateToken("disabled-client", "", 0, nil)
	if err != nil {
		t.Fatalf("create token: %v", err)
	}
	req = httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		bytes.NewBufferString(`{"model":"disabled-model","messages":[]}`))
	req.Header.Set("Authorization", "Bearer "+tok.Token)
	req.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("disabled model status = %d want 403 body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "model_disabled") {
		t.Fatalf("disabled error should include model_disabled: %s", w.Body.String())
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

func TestProxyStripsAcceptEncodingAndDecodesGzipUpstreamResponse(t *testing.T) {
	if err := store.InitForTest("file:api_gzip_decode?mode=memory&cache=shared"); err != nil {
		t.Fatalf("init test db: %v", err)
	}
	gin.SetMode(gin.TestMode)

	upstreamSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Accept-Encoding"); got == "" {
			t.Log("transport added Accept-Encoding automatically")
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Encoding", "gzip")
		gz := gzip.NewWriter(w)
		_, _ = gz.Write([]byte(`{"id":"chatcmpl-1","object":"chat.completion","model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
		_ = gz.Close()
	}))
	defer upstreamSrv.Close()

	cfg := config.Get()
	oldBaseURL := cfg.GoBaseURL
	cfg.GoBaseURL = upstreamSrv.URL
	defer func() { cfg.GoBaseURL = oldBaseURL }()

	config.RegisterModel(config.ModelRoute{
		ID:        "gzip-chat-model",
		Name:      "Gzip Chat Model",
		Upstream:  config.UpstreamGo,
		Protocol:  config.ProtocolChat,
		RealModel: "m",
		Group:     "go",
	})
	tok, err := pool.CreateToken("gzip-client", "", 0, nil)
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
		bytes.NewBufferString(`{"model":"gzip-chat-model","messages":[]}`))
	req.Header.Set("Authorization", "Bearer "+tok.Token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept-Encoding", "gzip")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"content":"hello"`) {
		t.Fatalf("gzip upstream response was not decoded: %s", w.Body.String())
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
	opts, _ := got["stream_options"].(map[string]any)
	if opts["include_usage"] != true {
		t.Fatalf("stream_options.include_usage = %#v, want true; body=%s", opts["include_usage"], string(upstreamBody))
	}
}

func TestProxyLogsSameProtocolStreamUsage(t *testing.T) {
	if err := store.InitForTest("file:api_stream_usage?mode=memory&cache=shared"); err != nil {
		t.Fatalf("init test db: %v", err)
	}
	gin.SetMode(gin.TestMode)

	upstreamSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"}}]}\n\n"))
		_, _ = w.Write([]byte("data: {\"choices\":[],\"usage\":{\"prompt_tokens\":12,\"completion_tokens\":8,\"total_tokens\":20}}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer upstreamSrv.Close()

	cfg := config.Get()
	oldBaseURL := cfg.GoBaseURL
	cfg.GoBaseURL = upstreamSrv.URL
	defer func() { cfg.GoBaseURL = oldBaseURL }()

	config.RegisterModel(config.ModelRoute{
		ID:        "stream-usage-model",
		Name:      "Stream Usage Model",
		Upstream:  config.UpstreamGo,
		Protocol:  config.ProtocolChat,
		RealModel: "stream-usage-model",
		Group:     "go",
	})
	tok, err := pool.CreateToken("stream-usage-client", "", 0, nil)
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
		bytes.NewBufferString(`{"model":"stream-usage-model","messages":[],"stream":true}`))
	req.Header.Set("Authorization", "Bearer "+tok.Token)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"usage"`) {
		t.Fatalf("stream usage chunk was not forwarded: %s", w.Body.String())
	}
	var logRow store.UsageLog
	if err := store.DB().First(&logRow).Error; err != nil {
		t.Fatalf("load usage log: %v", err)
	}
	if !logRow.Stream || logRow.InputTokens != 12 || logRow.OutputTokens != 8 || logRow.TotalTokens != 20 {
		t.Fatalf("unexpected stream usage log: %#v", logRow)
	}
}

func TestProxyLogsRawCrossProtocolCacheUsage(t *testing.T) {
	if err := store.InitForTest("file:api_cross_protocol_cache_usage?mode=memory&cache=shared"); err != nil {
		t.Fatalf("init test db: %v", err)
	}
	gin.SetMode(gin.TestMode)

	upstreamSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"id":"msg_1",
			"type":"message",
			"role":"assistant",
			"model":"m",
			"content":[{"type":"text","text":"hello"}],
			"stop_reason":"end_turn",
			"usage":{
				"input_tokens":100,
				"output_tokens":25,
				"cache_read_input_tokens":60,
				"cache_creation_input_tokens":10
			}
		}`))
	}))
	defer upstreamSrv.Close()

	cfg := config.Get()
	oldBaseURL := cfg.GoBaseURL
	cfg.GoBaseURL = upstreamSrv.URL
	defer func() { cfg.GoBaseURL = oldBaseURL }()

	config.RegisterModel(config.ModelRoute{
		ID:        "cross-cache-model",
		Name:      "Cross Cache Model",
		Upstream:  config.UpstreamGo,
		Protocol:  config.ProtocolMessages,
		RealModel: "m",
		Group:     "go",
	})
	tok, err := pool.CreateToken("cross-cache-client", "", 0, nil)
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
		bytes.NewBufferString(`{"model":"cross-cache-model","messages":[]}`))
	req.Header.Set("Authorization", "Bearer "+tok.Token)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var logRow store.UsageLog
	if err := store.DB().First(&logRow).Error; err != nil {
		t.Fatalf("load usage log: %v", err)
	}
	if logRow.InputTokens != 110 || logRow.OutputTokens != 25 ||
		logRow.CacheTokens != 60 || logRow.CacheReadTokens != 60 ||
		logRow.CacheCreationTokens != 10 || logRow.TotalTokens != 195 {
		t.Fatalf("unexpected cross-protocol usage log: %#v", logRow)
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
	// The raw upstream error body must NOT be exposed to the client — it may
	// contain provider/channel information. A generic error is returned instead.
	if strings.Contains(w.Body.String(), "quota exceeded") {
		t.Fatalf("upstream error body leaked to client: %s", w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "upstream request failed") {
		t.Fatalf("client did not receive a generic error message: %s", w.Body.String())
	}

	var logRow store.UsageLog
	if err := store.DB().First(&logRow).Error; err != nil {
		t.Fatalf("load usage log: %v", err)
	}
	if logRow.StatusCode != http.StatusPaymentRequired {
		t.Fatalf("log status = %d, want 402", logRow.StatusCode)
	}
	// The raw upstream error is kept in the admin usage log for debugging.
	if !strings.Contains(logRow.Error, "quota exceeded") {
		t.Fatalf("log error missing upstream body summary: %q", logRow.Error)
	}
}

func TestProxyStripsToolChoiceForReasoningModel(t *testing.T) {
	if err := store.InitForTest("file:api_strip_tool_choice?mode=memory&cache=shared"); err != nil {
		t.Fatalf("init test db: %v", err)
	}
	gin.SetMode(gin.TestMode)

	var sentBody []byte
	upstreamSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sentBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl-1","model":"deepseek-v4-flash","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}))
	defer upstreamSrv.Close()

	cfg := config.Get()
	oldBaseURL := cfg.GoBaseURL
	cfg.GoBaseURL = upstreamSrv.URL
	defer func() { cfg.GoBaseURL = oldBaseURL }()

	// Responses API inbound → Chat upstream for a DeepSeek reasoning model.
	// The client sends tool_choice=required; the gateway must strip it before
	// reaching the upstream to avoid the "Thinking mode does not support this
	// tool_choice" 400.
	config.RegisterModel(config.ModelRoute{
		ID:        "deepseek-v4-flash",
		Name:      "DeepSeek V4 Flash",
		Upstream:  config.UpstreamGo,
		Protocol:  config.ProtocolChat,
		RealModel: "deepseek-v4-flash",
		Group:     "go",
	})
	tok, err := pool.CreateToken("strip-tool-choice-client", "", 0, nil)
	if err != nil {
		t.Fatalf("create token: %v", err)
	}
	if err := store.DB().Create(&store.Key{Value: "upstream-key", Group: "go", Label: "only", Enabled: true, Weight: 1}).Error; err != nil {
		t.Fatalf("create key: %v", err)
	}

	r := NewRouter(pool.NewPicker())
	req := httptest.NewRequest(http.MethodPost, "/v1/responses",
		bytes.NewBufferString(`{"model":"deepseek-v4-flash","input":"hi","tools":[{"type":"function","name":"read","parameters":{}}],"tool_choice":"required"}`))
	req.Header.Set("Authorization", "Bearer "+tok.Token)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var sent map[string]any
	if err := json.Unmarshal(sentBody, &sent); err != nil {
		t.Fatalf("upstream body is not JSON: %v\n%s", err, string(sentBody))
	}
	if _, exists := sent["tool_choice"]; exists {
		t.Fatalf("tool_choice leaked to upstream for reasoning model: %s", string(sentBody))
	}
}

func TestProxyNetworkErrorHidesUpstreamURL(t *testing.T) {
	if err := store.InitForTest("file:api_network_error?mode=memory&cache=shared"); err != nil {
		t.Fatalf("init test db: %v", err)
	}
	gin.SetMode(gin.TestMode)

	// Point the gateway at a closed local port so the upstream dial fails.
	cfg := config.Get()
	oldBaseURL := cfg.GoBaseURL
	cfg.GoBaseURL = "http://127.0.0.1:1"
	defer func() { cfg.GoBaseURL = oldBaseURL }()

	config.RegisterModel(config.ModelRoute{
		ID:        "network-error-model",
		Name:      "Network Error Model",
		Upstream:  config.UpstreamGo,
		Protocol:  config.ProtocolChat,
		RealModel: "real-network-error-model",
		Group:     "go",
	})
	tok, err := pool.CreateToken("network-error-client", "", 0, nil)
	if err != nil {
		t.Fatalf("create token: %v", err)
	}
	if err := store.DB().Create(&store.Key{Value: "upstream-key", Group: "go", Label: "only", Enabled: true, Weight: 1}).Error; err != nil {
		t.Fatalf("create key: %v", err)
	}

	r := NewRouter(pool.NewPicker())
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		bytes.NewBufferString(`{"model":"network-error-model","messages":[]}`))
	req.Header.Set("Authorization", "Bearer "+tok.Token)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502; body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	// The client error must not reveal the upstream host/port or raw dial error.
	if strings.Contains(body, "127.0.0.1") || strings.Contains(strings.ToLower(body), "dial") {
		t.Fatalf("upstream host/dial error leaked to client: %s", body)
	}
	if !strings.Contains(body, "upstream") {
		t.Fatalf("client did not receive a generic error message: %s", body)
	}

	var logRow store.UsageLog
	if err := store.DB().First(&logRow).Error; err != nil {
		t.Fatalf("load usage log: %v", err)
	}
	// The raw network error is kept in the admin log for debugging.
	if !strings.Contains(logRow.Error, "failed to reach upstream") {
		t.Fatalf("log error missing network failure detail: %q", logRow.Error)
	}
}

func TestPreviewBodySanitizesControlChars(t *testing.T) {
	tests := []struct {
		name  string
		input []byte
		want  string
	}{
		{
			name:  "strips ANSI escape sequences",
			input: []byte("<html>502 Bad gateway\x1b[31m error\x1b[0m</html>"),
			want:  "<html>502 Bad gateway [31m error [0m</html>",
		},
		{
			name:  "preserves newlines and tabs as spaces (Fields normalization)",
			input: []byte("line1\nline2\ttab"),
			want:  "line1 line2 tab",
		},
		{
			name:  "replaces invalid UTF-8 with replacement character",
			input: []byte{0xff, 0xfe, 'o', 'k'},
			want:  "\ufffdok",
		},
		{
			name:  "truncates long bodies",
			input: []byte(strings.Repeat("x", 600)),
			want:  strings.Repeat("x", 512) + "…",
		},
		{
			name:  "strips ANSI escape sequences (control chars replaced)",
			input: []byte("<html>502 Bad gateway\x1berror</html>"),
			want:  "<html>502 Bad gateway error</html>",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := previewBody(tt.input)
			if got != tt.want {
				t.Errorf("previewBody() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestParseRequestBodyParsesModelAndStream(t *testing.T) {
	// With model
	withModel := []byte(`{"model":"test-model","messages":[],"stream":true}`)
	head := parseRequestBody("/v1/chat/completions", withModel)
	if !head.Parsed || !head.HasModel {
		t.Fatalf("expected parsed with model: %#v", head)
	}
	if head.Model != "test-model" {
		t.Fatalf("expected model test-model, got %q", head.Model)
	}
	if !head.Stream {
		t.Fatalf("expected stream=true")
	}
	if string(head.Body) != string(withModel) {
		t.Fatalf("body should not be modified by pure decode: %s", string(head.Body))
	}

	// Missing model
	missing := []byte(`{"messages":[]}`)
	head = parseRequestBody("/v1/chat/completions", missing)
	if !head.Parsed || head.HasModel {
		t.Fatalf("expected parsed without model: %#v", head)
	}
	if string(head.Body) != string(missing) {
		t.Fatalf("missing-model body changed: %s", string(head.Body))
	}

	// Invalid JSON
	invalid := []byte(`not json`)
	head = parseRequestBody("/v1/chat/completions", invalid)
	if head.Parsed {
		t.Fatalf("invalid JSON should not parse: %#v", head)
	}
}

// ---------------------------------------------------------------------------
// Multi-Upstream Failover Tests
// ---------------------------------------------------------------------------

// closeNotifyRecorder wraps httptest.ResponseRecorder to implement
// http.CloseNotifier, which httputil.ReverseProxy requires. Gin's
// responseWriter delegates CloseNotify to the underlying writer, so
// httptest.ResponseRecorder alone panics when ReverseProxy is used.
type closeNotifyRecorder struct {
	*httptest.ResponseRecorder
	ch chan bool
}

func (r *closeNotifyRecorder) CloseNotify() <-chan bool {
	return r.ch
}

func newCloseNotifyRecorder() *closeNotifyRecorder {
	return &closeNotifyRecorder{
		ResponseRecorder: httptest.NewRecorder(),
		ch:               make(chan bool),
	}
}

// TestMultiUpstreamSingleGo verifies that a single Go upstream works
// identically to the pre-multi-upstream behaviour.
func TestMultiUpstreamSingleGo(t *testing.T) {
	if err := store.InitForTest("file:multi_upstream_single_go?mode=memory&cache=shared"); err != nil {
		t.Fatalf("init test db: %v", err)
	}
	gin.SetMode(gin.TestMode)

	upstreamSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl-1","model":"m","choices":[],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`))
	}))
	defer upstreamSrv.Close()

	cfg := config.Get()
	oldGoURL := cfg.GoBaseURL
	cfg.GoBaseURL = upstreamSrv.URL
	defer func() { cfg.GoBaseURL = oldGoURL }()

	// Single upstream — only Upstream is set, Upstreams is empty.
	// applyLocalModelDefaults should populate Upstreams = [UpstreamGo].
	config.RegisterModel(config.ModelRoute{
		ID:        "single-go-model",
		Name:      "Single Go Model",
		Upstream:  config.UpstreamGo,
		Protocol:  config.ProtocolChat,
		RealModel: "m",
		Group:     "go",
	})
	defer config.RemoveModel("single-go-model")

	tok, err := pool.CreateToken("single-go-client", "", 0, nil)
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
		bytes.NewBufferString(`{"model":"single-go-model","messages":[]}`))
	req.Header.Set("Authorization", "Bearer "+tok.Token)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
}

// TestMultiUpstreamFirstGoSucceeds verifies that when both Go and Ollama are
// configured, the first upstream (Go) is used and the second is never contacted.
func TestMultiUpstreamFirstGoSucceeds(t *testing.T) {
	if err := store.InitForTest("file:multi_upstream_first_go?mode=memory&cache=shared"); err != nil {
		t.Fatalf("init test db: %v", err)
	}
	gin.SetMode(gin.TestMode)

	// Go upstream — will succeed
	goSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl-go","model":"m","choices":[],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`))
	}))
	defer goSrv.Close()

	// Ollama upstream — should never be contacted
	ollamaContacted := false
	ollamaSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ollamaContacted = true
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl-ollama","model":"m","choices":[]}`))
	}))
	defer ollamaSrv.Close()

	cfg := config.Get()
	oldGoURL := cfg.GoBaseURL
	oldOllamaURL := cfg.OllamaBaseURL
	cfg.GoBaseURL = goSrv.URL
	cfg.OllamaBaseURL = ollamaSrv.URL
	defer func() {
		cfg.GoBaseURL = oldGoURL
		cfg.OllamaBaseURL = oldOllamaURL
	}()

	// Model with both upstreams — Go first, Ollama second
	config.RegisterModel(config.ModelRoute{
		ID:        "dual-model",
		Name:      "Dual Model",
		Upstream:  config.UpstreamGo,
		Upstreams: []config.Upstream{config.UpstreamGo, config.UpstreamOllama},
		Protocol:  config.ProtocolChat,
		RealModel: "m",
		Group:     "go",
	})
	defer config.RemoveModel("dual-model")

	tok, err := pool.CreateToken("dual-client", "", 0, nil)
	if err != nil {
		t.Fatalf("create token: %v", err)
	}
	if err := store.DB().Create(&store.Key{
		Value:   "go-key",
		Group:   "go",
		Label:   "go-test",
		Enabled: true,
		Weight:  1,
	}).Error; err != nil {
		t.Fatalf("create go key: %v", err)
	}
	if err := store.DB().Create(&store.Key{
		Value:   "ollama-key",
		Group:   "ollama",
		Label:   "ollama-test",
		Enabled: true,
		Weight:  1,
	}).Error; err != nil {
		t.Fatalf("create ollama key: %v", err)
	}

	r := NewRouter(pool.NewPicker())
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		bytes.NewBufferString(`{"model":"dual-model","messages":[]}`))
	req.Header.Set("Authorization", "Bearer "+tok.Token)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if ollamaContacted {
		t.Fatal("Ollama upstream was contacted but Go should have succeeded first")
	}
}

// TestMultiUpstreamFirstGoFailsSecondOllamaSucceeds verifies that when the
// first upstream (Go) fails, the request is retried on the second (Ollama).
func TestMultiUpstreamFirstGoFailsSecondOllamaSucceeds(t *testing.T) {
	if err := store.InitForTest("file:multi_upstream_go_fail_ollama_ok?mode=memory&cache=shared"); err != nil {
		t.Fatalf("init test db: %v", err)
	}
	gin.SetMode(gin.TestMode)

	// Go upstream — returns 500 so failover kicks in
	goSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":{"message":"upstream failure","type":"server_error"}}`))
	}))
	defer goSrv.Close()

	// Ollama upstream — succeeds
	ollamaSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl-ollama","model":"m","choices":[],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`))
	}))
	defer ollamaSrv.Close()

	cfg := config.Get()
	oldGoURL := cfg.GoBaseURL
	oldOllamaURL := cfg.OllamaBaseURL
	cfg.GoBaseURL = goSrv.URL
	cfg.OllamaBaseURL = ollamaSrv.URL
	defer func() {
		cfg.GoBaseURL = oldGoURL
		cfg.OllamaBaseURL = oldOllamaURL
	}()

	config.RegisterModel(config.ModelRoute{
		ID:        "failover-model",
		Name:      "Failover Model",
		Upstream:  config.UpstreamGo,
		Upstreams: []config.Upstream{config.UpstreamGo, config.UpstreamOllama},
		Protocol:  config.ProtocolChat,
		RealModel: "m",
		Group:     "go",
	})
	defer config.RemoveModel("failover-model")

	tok, err := pool.CreateToken("failover-client", "", 0, nil)
	if err != nil {
		t.Fatalf("create token: %v", err)
	}
	if err := store.DB().Create(&store.Key{
		Value:   "go-key",
		Group:   "go",
		Label:   "go-test",
		Enabled: true,
		Weight:  1,
	}).Error; err != nil {
		t.Fatalf("create go key: %v", err)
	}
	if err := store.DB().Create(&store.Key{
		Value:   "ollama-key",
		Group:   "ollama",
		Label:   "ollama-test",
		Enabled: true,
		Weight:  1,
	}).Error; err != nil {
		t.Fatalf("create ollama key: %v", err)
	}

	r := NewRouter(pool.NewPicker())
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		bytes.NewBufferString(`{"model":"failover-model","messages":[]}`))
	req.Header.Set("Authorization", "Bearer "+tok.Token)
	req.Header.Set("Content-Type", "application/json")
	w := newCloseNotifyRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
}

// TestMultiUpstreamAllFail verifies that when all upstreams fail, a 502 error
// is returned to the client.
func TestMultiUpstreamAllFail(t *testing.T) {
	if err := store.InitForTest("file:multi_upstream_all_fail?mode=memory&cache=shared"); err != nil {
		t.Fatalf("init test db: %v", err)
	}
	gin.SetMode(gin.TestMode)

	// Go upstream — returns 500
	goSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":{"message":"go failure"}}`))
	}))
	defer goSrv.Close()

	// Ollama upstream — also returns 500
	ollamaSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":{"message":"ollama failure"}}`))
	}))
	defer ollamaSrv.Close()

	cfg := config.Get()
	oldGoURL := cfg.GoBaseURL
	oldOllamaURL := cfg.OllamaBaseURL
	cfg.GoBaseURL = goSrv.URL
	cfg.OllamaBaseURL = ollamaSrv.URL
	defer func() {
		cfg.GoBaseURL = oldGoURL
		cfg.OllamaBaseURL = oldOllamaURL
	}()

	config.RegisterModel(config.ModelRoute{
		ID:        "all-fail-model",
		Name:      "All Fail Model",
		Upstream:  config.UpstreamGo,
		Upstreams: []config.Upstream{config.UpstreamGo, config.UpstreamOllama},
		Protocol:  config.ProtocolChat,
		RealModel: "m",
		Group:     "go",
	})
	defer config.RemoveModel("all-fail-model")

	tok, err := pool.CreateToken("all-fail-client", "", 0, nil)
	if err != nil {
		t.Fatalf("create token: %v", err)
	}
	if err := store.DB().Create(&store.Key{
		Value:   "go-key",
		Group:   "go",
		Label:   "go-test",
		Enabled: true,
		Weight:  1,
	}).Error; err != nil {
		t.Fatalf("create go key: %v", err)
	}
	if err := store.DB().Create(&store.Key{
		Value:   "ollama-key",
		Group:   "ollama",
		Label:   "ollama-test",
		Enabled: true,
		Weight:  1,
	}).Error; err != nil {
		t.Fatalf("create ollama key: %v", err)
	}

	r := NewRouter(pool.NewPicker())
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		bytes.NewBufferString(`{"model":"all-fail-model","messages":[]}`))
	req.Header.Set("Authorization", "Bearer "+tok.Token)
	req.Header.Set("Content-Type", "application/json")
	w := newCloseNotifyRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502; body=%s", w.Code, w.Body.String())
	}
}

// TestMultiUpstreamNoKeysForAnyUpstream verifies that when no keys exist for
// any upstream, a 503 error is returned.
func TestMultiUpstreamNoKeysForAnyUpstream(t *testing.T) {
	if err := store.InitForTest("file:multi_upstream_no_keys?mode=memory&cache=shared"); err != nil {
		t.Fatalf("init test db: %v", err)
	}
	gin.SetMode(gin.TestMode)

	config.RegisterModel(config.ModelRoute{
		ID:        "no-keys-model",
		Name:      "No Keys Model",
		Upstream:  config.UpstreamGo,
		Upstreams: []config.Upstream{config.UpstreamGo, config.UpstreamOllama},
		Protocol:  config.ProtocolChat,
		RealModel: "m",
		Group:     "go",
	})
	defer config.RemoveModel("no-keys-model")

	tok, err := pool.CreateToken("no-keys-client", "", 0, nil)
	if err != nil {
		t.Fatalf("create token: %v", err)
	}
	// No keys created — both upstreams should fail with 503

	r := NewRouter(pool.NewPicker())
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		bytes.NewBufferString(`{"model":"no-keys-model","messages":[]}`))
	req.Header.Set("Authorization", "Bearer "+tok.Token)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503; body=%s", w.Code, w.Body.String())
	}
}
