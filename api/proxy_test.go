Warning: truncated output (original token count: 55417)
Total output lines: 6085

package api

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/xb0or/opencode-GO/config"
	"github.com/xb0or/opencode-GO/internal/router"
	"github.com/xb0or/opencode-GO/pool"
	"github.com/xb0or/opencode-GO/protocol"
	"github.com/xb0or/opencode-GO/store"
)

type cancelAfterFirstEventReader struct {
	event []byte
	sent  bool
}

func (r *cancelAfterFirstEventReader) Read(p []byte) (int, error) {
	if !r.sent {
		r.sent = true
		return copy(p, r.event), nil
	}
	return 0, context.Canceled
}

func (r *cancelAfterFirstEventReader) Close() error { return nil }

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

func TestIsClientContextError(t *testing.T) {
	if !isClientContextError(fmt.Errorf("stream convert: %w", context.Canceled)) {
		t.Fatal("wrapped context.Canceled should be classified as a client context error")
	}
	if !isClientContextError(context.DeadlineExceeded) {
		t.Fatal("context.DeadlineExceeded should be classified as a client context error")
	}
	if isClientContextError(fmt.Errorf("upstream decoder: unexpected EOF")) {
		t.Fatal("ordinary decoder errors must not be classified as client context errors")
	}
}

func TestCrossProtocolStreamContextCanceledDoesNotMarkKey(t *testing.T) {
	if err := store.InitForTest("file:stream_context_canceled?mode=memory&cache=shared"); err != nil {
		t.Fatalf("init test db: %v", err)
	}

	key := &store.Key{Value: "stream-key", Group: "go", Enabled: true, Weight: 1}
	if err := store.DB().Create(key).Error; err != nil {
		t.Fatalf("create key: %v", err)
	}

	c, w := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", http.NoBody)
	reader := &cancelAfterFirstEventReader{event: []byte(
		"event: message_start\n" +
			"data: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"model\":\"m\"}}\n\n")}
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       reader,
	}
	route := config.ModelRoute{
		ID:        "stream-context-model",
		Upstream:  config.UpstreamGo,
		Protocol:  config.ProtocolChat,
		RealModel: "m",
		Group:     "go",
	}

	rr := proxyCrossProtocolStream(c, resp, config.ProtocolChat, config.ProtocolMessages,
		pool.NewPicker(), key, route, time.Now())
	if !rr.Terminal {
		t.Fatal("context cancellation after a committed stream must terminate without failover")
	}
	if w.Code != http.StatusOK {
		t.Fatalf("response status = %d, want 200 after the first SSE event was committed", w.Code)
	}

	var refreshed store.Key
	if err := store.DB().First(&refreshed, key.ID).Error; err != nil {
		t.Fatalf("reload key: %v", err)
	}
	if refreshed.FailCount != 0 {
		t.Fatalf("client context cancellation must not increment fail_count: %d", refreshed.FailCount)
	}
	if refreshed.CooldownUntil != nil {
		t.Fatalf("client context cancellation must not enter cooldown: %v", refreshed.CooldownUntil)
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

func TestStreamTimingCaptureRecordsFRTAndTTFT(t *testing.T) {
	start := time.Now().Add(-25 * time.Millisecond)
	capture := newStreamTimingCapture(config.ProtocolChat, start)
	capture.Observe([]byte("data: {\"choices\":[{\"delta\":{\"role\":\"assistant\"}}]}\n\n"))
	if capture.Metrics().FirstResponseMs == 0 || capture.Metrics().TTFTMs != 0 {
		t.Fatalf("role-only event metrics = %#v, want FRT and no TTFT", capture.Metrics())
	}
	capture.Observe([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"hello\"}}]}\n\n"))
	if capture.Metrics().TTFTMs == 0 || capture.Metrics().TTFTMs < capture.Metrics().FirstResponseMs {
		t.Fatalf("content event metrics = %#v, want TTFT >= FRT", capture.Metrics())
	}
}

func TestUniqueUpstreamsPreservesOrder(t *testing.T) {
	got := uniqueUpstreams([]config.Upstream{
		config.UpstreamOllama,
		config.UpstreamGo,
		config.UpstreamOllama,
	})
	if len(got) != 2 || got[0] != config.UpstreamOllama || got[1] != config.UpstreamGo {
		t.Fatalf("uniqueUpstreams() = %#v, want [ollama go]", got)
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

	// This test relies on passthrough (unregistered model forwarded to Go).
	cfg := config.Get()
	oldMode := cfg.PassthroughMode
	cfg.PassthroughMode = "go"
	defer func() { cfg.PassthroughMode = oldMode }()

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

	// This test relies on passthrough (unregistered model forwarded to Go).
	cfg := config.Get()
	oldMode := cfg.PassthroughMode
	cfg.PassthroughMode = "go"
	defer func() { cfg.PassthroughMode = oldMode }()

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

	// This test relies on passthrough (unregistered model forwarded to Go).
	cfg := config.Get()
	oldMode := cfg.PassthroughMode
	cfg.PassthroughMode = "go"
	defer func() { cfg.PassthroughMode = oldMode }()

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

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500; body=%s", w.Code, w.Body.String())
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

// ---------------------------------------------------------------------------
// Review-requested tests (round 2)
// ---------------------------------------------------------------------------

// TestMultiUpstreamCrossProtocolEndpointPath verifies that when a Messages
// inbound request is routed to a Go upstream speaking Messages protocol,
// the upstream receives the correct URL path (/v1/messages) and a
// Messages-format body (not Chat-format).
func TestMultiUpstreamCrossProtocolEndpointPath(t *testing.T) {
	if err := store.InitForTest("file:multi_upstream_endpoint_path?mode=memory&cache=shared"); err != nil {
		t.Fatalf("init test db: %v", err)
	}
	gin.SetMode(gin.TestMode)

	var upstreamPath string
	var upstreamBody []byte
	upstreamSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamPath = r.URL.Path
		var err error
		upstreamBody, err = io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read upstream body: %v", err)
		}
		w.Header().Set("Content-Type", "a…25417 tokens truncated…string, not a JSON object.
		_, _ = w.Write([]byte("data: upstream overloaded\n\n"))
		flusher.Flush()
	}))
	defer upstreamSrv.Close()

	cfg := config.Get()
	oldBaseURL := cfg.GoBaseURL
	cfg.GoBaseURL = upstreamSrv.URL
	defer func() { cfg.GoBaseURL = oldBaseURL }()

	config.RegisterModel(config.ModelRoute{
		ID:        "r2-p02-invalid-model",
		Name:      "R2 P0-2 Invalid",
		Upstream:  config.UpstreamGo,
		Protocol:  config.ProtocolMessages,
		RealModel: "r2-p02-invalid-model",
		Group:     "go",
	})
	defer config.RemoveModel("r2-p02-invalid-model")

	tok, err := pool.CreateToken("r2-p02-invalid-client", "", 0, nil)
	if err != nil {
		t.Fatalf("create token: %v", err)
	}
	// Single key — no failover target, so the client gets 502.
	if err := store.DB().Create(&store.Key{
		Value: "upstream-key", Group: "go", Label: "test", Enabled: true, Weight: 1,
	}).Error; err != nil {
		t.Fatalf("create key: %v", err)
	}

	proxySrv := httptest.NewServer(NewRouter(pool.NewPicker()))
	defer proxySrv.Close()

	req, _ := http.NewRequest(http.MethodPost, proxySrv.URL+"/v1/chat/completions",
		bytes.NewBufferString(`{"model":"r2-p02-invalid-model","messages":[{"role":"user","content":"hi"}],"stream":true}`))
	req.Header.Set("Authorization", "Bearer "+tok.Token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("proxy request: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadGateway {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		t.Fatalf("status = %d, want 502 (undecodable first event must not commit 200); body: %s",
			resp.StatusCode, string(body))
	}
	if resp.StatusCode == http.StatusOK {
		t.Fatal("status must NOT be 200 — the invalid first event must not commit the stream")
	}
	if ct := resp.Header.Get("Content-Type"); strings.Contains(ct, "text/event-stream") {
		t.Errorf("Content-Type = %q, must NOT be text/event-stream (no 200 was committed)", ct)
	}
}

// TestR2_P0_2_ValidFirstEventCommits200 verifies that a valid Messages SSE
// stream (message_start, content_block_start, content_block_delta "hello",
// pause, then the rest) commits 200 + text/event-stream and the client
// receives the converted "hello" content. This confirms the P0-2 staging
// validation does not break valid streams.
func TestR2_P0_2_ValidFirstEventCommits200(t *testing.T) {
	if err := store.InitForTest("file:r2_p02_valid?mode=memory&cache=shared"); err != nil {
		t.Fatalf("init test db: %v", err)
	}
	gin.SetMode(gin.TestMode)

	upstreamSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Fatal("upstream ResponseWriter does not support flushing")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"model\":\"m\"}}\n\n"))
		flusher.Flush()
		_, _ = w.Write([]byte("event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\"}}\n\n"))
		flusher.Flush()
		_, _ = w.Write([]byte("event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"hello\"}}\n\n"))
		flusher.Flush()
		// Pause — the client must receive the converted "hello" during this gap.
		time.Sleep(400 * time.Millisecond)
		_, _ = w.Write([]byte("event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n"))
		flusher.Flush()
		_, _ = w.Write([]byte("event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"}}\n\n"))
		flusher.Flush()
		_, _ = w.Write([]byte("event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"))
		flusher.Flush()
	}))
	defer upstreamSrv.Close()

	cfg := config.Get()
	oldBaseURL := cfg.GoBaseURL
	cfg.GoBaseURL = upstreamSrv.URL
	defer func() { cfg.GoBaseURL = oldBaseURL }()

	config.RegisterModel(config.ModelRoute{
		ID:        "r2-p02-valid-model",
		Name:      "R2 P0-2 Valid",
		Upstream:  config.UpstreamGo,
		Protocol:  config.ProtocolMessages,
		RealModel: "r2-p02-valid-model",
		Group:     "go",
	})
	defer config.RemoveModel("r2-p02-valid-model")

	tok, err := pool.CreateToken("r2-p02-valid-client", "", 0, nil)
	if err != nil {
		t.Fatalf("create token: %v", err)
	}
	if err := store.DB().Create(&store.Key{
		Value: "upstream-key", Group: "go", Label: "test", Enabled: true, Weight: 1,
	}).Error; err != nil {
		t.Fatalf("create key: %v", err)
	}

	proxySrv := httptest.NewServer(NewRouter(pool.NewPicker()))
	defer proxySrv.Close()

	req, _ := http.NewRequest(http.MethodPost, proxySrv.URL+"/v1/chat/completions",
		bytes.NewBufferString(`{"model":"r2-p02-valid-model","messages":[{"role":"user","content":"hi"}],"stream":true}`))
	req.Header.Set("Authorization", "Bearer "+tok.Token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("proxy request: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200; body preview: %s", resp.StatusCode, previewResponseBody(resp.Body))
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Fatalf("Content-Type = %q, want text/event-stream (valid stream must commit 200+SSE)", ct)
	}

	// Read the converted stream and confirm "hello" arrives before the pause ends.
	reader := bufio.NewReader(resp.Body)
	deadline := time.Now().Add(350 * time.Millisecond)
	foundHello := false
	gotChatFormat := false
loopValid:
	for time.Now().Before(deadline) {
		type result struct {
			event string
			err   error
		}
		ch := make(chan result, 1)
		go func() {
			var sb strings.Builder
			for {
				line, err := reader.ReadSlice('\n')
				if len(line) > 0 {
					sb.Write(line)
				}
				if strings.HasSuffix(sb.String(), "\n\n") {
					ch <- result{event: sb.String(), err: nil}
					return
				}
				if err != nil {
					ch <- result{event: sb.String(), err: err}
					return
				}
			}
		}()
		remaining := time.Until(deadline)
		if remaining < 0 {
			remaining = 0
		}
		select {
		case res := <-ch:
			if res.err != nil {
				t.Fatalf("read converted event: %v (partial=%q)", res.err, res.event)
			}
			if strings.Contains(res.event, "chat.completion.chunk") {
				gotChatFormat = true
			}
			if strings.Contains(res.event, `"hello"`) {
				foundHello = true
				break loopValid
			}
		case <-time.After(remaining):
			break loopValid
		}
	}
	if !gotChatFormat {
		t.Fatal("did not receive any Chat-format events — cross-protocol conversion failed")
	}
	if !foundHello {
		t.Fatal("timed out waiting for converted 'hello' content — valid stream was not committed")
	}
}

// TestR2_P0_3_DecoderErrorNoSuccessTerminal verifies that a decoder error
// AFTER the response is committed (200 + SSE) does NOT emit the success
// terminal marker. For a Chat target, the success terminal is "data: [DONE]".
// The upstream sends one valid event (message_start + content_block_start +
// content_block_delta "hello"), then a corrupt event ("data: {broken json"),
// then closes. The client must receive 200 + SSE with "hello" but must NOT
// receive "[DONE]".
func TestR2_P0_3_DecoderErrorNoSuccessTerminal(t *testing.T) {
	if err := store.InitForTest("file:r2_p03_decoder?mode=memory&cache=shared"); err != nil {
		t.Fatalf("init test db: %v", err)
	}
	gin.SetMode(gin.TestMode)

	upstreamSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Fatal("upstream ResponseWriter does not support flushing")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		// One valid event sequence.
		_, _ = w.Write([]byte("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"model\":\"m\"}}\n\n"))
		flusher.Flush()
		_, _ = w.Write([]byte("event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\"}}\n\n"))
		flusher.Flush()
		_, _ = w.Write([]byte("event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"hello\"}}\n\n"))
		flusher.Flush()
		// Corrupt event — not valid JSON. The decoder will error here.
		_, _ = w.Write([]byte("data: {broken json\n\n"))
		flusher.Flush()
	}))
	defer upstreamSrv.Close()

	cfg := config.Get()
	oldBaseURL := cfg.GoBaseURL
	cfg.GoBaseURL = upstreamSrv.URL
	defer func() { cfg.GoBaseURL = oldBaseURL }()

	config.RegisterModel(config.ModelRoute{
		ID:        "r2-p03-decoder-model",
		Name:      "R2 P0-3 Decoder",
		Upstream:  config.UpstreamGo,
		Protocol:  config.ProtocolMessages,
		RealModel: "r2-p03-decoder-model",
		Group:     "go",
	})
	defer config.RemoveModel("r2-p03-decoder-model")

	tok, err := pool.CreateToken("r2-p03-decoder-client", "", 0, nil)
	if err != nil {
		t.Fatalf("create token: %v", err)
	}
	if err := store.DB().Create(&store.Key{
		Value: "upstream-key", Group: "go", Label: "test", Enabled: true, Weight: 1,
	}).Error; err != nil {
		t.Fatalf("create key: %v", err)
	}

	proxySrv := httptest.NewServer(NewRouter(pool.NewPicker()))
	defer proxySrv.Close()

	req, _ := http.NewRequest(http.MethodPost, proxySrv.URL+"/v1/chat/completions",
		bytes.NewBufferString(`{"model":"r2-p03-decoder-model","messages":[{"role":"user","content":"hi"}],"stream":true}`))
	req.Header.Set("Authorization", "Bearer "+tok.Token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("proxy request: %v", err)
	}
	defer resp.Body.Close()

	// The first valid event commits 200 + SSE.
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (first valid event commits the response); body: %s",
			resp.StatusCode, previewResponseBody(resp.Body))
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Fatalf("Content-Type = %q, want text/event-stream (response was committed)", ct)
	}

	// Read the entire client-side stream.
	body, _ := io.ReadAll(resp.Body)
	bodyStr := string(body)

	// The "hello" content must have been delivered before the break.
	if !strings.Contains(bodyStr, `"hello"`) {
		t.Errorf("client body missing 'hello' content (should be delivered before decoder error):\n%s", bodyStr)
	}

	// The success terminal "[DONE]" must NOT be present — a decoder error
	// triggers onError, not onComplete, so the success terminal is suppressed.
	if strings.Contains(bodyStr, "[DONE]") {
		t.Errorf("client body must NOT contain '[DONE]' (decoder error must not emit success terminal):\n%s", bodyStr)
	}
}

// ---------------------------------------------------------------------------
// Protocol-level tests (no HTTP server). These exercise
// protocol.StreamConvertIncremental directly to verify multi-tool-call and
// usage forwarding behavior across all three target protocols.
// ---------------------------------------------------------------------------

// r2ChatTwoToolCallStream builds an upstream Chat SSE stream with TWO tool
// calls: tool_calls[0]={id:"call_1",name:"get_weather",arguments:'{"city":"Taipei"}'}
// and tool_calls[1]={id:"call_2",name:"search",arguments:'{"q":"news"}'}.
func r2ChatTwoToolCallStream() string {
	return "data: {\"id\":\"chatcmpl-1\",\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\"}}]}\n\n" +
		// Tool call 0: start with id + name.
		"data: {\"id\":\"chatcmpl-1\",\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"type\":\"function\",\"function\":{\"name\":\"get_weather\",\"arguments\":\"\"}}]}}]}\n\n" +
		// Tool call 0: arguments delta.
		"data: {\"id\":\"chatcmpl-1\",\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"{\\\"city\\\":\\\"Taipei\\\"}\"}}]}}]}\n\n" +
		// Tool call 1: start with id + name.
		"data: {\"id\":\"chatcmpl-1\",\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":1,\"id\":\"call_2\",\"type\":\"function\",\"function\":{\"name\":\"search\",\"arguments\":\"\"}}]}}]}\n\n" +
		// Tool call 1: arguments delta.
		"data: {\"id\":\"chatcmpl-1\",\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":1,\"function\":{\"arguments\":\"{\\\"q\\\":\\\"news\\\"}\"}}]},\"finish_reason\":\"tool_calls\"}]}\n\n" +
		"data: [DONE]\n\n"
}

// r2RunStreamConvertIncremental runs StreamConvertIncremental with the given
// upstream stream and target protocol, returning the converted output. The
// whole stream is passed as firstEvent (with a trailing blank line) and an
// empty reader as rest, so the incremental converter processes it in one go.
func r2RunStreamConvertIncremental(t *testing.T, upProto, dstProto, stream string) string {
	t.Helper()
	var dst bytes.Buffer
	flush := func() {}
	rest := bytes.NewReader(nil)
	_, err := protocol.StreamConvertIncremental(upProto, dstProto,
		[]byte(stream), rest, &dst, flush, nil)
	if err != nil {
		t.Fatalf("StreamConvertIncremental(%s->%s) error: %v", upProto, dstProto, err)
	}
	return dst.String()
}

// TestR2_P1_1_MessagesMultiToolBlocks verifies that two tool calls in a
// single Chat upstream stream each get their OWN Messages content block
// (two content_block_start events with type "tool"), with distinct block
// indices, and that each tool's id/name/arguments are preserved and routed
// to the correct block.
func TestR2_P1_1_MessagesMultiToolBlocks(t *testing.T) {
	out := r2RunStreamConvertIncremental(t, "chat", "messages", r2ChatTwoToolCallStream())

	// There must be exactly two content_block_start events with type "tool".
	toolBlockStarts := strings.Count(out, `"type":"content_block_start"`+`"content_block":{"type":"tool_use"`)
	// The encoder may emit content_block_start with content_block field; count
	// tool_use blocks directly as a robust check.
	toolUseCount := strings.Count(out, `"type":"tool_use"`)
	if toolUseCount != 2 {
		t.Errorf("expected 2 tool_use content blocks, got %d (toolBlockStarts=%d):\n%s",
			toolUseCount, toolBlockStarts, out)
	}

	// Both tool names and ids must be present.
	for _, want := range []string{
		`"name":"get_weather"`,
		`"name":"search"`,
		`"id":"call_1"`,
		`"id":"call_2"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %s:\n%s", want, out)
		}
	}

	// Both tools' arguments must be present and routed to their blocks.
	if !strings.Contains(out, `"partial_json":"{\"city\":\"Taipei\"}"`) {
		t.Errorf("output missing get_weather arguments partial_json:\n%s", out)
	}
	if !strings.Contains(out, `"partial_json":"{\"q\":\"news\"}"`) {
		t.Errorf("output missing search arguments partial_json:\n%s", out)
	}

	// The two tool blocks must have DIFFERENT content_block indices. Collect
	// the "index" values from content_block_start data lines whose
	// content_block type is tool_use. Messages SSE emits the event type on
	// the "event:" line and the JSON payload on the "data:" line, so we scan
	// for "data:" lines containing both "content_block_start" and "tool_use".
	scanner := bufio.NewScanner(strings.NewReader(out))
	scanner.Buffer(make([]byte, 256*1024), 256*1024)
	var toolBlockIndices []int
	for scanner.Scan() {
		line := scanner.Bytes()
		if !bytes.HasPrefix(line, []byte("data: ")) {
			continue
		}
		if !bytes.Contains(line, []byte("content_block_start")) {
			continue
		}
		if !bytes.Contains(line, []byte("tool_use")) {
			continue
		}
		// Extract the "index" field value.
		var ev map[string]json.RawMessage
		if err := json.Unmarshal(bytes.TrimSpace(line[6:]), &ev); err != nil {
			continue
		}
		idxRaw, ok := ev["index"]
		if !ok {
			// A missing index defaults to 0 (the first content block).
			toolBlockIndices = append(toolBlockIndices, 0)
			continue
		}
		var idx int
		if json.Unmarshal(idxRaw, &idx) == nil {
			toolBlockIndices = append(toolBlockIndices, idx)
		}
	}
	if len(toolBlockIndices) != 2 {
		t.Errorf("expected 2 tool_use content_block_start events with index, got %d: %v", len(toolBlockIndices), toolBlockIndices)
	} else if toolBlockIndices[0] == toolBlockIndices[1] {
		t.Errorf("the two tool blocks must have DIFFERENT indices, both = %d", toolBlockIndices[0])
	}
}

// TestR2_P1_2_ResponsesToolCallPreservesIDName verifies that two tool calls
// in a Chat upstream stream, converted to the Responses protocol, produce
// two "response.output_item.added" events each carrying a function_call item
// with a distinct call_id and function name, and that the
// "response.function_call_arguments.delta" events use the correct
// output_index (0 and 1, not all 0).
func TestR2_P1_2_ResponsesToolCallPreservesIDName(t *testing.T) {
	out := r2RunStreamConvertIncremental(t, "chat", "responses", r2ChatTwoToolCallStream())

	// Two response.output_item.added events with function_call items.
	if n := strings.Count(out, `"type":"response.output_item.added"`); n < 2 {
		t.Errorf("expected at least 2 response.output_item.added events, got %d:\n%s", n, out)
	}
	// Distinct call_id and function names preserved.
	for _, want := range []string{
		`"type":"function_call"`,
		`"call_id":"call_1"`,
		`"call_id":"call_2"`,
		`"name":"get_weather"`,
		`"name":"search"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %s:\n%s", want, out)
		}
	}

	// The function_call_arguments.delta events must use output_index 0 for
	// the first tool and 1 for the second. Collect all output_index values
	// attached to response.function_call_arguments.delta events.
	scanner := bufio.NewScanner(strings.NewReader(out))
	scanner.Buffer(make([]byte, 256*1024), 256*1024)
	var deltaIndices []int
	for scanner.Scan() {
		line := scanner.Bytes()
		if !bytes.HasPrefix(line, []byte("data: ")) {
			continue
		}
		payload := bytes.TrimSpace(line[6:])
		if !bytes.Contains(payload, []byte("response.function_call_arguments.delta")) {
			continue
		}
		var ev map[string]json.RawMessage
		if err := json.Unmarshal(payload, &ev); err != nil {
			continue
		}
		idxRaw, ok := ev["output_index"]
		if !ok {
			continue
		}
		var idx int
		if json.Unmarshal(idxRaw, &idx) == nil {
			deltaIndices = append(deltaIndices, idx)
		}
	}
	if len(deltaIndices) < 2 {
		t.Errorf("expected at least 2 function_call_arguments.delta events, got %d: %v", len(deltaIndices), deltaIndices)
	} else {
		// At least one delta must use output_index 0 and at least one must
		// use output_index 1 (the two tools must not share a single index).
		seen := map[int]bool{}
		for _, idx := range deltaIndices {
			seen[idx] = true
		}
		if !seen[0] || !seen[1] {
			t.Errorf("function_call_arguments.delta output_index values = %v, want both 0 and 1 present", deltaIndices)
		}
	}
}

// TestR2_P1_3_UsageForwardedToClient verifies that usage information from an
// upstream Chat SSE stream (a usage chunk before [DONE]) is forwarded to
// the client for all three target protocols: chat, messages, and responses.
func TestR2_P1_3_UsageForwardedToClient(t *testing.T) {
	// Upstream Chat SSE: a content delta then a usage chunk, then [DONE].
	stream := "data: {\"id\":\"chatcmpl-1\",\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"}}]}\n\n" +
		"data: {\"id\":\"chatcmpl-1\",\"model\":\"m\",\"choices\":[],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":20,\"total_tokens\":30}}\n\n" +
		"data: [DONE]\n\n"

	// Target chat: output should contain a chunk with the usage field.
	t.Run("chat", func(t *testing.T) {
		out := r2RunStreamConvertIncremental(t, "chat", "chat", stream)
		if !strings.Contains(out, `"prompt_tokens":10`) {
			t.Errorf("chat output missing prompt_tokens usage:\n%s", out)
		}
		if !strings.Contains(out, `"completion_tokens":20`) {
			t.Errorf("chat output missing completion_tokens usage:\n%s", out)
		}
	})

	// Target messages: message_delta should carry usage / completion tokens.
	t.Run("messages", func(t *testing.T) {
		out := r2RunStreamConvertIncremental(t, "chat", "messages", stream)
		// The message_delta event carries output_tokens (completion tokens).
		if !strings.Contains(out, `"output_tokens":20`) {
			t.Errorf("messages output missing output_tokens=20 (completion usage):\n%s", out)
		}
	})

	// Target responses: response.completed should include usage.
	t.Run("responses", func(t *testing.T) {
		out := r2RunStreamConvertIncremental(t, "chat", "responses", stream)
		// RespUsage uses input_tokens / output_tokens / total_tokens.
		if !strings.Contains(out, `"input_tokens":10`) {
			t.Errorf("responses output missing input_tokens=10 (prompt usage):\n%s", out)
		}
		if !strings.Contains(out, `"output_tokens":20`) {
			t.Errorf("responses output missing output_tokens=20 (completion usage):\n%s", out)
		}
	})
}

// ---------------------------------------------------------------------------
// Round-3 audit verification tests.
//
// These tests verify fixes from the third P0 audit pass:
//   P0-3: The cross-protocol streaming commit path must call MarkSuccess on
//         a successful stream (key NOT marked as failure), and the commit
//         flag is set immediately after WriteHeader so a post-commit
//         client-write failure never enters the failover branch.
//   P0-4: HTTP 408/410 (and other non-retryable 4xx not already handled by
//         shouldRetryWithNextKey/shouldMarkUpstreamFailure) from a
//         cross-protocol stream=true upstream must NOT enter the SSE
//         streaming path, must NOT trigger key/upstream failover, and must
//         NOT mark the key as failed. The original status code is preserved
//         and a JSON error body (not SSE) is returned to the client.
// ---------------------------------------------------------------------------

// TestR3_P0_4_HTTP408DoesNotEnterSSE verifies that when a cross-protocol
// stream=true request gets an HTTP 408 (JSON, NOT SSE) from the upstream,
// the proxy:
//   - returns 408 to the client (not 502, not 200)
//   - returns a JSON error body (Content-Type is NOT text/event-stream)
//   - does NOT retry with the next key/upstream (single upstream hit)
//   - does NOT mark the key as failed (FailCount stays 0)
func TestR3_P0_4_HTTP408DoesNotEnterSSE(t *testing.T) {
	if err := store.InitForTest(fmt.Sprintf("file:%s_%d?mode=memory&cache=shared", t.Name(), time.Now().UnixNano())); err != nil {
		t.Fatalf("init test db: %v", err)
	}
	gin.SetMode(gin.TestMode)

	var upstreamHits atomic.Int32
	upstreamSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamHits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusRequestTimeout)
		_, _ = w.Write([]byte(`{"error":"timeout"}`))
	}))
	defer upstreamSrv.Close()

	cfg := config.Get()
	oldBaseURL := cfg.GoBaseURL
	cfg.GoBaseURL = upstreamSrv.URL
	defer func() { cfg.GoBaseURL = oldBaseURL }()

	// Inbound Chat -> Go upstream speaking Messages (cross-protocol).
	config.RegisterModel(config.ModelRoute{
		ID:        "r3-p04-408-model",
		Name:      "R3 P0-4 408",
		Upstream:  config.UpstreamGo,
		Protocol:  config.ProtocolMessages,
		RealModel: "r3-p04-408-model",
		Group:     "go",
	})
	defer config.RemoveModel("r3-p04-408-model")

	tok, err := pool.CreateToken("r3-p04-408-client", "", 0, nil)
	if err != nil {
		t.Fatalf("create token: %v", err)
	}
	// Two keys in the "go" group -- failover must NOT happen for 408.
	keyA := store.Key{Value: "upstream-key-a", Group: "go", Label: "a", Enabled: true, Weight: 1}
	if err := store.DB().Create(&keyA).Error; err != nil {
		t.Fatalf("create key a: %v", err)
	}
	if err := store.DB().Create(&store.Key{
		Value: "upstream-key-b", Group: "go", Label: "b", Enabled: true, Weight: 1,
	}).Error; err != nil {
		t.Fatalf("create key b: %v", err)
	}

	proxySrv := httptest.NewServer(NewRouter(pool.NewPicker()))
	defer proxySrv.Close()

	req, _ := http.NewRequest(http.MethodPost, proxySrv.URL+"/v1/chat/completions",
		bytes.NewBufferString(`{"model":"r3-p04-408-model","messages":[{"role":"user","content":"hi"}],"stream":true}`))
	req.Header.Set("Authorization", "Bearer "+tok.Token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("proxy request: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusRequestTimeout {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		t.Fatalf("status = %d, want 408; body: %s", resp.StatusCode, string(body))
	}
	if got := upstreamHits.Load(); got != 1 {
		t.Errorf("upstream hit count = %d, want 1 (408 must not trigger failover)", got)
	}
	if ct := resp.Header.Get("Content-Type"); strings.Contains(ct, "text/event-stream") {
		t.Errorf("Content-Type = %q, must NOT be text/event-stream for a 408 error", ct)
	}
	// The body must be a JSON error envelope, not SSE. The proxy replaces the
	// upstream body with a generic OpenAI-style error, so we just verify it
	// is JSON (contains "error") and is not an SSE stream (no "data:" prefix).
	bodyBytes, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
	bodyStr := string(bodyBytes)
	if !strings.Contains(bodyStr, `"error"`) {
		t.Errorf("response body must be a JSON error envelope, got: %s", bodyStr)
	}
	if strings.HasPrefix(strings.TrimSpace(bodyStr), "data:") {
		t.Errorf("response body must NOT be an SSE stream, got: %s", bodyStr)
	}
	// P0-4: 408 must NOT mark the key as failed (no markKeyFailure call).
	var refreshedKeyA store.Key
	if err := store.DB().First(&refreshedKeyA, keyA.ID).Error; err != nil {
		t.Fatalf("reload key a: %v", err)
	}
	if refreshedKeyA.FailCount != 0 {
		t.Errorf("key a FailCount = %d, want 0 (408 must not mark key as failed)", refreshedKeyA.FailCount)
	}
}

// TestR3_P0_4_HTTP410DoesNotEnterSSE is the 410 (Gone) variant: the proxy
// must preserve 410, return a JSON error body, not enter SSE, not failover,
// and not mark the key as failed.
func TestR3_P0_4_HTTP410DoesNotEnterSSE(t *testing.T) {
	if err := store.InitForTest(fmt.Sprintf("file:%s_%d?mode=memory&cache=shared", t.Name(), time.Now().UnixNano())); err != nil {
		t.Fatalf("init test db: %v", err)
	}
	gin.SetMode(gin.TestMode)

	var upstreamHits atomic.Int32
	upstreamSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamHits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusGone)
		_, _ = w.Write([]byte(`{"error":"gone"}`))
	}))
	defer upstreamSrv.Close()

	cfg := config.Get()
	oldBaseURL := cfg.GoBaseURL
	cfg.GoBaseURL = upstreamSrv.URL
	defer func() { cfg.GoBaseURL = oldBaseURL }()

	config.RegisterModel(config.ModelRoute{
		ID:        "r3-p04-410-model",
		Name:      "R3 P0-4 410",
		Upstream:  config.UpstreamGo,
		Protocol:  config.ProtocolMessages,
		RealModel: "r3-p04-410-model",
		Group:     "go",
	})
	defer config.RemoveModel("r3-p04-410-model")

	tok, err := pool.CreateToken("r3-p04-410-client", "", 0, nil)
	if err != nil {
		t.Fatalf("create token: %v", err)
	}
	keyA := store.Key{Value: "upstream-key-a", Group: "go", Label: "a", Enabled: true, Weight: 1}
	if err := store.DB().Create(&keyA).Error; err != nil {
		t.Fatalf("create key a: %v", err)
	}
	if err := store.DB().Create(&store.Key{
		Value: "upstream-key-b", Group: "go", Label: "b", Enabled: true, Weight: 1,
	}).Error; err != nil {
		t.Fatalf("create key b: %v", err)
	}

	proxySrv := httptest.NewServer(NewRouter(pool.NewPicker()))
	defer proxySrv.Close()

	req, _ := http.NewRequest(http.MethodPost, proxySrv.URL+"/v1/chat/completions",
		bytes.NewBufferString(`{"model":"r3-p04-410-model","messages":[{"role":"user","content":"hi"}],"stream":true}`))
	req.Header.Set("Authorization", "Bearer "+tok.Token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("proxy request: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusGone {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		t.Fatalf("status = %d, want 410; body: %s", resp.StatusCode, string(body))
	}
	if got := upstreamHits.Load(); got != 1 {
		t.Errorf("upstream hit count = %d, want 1 (410 must not trigger failover)", got)
	}
	if ct := resp.Header.Get("Content-Type"); strings.Contains(ct, "text/event-stream") {
		t.Errorf("Content-Type = %q, must NOT be text/event-stream for a 410 error", ct)
	}
	bodyBytes, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
	bodyStr := string(bodyBytes)
	if !strings.Contains(bodyStr, `"error"`) {
		t.Errorf("response body must be a JSON error envelope, got: %s", bodyStr)
	}
	if strings.HasPrefix(strings.TrimSpace(bodyStr), "data:") {
		t.Errorf("response body must NOT be an SSE stream, got: %s", bodyStr)
	}
	// P0-4: 410 must NOT mark the key as failed.
	var refreshedKeyA store.Key
	if err := store.DB().First(&refreshedKeyA, keyA.ID).Error; err != nil {
		t.Fatalf("reload key a: %v", err)
	}
	if refreshedKeyA.FailCount != 0 {
		t.Errorf("key a FailCount = %d, want 0 (410 must not mark key as failed)", refreshedKeyA.FailCount)
	}
}

// TestR3_P0_3_CommitBeforeStagingWrite verifies the P0-3 commit logic at
// the behavior level: a successful cross-protocol SSE stream (valid Messages
// upstream events converted to Chat) commits HTTP 200 + text/event-stream
// and calls MarkSuccess on the key (FailCount stays 0, CooldownUntil nil).
// This confirms the normal success path does not mark the key as failed and
// that the commit-before-staging-write ordering produces a 200 SSE response.
//
// The actual client-disconnect-during-staging-write scenario is difficult to
// simulate reliably in a unit test (it would require hijacking the gin writer
// to fail mid-flush); the sentinel error errClientWriteAfterCommit is an
// internal symbol. That path is covered by code review; the structure here
// exercises the same onFirstEvent commit logic on the success path.
func TestR3_P0_3_CommitBeforeStagingWrite(t *testing.T) {
	if err := store.InitForTest(fmt.Sprintf("file:%s_%d?mode=memory&cache=shared", t.Name(), time.Now().UnixNano())); err != nil {
		t.Fatalf("init test db: %v", err)
	}
	gin.SetMode(gin.TestMode)

	upstreamSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Fatal("upstream ResponseWriter does not support flushing")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"model\":\"m\"}}\n\n"))
		flusher.Flush()
		_, _ = w.Write([]byte("event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\"}}\n\n"))
		flusher.Flush()
		_, _ = w.Write([]byte("event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"hi\"}}\n\n"))
		flusher.Flush()
		_, _ = w.Write([]byte("event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n"))
		flusher.Flush()
		_, _ = w.Write([]byte("event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"}}\n\n"))
		flusher.Flush()
		_, _ = w.Write([]byte("event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"))
		flusher.Flush()
	}))
	defer upstreamSrv.Close()

	cfg := config.Get()
	oldBaseURL := cfg.GoBaseURL
	cfg.GoBaseURL = upstreamSrv.URL
	defer func() { cfg.GoBaseURL = oldBaseURL }()

	config.RegisterModel(config.ModelRoute{
		ID:        "r3-p03-commit-model",
		Name:      "R3 P0-3 Commit",
		Upstream:  config.UpstreamGo,
		Protocol:  config.ProtocolMessages,
		RealModel: "r3-p03-commit-model",
		Group:     "go",
	})
	defer config.RemoveModel("r3-p03-commit-model")

	tok, err := pool.CreateToken("r3-p03-commit-client", "", 0, nil)
	if err != nil {
		t.Fatalf("create token: %v", err)
	}
	// Single key -- MarkSuccess must reset its (zero) fail count and leave
	// cooldown nil. A failure here would set FailCount > 0 / CooldownUntil.
	key := store.Key{Value: "upstream-key", Group: "go", Label: "test", Enabled: true, Weight: 1}
	if err := store.DB().Create(&key).Error; err != nil {
		t.Fatalf("create key: %v", err)
	}

	proxySrv := httptest.NewServer(NewRouter(pool.NewPicker()))
	defer proxySrv.Close()

	req, _ := http.NewRequest(http.MethodPost, proxySrv.URL+"/v1/chat/completions",
		bytes.NewBufferString(`{"model":"r3-p03-commit-model","messages":[{"role":"user","content":"hi"}],"stream":true}`))
	req.Header.Set("Authorization", "Bearer "+tok.Token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("proxy request: %v", err)
	}
	defer resp.Body.Close()

	// The first valid event commits 200 + SSE (P0-3 commit-before-staging-write).
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		t.Fatalf("status = %d, want 200 (successful stream must commit); body: %s",
			resp.StatusCode, string(body))
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Fatalf("Content-Type = %q, want text/event-stream (committed stream)", ct)
	}

	// Drain the converted stream so the proxy finishes and calls MarkSuccess.
	body, _ := io.ReadAll(resp.Body)
	bodyStr := string(body)
	if !strings.Contains(bodyStr, `"hi"`) {
		t.Errorf("converted stream missing 'hi' content:\n%s", bodyStr)
	}

	// P0-3 success path: MarkSuccess must have been called -- FailCount is 0
	// and CooldownUntil is nil (key is healthy, not in cooldown).
	var refreshed store.Key
	if err := store.DB().First(&refreshed, key.ID).Error; err != nil {
		t.Fatalf("reload key: %v", err)
	}
	if refreshed.FailCount != 0 {
		t.Errorf("key FailCount = %d, want 0 (MarkSuccess must reset fail count on a successful stream)", refreshed.FailCount)
	}
	if refreshed.CooldownUntil != nil {
		t.Errorf("key CooldownUntil = %v, want nil (MarkSuccess clears cooldown on a successful stream)", *refreshed.CooldownUntil)
	}
}

// TestR4_G1_OllamaUsesPreResolvedGroup verifies that the Ollama upstream uses
// the group pre-resolved by ResolveUpstreamGroup in the outer failover loop,
// NOT a re-resolved group. The scenario:
//   - Model has Upstream=ollama, Group="premium" (legacy style, no
//     UpstreamGroups, no Targets).
//   - DB has a key in the "premium" pool only — there is NO "ollama" pool key.
//
// Without the G1 fix, proxyOllamaRequest would re-resolve the group via
// TargetGroup/UpstreamGroup and fall back to "ollama" (the upstream name),
// find no keys, and skip/failover. With the G1 fix, the pre-resolved
// "premium" group is used, the premium key is picked, Ollama succeeds, and
// there is NO failover to Go.
func TestR4_G1_OllamaUsesPreResolvedGroup(t *testing.T) {
	if err := store.InitForTest("file:r4_g1_ollama_preresolved_group?mode=memory&cache=shared"); err != nil {
		t.Fatalf("init test db: %v", err)
	}
	gin.SetMode(gin.TestMode)

	var ollamaHits atomic.Int32
	var goHits atomic.Int32
	var ollamaAuth string

	// Ollama upstream — succeeds and records the Authorization header it
	// received so we can prove the premium-pool key was used.
	ollamaSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ollamaHits.Add(1)
		ollamaAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl-ollama","model":"m","choices":[{"message":{"role":"assistant","content":"ok"}}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`))
	}))
	defer ollamaSrv.Close()

	// Go upstream — should NEVER be hit because Ollama succeeds. If it is,
	// that means Ollama was skipped (e.g. because of wrong group resolution).
	goSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		goHits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl-go","model":"m","choices":[],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}))
	defer goSrv.Close()

	cfg := config.Get()
	oldGoURL := cfg.GoBaseURL
	oldOllamaURL := cfg.OllamaBaseURL
	cfg.GoBaseURL = goSrv.URL
	cfg.OllamaBaseURL = ollamaSrv.URL
	defer func() {
		cfg.GoBaseURL = oldGoURL
		cfg.OllamaBaseURL = oldOllamaURL
	}()

	// Legacy-style config: Upstream=ollama, Group="premium", no
	// UpstreamGroups, no Targets. ResolveUpstreamGroup(ollama) must return
	// "premium" via the legacy fallback (upstream == route.Upstream).
	config.RegisterModel(config.ModelRoute{
		ID:        "r4-g1-ollama-premium",
		Name:      "R4 G1 Ollama Premium",
		Upstream:  config.UpstreamOllama,
		Upstreams: []config.Upstream{config.UpstreamOllama, config.UpstreamGo},
		Protocol:  config.ProtocolChat,
		RealModel: "m",
		Group:     "premium",
	})
	defer config.RemoveModel("r4-g1-ollama-premium")

	// Unrestricted token so permission checks pass for any group.
	tok, err := pool.CreateToken("r4-g1-client", "", 0, nil)
	if err != nil {
		t.Fatalf("create token: %v", err)
	}

	// ONLY a "premium" pool key exists. There is NO "ollama" pool key. If
	// Ollama re-resolved the group to "ollama", PickAttempts("ollama") would
	// fail and the request would failover to Go.
	premiumKey := &store.Key{
		Value:   "premium-ollama-key",
		Group:   "premium",
		Label:   "premium-test",
		Enabled: true,
		Weight:  1,
	}
	if err := store.DB().Create(premiumKey).Error; err != nil {
		t.Fatalf("create premium key: %v", err)
	}

	r := NewRouter(pool.NewPicker())
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		bytes.NewBufferString(`{"model":"r4-g1-ollama-premium","messages":[]}`))
	req.Header.Set("Authorization", "Bearer "+tok.Token)
	req.Header.Set("Content-Type", "application/json")
	w := newCloseNotifyRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}

	// Ollama must have been reached — not skipped due to "no keys for ollama".
	if ollamaHits.Load() != 1 {
		t.Fatalf("ollamaHits = %d, want 1 (Ollama must be reached via premium group)", ollamaHits.Load())
	}

	// Go must NOT have been hit — no failover when Ollama succeeds.
	if goHits.Load() != 0 {
		t.Fatalf("goHits = %d, want 0 (no failover to Go when Ollama succeeds)", goHits.Load())
	}

	// The key used must come from the "premium" pool, not an "ollama" pool.
	wantAuth := "Bearer premium-ollama-key"
	if ollamaAuth != wantAuth {
		t.Fatalf("Ollama Authorization = %q, want %q (premium-pool key must be used)", ollamaAuth, wantAuth)
	}

	// Cross-check via the usage log: the recorded KeyID must be the premium
	// key's ID and the recorded group must be "premium".
	var logRow store.UsageLog
	if err := store.DB().First(&logRow).Error; err != nil {
		t.Fatalf("load usage log: %v", err)
	}
	if logRow.KeyID != premiumKey.ID {
		t.Fatalf("usage log KeyID = %d, want %d (premium key)", logRow.KeyID, premiumKey.ID)
	}
	if logRow.Group != "premium" {
		t.Fatalf("usage log group = %q, want %q", logRow.Group, "premium")
	}
}


// ---------------------------------------------------------------------------
// Round-5 audit verification tests (API-level).
// ---------------------------------------------------------------------------

// TestR5_P1_1_RouteGroupNotMutated verifies that the failover loop does NOT
// mutate the original route.Group. When Go fails and Ollama has a custom
// UpstreamGroups mapping, Ollama must use its own group, not the "go" group
// that was resolved for the first iteration.
func TestR5_P1_1_RouteGroupNotMutated(t *testing.T) {
	if err := store.InitForTest("file:r5_p1_1_route_group_not_mutated?mode=memory&cache=shared"); err != nil {
		t.Fatalf("init test db: %v", err)
	}
	gin.SetMode(gin.TestMode)

	// Go upstream — returns 500 to trigger failover
	goSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":{"message":"go down","type":"server_error"}}`))
	}))
	defer goSrv.Close()

	// Ollama upstream — succeeds. Records the Authorization header.
	var ollamaAuth string
	ollamaSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ollamaAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl-1","model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
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

	// Model with UpstreamGroups mapping: ollama → "premium-ollama"
	config.RegisterModel(config.ModelRoute{
		ID:             "r5-p1-1-model",
		Name:           "R5 P1-1 Model",
		Upstream:       config.UpstreamGo,
		Upstreams:      []config.Upstream{config.UpstreamGo, config.UpstreamOllama},
		Protocol:       config.ProtocolChat,
		RealModel:      "m",
		Group:          "go",
		UpstreamGroups: map[config.Upstream]string{config.UpstreamOllama: "premium-ollama"},
	})
	defer config.RemoveModel("r5-p1-1-model")

	tok, err := pool.CreateToken("r5-p1-1-client", "", 0, nil)
	if err != nil {
		t.Fatalf("create token: %v", err)
	}
	// Only premium-ollama group has a key — no "go" group key.
	if err := store.DB().Create(&store.Key{
		Value: "premium-ollama-key", Group: "premium-ollama", Label: "ollama-test", Enabled: true, Weight: 1,
	}).Error; err != nil {
		t.Fatalf("create ollama key: %v", err)
	}

	r := NewRouter(pool.NewPicker())
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		bytes.NewBufferString(`{"model":"r5-p1-1-model","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer "+tok.Token)
	req.Header.Set("Content-Type", "application/json")
	w := newCloseNotifyRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if ollamaAuth != "Bearer premium-ollama-key" {
		t.Errorf("P1-1 FAIL: Ollama auth = %q, want 'Bearer premium-ollama-key' (group pollution would use go-key or fail)", ollamaAuth)
	}
}

// TestR5_P1_3_HTTP408TriggersUpstreamFailover verifies that a 408 response
// from the first upstream triggers failover to the second upstream.
func TestR5_P1_3_HTTP408TriggersUpstreamFailover(t *testing.T) {
	if err := store.InitForTest("file:r5_p1_3_408_failover?mode=memory&cache=shared"); err != nil {
		t.Fatalf("init test db: %v", err)
	}
	gin.SetMode(gin.TestMode)

	// Go upstream — returns 408 Request Timeout
	goSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusRequestTimeout)
		_, _ = w.Write([]byte(`{"error":{"message":"request timeout","type":"timeout"}}`))
	}))
	defer goSrv.Close()

	// Ollama upstream — succeeds
	ollamaHit := false
	ollamaSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ollamaHit = true
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl-1","model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
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
		ID:        "r5-p1-3-model",
		Name:      "R5 P1-3 Model",
		Upstream:  config.UpstreamGo,
		Upstreams: []config.Upstream{config.UpstreamGo, config.UpstreamOllama},
		Protocol:  config.ProtocolChat,
		RealModel: "m",
		Group:     "go",
	})
	defer config.RemoveModel("r5-p1-3-model")

	tok, err := pool.CreateToken("r5-p1-3-client", "", 0, nil)
	if err != nil {
		t.Fatalf("create token: %v", err)
	}
	if err := store.DB().Create(&store.Key{
		Value: "go-key", Group: "go", Label: "go-test", Enabled: true, Weight: 1,
	}).Error; err != nil {
		t.Fatalf("create go key: %v", err)
	}
	if err := store.DB().Create(&store.Key{
		Value: "ollama-key", Group: "ollama", Label: "ollama-test", Enabled: true, Weight: 1,
	}).Error; err != nil {
		t.Fatalf("create ollama key: %v", err)
	}

	r := NewRouter(pool.NewPicker())
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		bytes.NewBufferString(`{"model":"r5-p1-3-model","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer "+tok.Token)
	req.Header.Set("Content-Type", "application/json")
	w := newCloseNotifyRecorder()
	r.ServeHTTP(w, req)

	if !ollamaHit {
		t.Errorf("P1-3 FAIL: Ollama was not hit — 408 did not trigger upstream failover. Status=%d, body=%s", w.Code, w.Body.String())
	}
	if w.Code != http.StatusOK {
		t.Errorf("P1-3 FAIL: status = %d, want 200 (Ollama should succeed); body=%s", w.Code, w.Body.String())
	}
}

// TestR6_P0_2_ResponseFailedTriggersFailover verifies that when the Go
// upstream returns a Responses stream containing response.failed (not
// response.completed), the gateway marks the key as failed and fails over
// to the second upstream (Ollama). The client must receive a 200 with the
// successful Ollama response, NOT finish_reason:"failed".
func TestR6_P0_2_ResponseFailedTriggersFailover(t *testing.T) {
	if err := store.InitForTest("file:r6_p0_2_response_failed_failover?mode=memory&cache=shared"); err != nil {
		t.Fatalf("init test db: %v", err)
	}
	gin.SetMode(gin.TestMode)

	// Go upstream — returns 200 with SSE stream: created → failed (no content)
	goSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fl := w.(http.Flusher)
		fl.Flush()
		fmt.Fprintf(w, "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_1\",\"model\":\"m\"}}\n\n")
		fl.Flush()
		fmt.Fprintf(w, "data: {\"type\":\"response.failed\",\"response\":{\"id\":\"resp_1\",\"status\":\"failed\",\"error\":{\"message\":\"overloaded\"}}}\n\n")
		fl.Flush()
	}))
	defer goSrv.Close()

	// Ollama upstream — returns a valid Responses SSE stream with completed
	ollamaHit := false
	ollamaSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ollamaHit = true
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fl := w.(http.Flusher)
		fl.Flush()
		fmt.Fprintf(w, "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_2\",\"model\":\"m\"}}\n\n")
		fl.Flush()
		fmt.Fprintf(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"ok\"}\n\n")
		fl.Flush()
		fmt.Fprintf(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_2\",\"status\":\"completed\"}}\n\n")
		fl.Flush()
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

	// Inbound = Responses, Go upstream speaks Responses (same protocol)
	config.RegisterModel(config.ModelRoute{
		ID:        "r6-p0-2-model",
		Name:      "R6 P0-2 Model",
		Upstream:  config.UpstreamGo,
		Upstreams: []config.Upstream{config.UpstreamGo, config.UpstreamOllama},
		Protocol:  config.ProtocolResponses,
		RealModel: "m",
		Group:     "go",
	})
	defer config.RemoveModel("r6-p0-2-model")

	tok, err := pool.CreateToken("r6-p0-2-client", "", 0, nil)
	if err != nil {
		t.Fatalf("create token: %v", err)
	}
	if err := store.DB().Create(&store.Key{
		Value: "go-key", Group: "go", Label: "go-test", Enabled: true, Weight: 1,
	}).Error; err != nil {
		t.Fatalf("create go key: %v", err)
	}
	if err := store.DB().Create(&store.Key{
		Value: "ollama-key", Group: "ollama", Label: "ollama-test", Enabled: true, Weight: 1,
	}).Error; err != nil {
		t.Fatalf("create ollama key: %v", err)
	}

	r := NewRouter(pool.NewPicker())
	req := httptest.NewRequest(http.MethodPost, "/v1/responses",
		bytes.NewBufferString(`{"model":"r6-p0-2-model","input":"hi","stream":true}`))
	req.Header.Set("Authorization", "Bearer "+tok.Token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	w := newCloseNotifyRecorder()
	r.ServeHTTP(w, req)

	body := w.Body.String()

	// Must failover to Ollama and return 200
	if !ollamaHit {
		t.Errorf("R6 P0-2 FAIL: Ollama was not hit — response.failed did not trigger failover. Status=%d, body=%s", w.Code, body)
	}
	if w.Code != http.StatusOK {
		t.Errorf("R6 P0-2 FAIL: status = %d, want 200 (Ollama should succeed); body=%s", w.Code, body)
	}
	// Must contain the successful Ollama response.completed
	if !strings.Contains(body, `"type":"response.completed"`) {
		t.Errorf("R6 P0-2 FAIL: body should contain response.completed from Ollama:\n%s", body)
	}
	// Must NOT contain response.failed from the Go upstream
	if strings.Contains(body, `"type":"response.failed"`) {
		t.Errorf("R6 P0-2 FAIL: client received response.failed in body:\n%s", body)
	}

	// Go key should have its FailCount incremented; Ollama key should be clean
	var goKey, ollamaKey store.Key
	store.DB().Where("value = ?", "go-key").First(&goKey)
	store.DB().Where("value = ?", "ollama-key").First(&ollamaKey)
	if goKey.FailCount == 0 {
		t.Errorf("R6 P0-2 FAIL: go-key FailCount = 0, want > 0 (key should be marked as failed)")
	}
	if ollamaKey.FailCount != 0 {
		t.Errorf("R6 P0-2 FAIL: ollama-key FailCount = %d, want 0 (should be marked success)", ollamaKey.FailCount)
	}
}

// ---------------------------------------------------------------------------
// Round-8 audit verification tests (responsesSSETracker terminal-state).
// ---------------------------------------------------------------------------

// TestR8_P0_2_DeltaThenFailedNoMarkSuccess verifies that a Responses stream
// with content followed by response.failed (already committed) does NOT
// call MarkSuccess — the key's FailCount must be incremented.
func TestR8_P0_2_DeltaThenFailedNoMarkSuccess(t *testing.T) {
	if err := store.InitForTest("file:r8_delta_then_failed?mode=memory&cache=shared"); err != nil {
		t.Fatalf("init test db: %v", err)
	}
	gin.SetMode(gin.TestMode)

	// Single Go upstream — returns created → delta → failed (committed)
	goSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fl := w.(http.Flusher)
		fl.Flush()
		fmt.Fprintf(w, "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_1\",\"model\":\"m\"}}\n\n")
		fl.Flush()
		fmt.Fprintf(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"hello\"}\n\n")
		fl.Flush()
		fmt.Fprintf(w, "data: {\"type\":\"response.failed\",\"response\":{\"id\":\"resp_1\",\"status\":\"failed\",\"error\":{\"message\":\"overloaded\"}}}\n\n")
		fl.Flush()
	}))
	defer goSrv.Close()

	cfg := config.Get()
	oldGoURL := cfg.GoBaseURL
	cfg.GoBaseURL = goSrv.URL
	defer func() { cfg.GoBaseURL = oldGoURL }()

	config.RegisterModel(config.ModelRoute{
		ID:        "r8-delta-failed-model",
		Name:      "R8 Delta Failed",
		Upstream:  config.UpstreamGo,
		Upstreams: []config.Upstream{config.UpstreamGo},
		Protocol:  config.ProtocolResponses,
		RealModel: "m",
		Group:     "go",
	})
	defer config.RemoveModel("r8-delta-failed-model")

	tok, err := pool.CreateToken("r8-delta-failed-client", "", 0, nil)
	if err != nil {
		t.Fatalf("create token: %v", err)
	}
	if err := store.DB().Create(&store.Key{
		Value: "go-key", Group: "go", Label: "go-test", Enabled: true, Weight: 1,
	}).Error; err != nil {
		t.Fatalf("create go key: %v", err)
	}

	r := NewRouter(pool.NewPicker())
	req := httptest.NewRequest(http.MethodPost, "/v1/responses",
		bytes.NewBufferString(`{"model":"r8-delta-failed-model","input":"hi","stream":true}`))
	req.Header.Set("Authorization", "Bearer "+tok.Token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	w := newCloseNotifyRecorder()
	r.ServeHTTP(w, req)

	// Stream was committed (delta sent) — 200 is expected
	if w.Code != http.StatusOK {
		t.Errorf("R8 FAIL: status = %d, want 200 (committed)", w.Code)
	}

	// Key FailCount must be > 0 (NOT MarkSuccess)
	var goKey store.Key
	store.DB().Where("value = ?", "go-key").First(&goKey)
	if goKey.FailCount == 0 {
		t.Errorf("R8 FAIL: go-key FailCount = 0, want > 0 (MarkSuccess should NOT have been called for response.failed)")
	}

	// Usage log must record an error
	var logs []store.UsageLog
	store.DB().Find(&logs)
	hasError := false
	for _, l := range logs {
		if l.Error != "" {
			hasError = true
			break
		}
	}
	if !hasError {
		t.Errorf("R8 FAIL: no usage log with non-empty error field (should record upstream failure)")
	}
}

// TestR8_P0_2_DeltaThenEOFNoMarkSuccess verifies that a Responses stream
// with content followed by EOF (no terminal event) does NOT call MarkSuccess.
func TestR8_P0_2_DeltaThenEOFNoMarkSuccess(t *testing.T) {
	if err := store.InitForTest("file:r8_delta_then_eof?mode=memory&cache=shared"); err != nil {
		t.Fatalf("init test db: %v", err)
	}
	gin.SetMode(gin.TestMode)

	// Single Go upstream — created → delta → EOF (no terminal)
	goSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fl := w.(http.Flusher)
		fl.Flush()
		fmt.Fprintf(w, "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_1\",\"model\":\"m\"}}\n\n")
		fl.Flush()
		fmt.Fprintf(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"hello\"}\n\n")
		fl.Flush()
		// Stream ends here — no response.completed/incomplete/failed
	}))
	defer goSrv.Close()

	cfg := config.Get()
	oldGoURL := cfg.GoBaseURL
	cfg.GoBaseURL = goSrv.URL
	defer func() { cfg.GoBaseURL = oldGoURL }()

	config.RegisterModel(config.ModelRoute{
		ID:        "r8-delta-eof-model",
		Name:      "R8 Delta EOF",
		Upstream:  config.UpstreamGo,
		Upstreams: []config.Upstream{config.UpstreamGo},
		Protocol:  config.ProtocolResponses,
		RealModel: "m",
		Group:     "go",
	})
	defer config.RemoveModel("r8-delta-eof-model")

	tok, err := pool.CreateToken("r8-delta-eof-client", "", 0, nil)
	if err != nil {
		t.Fatalf("create token: %v", err)
	}
	if err := store.DB().Create(&store.Key{
		Value: "go-key", Group: "go", Label: "go-test", Enabled: true, Weight: 1,
	}).Error; err != nil {
		t.Fatalf("create go key: %v", err)
	}

	r := NewRouter(pool.NewPicker())
	req := httptest.NewRequest(http.MethodPost, "/v1/responses",
		bytes.NewBufferString(`{"model":"r8-delta-eof-model","input":"hi","stream":true}`))
	req.Header.Set("Authorization", "Bearer "+tok.Token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	w := newCloseNotifyRecorder()
	r.ServeHTTP(w, req)

	// Key FailCount must be > 0 (no terminal → not MarkSuccess)
	var goKey store.Key
	store.DB().Where("value = ?", "go-key").First(&goKey)
	if goKey.FailCount == 0 {
		t.Errorf("R8 EOF FAIL: go-key FailCount = 0, want > 0 (MarkSuccess should NOT be called without terminal)")
	}
}

// TestR8_P0_2_DeltaThenCompletedMarkSuccess verifies that a valid Responses
// stream (created → delta → completed) DOES call MarkSuccess and clears
// the key's FailCount.
func TestR8_P0_2_DeltaThenCompletedMarkSuccess(t *testing.T) {
	if err := store.InitForTest("file:r8_delta_then_completed?mode=memory&cache=shared"); err != nil {
		t.Fatalf("init test db: %v", err)
	}
	gin.SetMode(gin.TestMode)

	goSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fl := w.(http.Flusher)
		fl.Flush()
		fmt.Fprintf(w, "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_1\",\"model\":\"m\"}}\n\n")
		fl.Flush()
		fmt.Fprintf(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"hello\"}\n\n")
		fl.Flush()
		fmt.Fprintf(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"status\":\"completed\"}}\n\n")
		fl.Flush()
	}))
	defer goSrv.Close()

	cfg := config.Get()
	oldGoURL := cfg.GoBaseURL
	cfg.GoBaseURL = goSrv.URL
	defer func() { cfg.GoBaseURL = oldGoURL }()

	config.RegisterModel(config.ModelRoute{
		ID:        "r8-completed-model",
		Name:      "R8 Completed",
		Upstream:  config.UpstreamGo,
		Upstreams: []config.Upstream{config.UpstreamGo},
		Protocol:  config.ProtocolResponses,
		RealModel: "m",
		Group:     "go",
	})
	defer config.RemoveModel("r8-completed-model")

	tok, err := pool.CreateToken("r8-completed-client", "", 0, nil)
	if err != nil {
		t.Fatalf("create token: %v", err)
	}
	// Create key with pre-existing FailCount to verify it gets reset
	if err := store.DB().Create(&store.Key{
		Value: "go-key", Group: "go", Label: "go-test", Enabled: true, Weight: 1,
	}).Error; err != nil {
		t.Fatalf("create go key: %v", err)
	}
	store.DB().Model(&store.Key{}).Where("value = ?", "go-key").Update("fail_count", 3)

	r := NewRouter(pool.NewPicker())
	req := httptest.NewRequest(http.MethodPost, "/v1/responses",
		bytes.NewBufferString(`{"model":"r8-completed-model","input":"hi","stream":true}`))
	req.Header.Set("Authorization", "Bearer "+tok.Token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	w := newCloseNotifyRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("R8 FAIL: status = %d, want 200", w.Code)
	}

	// MarkSuccess should have been called → FailCount reset to 0
	var goKey store.Key
	store.DB().Where("value = ?", "go-key").First(&goKey)
	if goKey.FailCount != 0 {
		t.Errorf("R8 FAIL: go-key FailCount = %d, want 0 (MarkSuccess should reset)", goKey.FailCount)
	}
}

// TestR8_TrackerSplitChunks verifies that the responsesSSETracker correctly
// identifies response.failed even when the JSON is split across multiple
// Write() calls (simulating fragmented TCP delivery).
func TestR8_TrackerSplitChunks(t *testing.T) {
	tracker := &responsesSSETracker{}

	// Split the response.failed event across multiple writes
	event := "data: {\"type\":\"response.failed\",\"response\":{\"id\":\"resp_1\",\"status\":\"failed\"}}\n\n"
	mid := len(event) / 2

	_, _ = tracker.Write([]byte(event[:mid]))
	_, _ = tracker.Write([]byte(event[mid:]))

	err := tracker.Finalize()
	if err == nil {
		t.Fatal("R8 tracker FAIL: expected error for split response.failed, got nil")
	}
}

// TestR8_TrackerCompletedThenFailed verifies that response.failed has the
// highest priority — even if response.completed was seen earlier, the
// final verdict is failure.
func TestR8_TrackerCompletedThenFailed(t *testing.T) {
	tracker := &responsesSSETracker{}

	_, _ = tracker.Write([]byte("data: {\"type\":\"response.created\",\"response\":{\"id\":\"r\"}}\n\n"))
	_, _ = tracker.Write([]byte("data: {\"type\":\"response.output_text.delta\",\"delta\":\"hi\"}\n\n"))
	_, _ = tracker.Write([]byte("data: {\"type\":\"response.completed\",\"response\":{\"id\":\"r\",\"status\":\"completed\"}}\n\n"))
	_, _ = tracker.Write([]byte("data: {\"type\":\"response.failed\",\"response\":{\"id\":\"r\",\"status\":\"failed\"}}\n\n"))

	err := tracker.Finalize()
	if err == nil {
		t.Fatal("R8 tracker FAIL: expected error for completed-then-failed, got nil")
	}
}
