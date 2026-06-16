package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/opencode-sw/gateway/config"
	"github.com/opencode-sw/gateway/pool"
	"github.com/opencode-sw/gateway/protocol"
	"github.com/opencode-sw/gateway/store"
	"github.com/opencode-sw/gateway/upstream"
)

// proxyChat handles POST /v1/chat/completions (OpenAI Chat Completions).
func proxyChat(p *pool.Picker) gin.HandlerFunc {
	return func(c *gin.Context) {
		proxyRequest(c, p, config.ProtocolChat, "/v1/chat/completions")
	}
}

// proxyMessages handles POST /v1/messages (Anthropic Messages).
func proxyMessages(p *pool.Picker) gin.HandlerFunc {
	return func(c *gin.Context) {
		proxyRequest(c, p, config.ProtocolMessages, "/v1/messages")
	}
}

// proxyResponses handles POST /v1/responses (OpenAI Responses API).
func proxyResponses(p *pool.Picker) gin.HandlerFunc {
	return func(c *gin.Context) {
		proxyRequest(c, p, config.ProtocolResponses, "/v1/responses")
	}
}

// proxyRequest is the shared handler with full cross-protocol conversion.
//
// Flow:
//  1. Read & parse the client body to find the requested model.
//  2. Resolve the model route.
//  3. If the inbound protocol differs from the upstream protocol, convert the
//     request body through the IR intermediate representation.
//  4. Pick a KEY from the model's group.
//  5. Forward to the upstream with the KEY injected.
//  6. If cross-protocol: buffer the upstream response and convert it back.
//     If same-protocol: stream verbatim.
//  7. Bookkeeping: mark the KEY success/failure, write a usage log.
func proxyRequest(c *gin.Context, p *pool.Picker, inbound config.Protocol, upstreamPath string) {
	start := time.Now()

	body, err := io.ReadAll(c.Request.Body)
	if err != nil {
		writeOpenAIError(c, http.StatusBadRequest, "invalid_request_error", "failed to read request body")
		return
	}
	_ = c.Request.Body.Close()

	head := inspectAndMapRequestBody(c.Request.URL.Path, body)
	upstreamBody := head.Body

	route, routed := config.LookupModel(head.Model)
	if !routed {
		route = passthroughRoute(head.Model, inbound)
	}

	if tokAny, exists := c.Get("token"); exists {
		if tok, ok := tokAny.(*store.Token); ok && !pool.GroupAllowed(tok, route.Group) {
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{
				"error": gin.H{
					"type":    "permission_denied",
					"message": "this token is not allowed to use group: " + route.Group,
				},
			})
			return
		}
	}

	// Stash group for logging/debugging after the model route is known.
	c.Set("group", route.Group)

	// Cross-protocol request conversion.
	// If the inbound protocol differs from the upstream model's protocol,
	// convert the request body through the IR.
	upstreamProto := route.Protocol
	crossProtocol := routed && head.HasModel && inbound != upstreamProto

	if crossProtocol {
		converted, err := protocol.ConvertRequest(inbound, upstreamProto, upstreamBody)
		if err != nil {
			writeOpenAIError(c, http.StatusBadRequest, "conversion_error",
				fmt.Sprintf("failed to convert request from %s to %s: %v", inbound, upstreamProto, err))
			return
		}
		upstreamBody = converted
		// The converted body may have a different stream field; re-parse.
		var h struct {
			Stream bool `json:"stream"`
		}
		_ = json.Unmarshal(upstreamBody, &h)
		head.Stream = h.Stream
	}

	attempts, err := p.PickAttempts(route.Group)
	if err != nil {
		writeOpenAIError(c, http.StatusServiceUnavailable, "no_upstream_key_error",
			"no available upstream key for group "+route.Group)
		return
	}

	// Rewrite the model field in the body to the upstream's real model id.
	rewritten, ok := rewriteModel(upstreamBody, route.RealModel)
	if ok {
		upstreamBody = rewritten
	}

	// Determine the upstream endpoint path.  If the client called /v1/messages
	// but the model speaks "chat" upstream, we must hit /v1/chat/completions.
	actualUpstreamPath := upstreamPath
	if crossProtocol {
		actualUpstreamPath = upstreamPathFor(upstreamProto)
	}

	baseURL := config.BaseURLFor(route.Upstream)
	target := baseURL + actualUpstreamPath

	ctx, cancel := context.WithTimeout(c.Request.Context(), upstream.Timeout())
	defer cancel()

	var lastErrMsg string
	for i := range attempts {
		key := &attempts[i]
		p.MarkUsed(key.ID)

		req, err := http.NewRequestWithContext(ctx, c.Request.Method, target, bytes.NewReader(upstreamBody))
		if err != nil {
			markAndLog(c, p, key, route, inbound, http.StatusInternalServerError, start, head.Stream, nil, err.Error())
			writeOpenAIError(c, http.StatusInternalServerError, "internal_error", "failed to build upstream request")
			return
		}
		copyForwardHeaders(req.Header, c.Request.Header)
		setContentLength(req, len(upstreamBody))
		injectUpstreamAuth(req.Header, key.Value)

		upstreamClient := upstream.NewClientForProxy(key.ProxyURL)
		resp, err := upstreamClient.Do(req)
		if err != nil {
			p.MarkFailure(key.ID)
			lastErrMsg = "failed to reach upstream: " + err.Error()
			markAndLog(c, p, key, route, inbound, http.StatusBadGateway, start, head.Stream, nil, lastErrMsg)
			if i+1 < len(attempts) {
				continue
			}
			writeOpenAIError(c, http.StatusBadGateway, "upstream_error", lastErrMsg)
			return
		}

		if shouldRetryWithNextKey(resp.StatusCode) && i+1 < len(attempts) {
			body, readErr := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			p.MarkFailure(key.ID)
			if readErr != nil {
				lastErrMsg = "failed to read upstream error response: " + readErr.Error()
				markAndLog(c, p, key, route, inbound, http.StatusBadGateway, start, head.Stream, nil, lastErrMsg)
			} else {
				lastErrMsg = summarizeUpstreamError(resp.StatusCode, body)
				markAndLog(c, p, key, route, inbound, resp.StatusCode, start, head.Stream, nil, lastErrMsg)
			}
			continue
		}

		defer resp.Body.Close()
		if crossProtocol {
			// Buffer the upstream response and convert it back to the inbound protocol.
			proxyCrossProtocolResponse(c, resp, head.Stream, inbound, upstreamProto, p, key, route, start)
		} else {
			// Same-protocol: stream verbatim.
			proxySameProtocolResponse(c, resp, head.Stream, p, key, route, inbound, start)
		}
		return
	}

	if lastErrMsg == "" {
		lastErrMsg = "all upstream keys failed"
	}
	writeOpenAIError(c, http.StatusBadGateway, "upstream_error", lastErrMsg)
}

// proxySameProtocolResponse streams the upstream response verbatim to the client.
func proxySameProtocolResponse(c *gin.Context, resp *http.Response, stream bool,
	p *pool.Picker, key *store.Key, route config.ModelRoute, inbound config.Protocol, start time.Time) {

	if resp.StatusCode >= 400 {
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			p.MarkFailure(key.ID)
			markAndLog(c, p, key, route, inbound, http.StatusBadGateway, start, stream, nil, err.Error())
			writeOpenAIError(c, http.StatusBadGateway, "upstream_error", "failed to read upstream error response")
			return
		}
		copyResponseHeaders(c, resp)
		c.Writer.WriteHeader(resp.StatusCode)
		_, _ = c.Writer.Write(body)
		errMsg := summarizeUpstreamError(resp.StatusCode, body)
		if shouldMarkUpstreamFailure(resp.StatusCode) {
			p.MarkFailure(key.ID)
		}
		markAndLog(c, p, key, route, inbound, resp.StatusCode, start, stream, nil, errMsg)
		return
	}

	if !stream {
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			p.MarkFailure(key.ID)
			markAndLog(c, p, key, route, inbound, http.StatusBadGateway, start, false, nil, err.Error())
			writeOpenAIError(c, http.StatusBadGateway, "upstream_error", "failed to read upstream response")
			return
		}
		copyResponseHeaders(c, resp)
		c.Writer.WriteHeader(resp.StatusCode)
		_, writeErr := c.Writer.Write(body)
		usage := usageFromResponse(inbound, body)
		if resp.StatusCode < 400 && writeErr == nil {
			p.MarkSuccess(key.ID)
			markAndLog(c, p, key, route, inbound, resp.StatusCode, start, false, usage, "")
		} else if shouldMarkUpstreamFailure(resp.StatusCode) {
			p.MarkFailure(key.ID)
			markAndLog(c, p, key, route, inbound, resp.StatusCode, start, false, usage, copyErrString(writeErr))
		} else {
			markAndLog(c, p, key, route, inbound, resp.StatusCode, start, false, usage, copyErrString(writeErr))
		}
		return
	}

	copyResponseHeaders(c, resp)
	c.Writer.WriteHeader(resp.StatusCode)
	_, copyErr := io.Copy(c.Writer, resp.Body)

	if resp.StatusCode < 400 && copyErr == nil {
		p.MarkSuccess(key.ID)
		markAndLog(c, p, key, route, inbound, resp.StatusCode, start, stream, nil, "")
	} else if shouldMarkUpstreamFailure(resp.StatusCode) {
		p.MarkFailure(key.ID)
		markAndLog(c, p, key, route, inbound, resp.StatusCode, start, stream, nil, copyErrString(copyErr))
	} else {
		markAndLog(c, p, key, route, inbound, resp.StatusCode, start, stream, nil, copyErrString(copyErr))
	}
}

// proxyCrossProtocolResponse buffers the upstream response, converts it through
// the IR, and writes the converted response to the client.
func proxyCrossProtocolResponse(c *gin.Context, resp *http.Response, stream bool,
	inbound, upstreamProto config.Protocol,
	p *pool.Picker, key *store.Key, route config.ModelRoute, start time.Time) {

	if resp.StatusCode >= 400 {
		// Don't convert error responses; pass them through.
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			p.MarkFailure(key.ID)
			markAndLog(c, p, key, route, inbound, http.StatusBadGateway, start, stream, nil, err.Error())
			writeOpenAIError(c, http.StatusBadGateway, "upstream_error", "failed to read upstream error response")
			return
		}
		copyResponseHeaders(c, resp)
		c.Writer.WriteHeader(resp.StatusCode)
		_, _ = c.Writer.Write(body)
		if shouldMarkUpstreamFailure(resp.StatusCode) {
			p.MarkFailure(key.ID)
		}
		markAndLog(c, p, key, route, inbound, resp.StatusCode, start, stream, nil, summarizeUpstreamError(resp.StatusCode, body))
		return
	}

	if stream {
		// Streaming cross-protocol: buffer the full upstream SSE stream,
		// then re-emit in the target protocol format.
		c.Writer.Header().Set("Content-Type", "text/event-stream")
		c.Writer.Header().Set("Cache-Control", "no-cache")
		c.Writer.Header().Set("Connection", "keep-alive")
		c.Writer.WriteHeader(http.StatusOK)

		err := protocol.StreamConverter(c.Writer, resp.Body, upstreamProto, inbound)
		if err != nil {
			p.MarkFailure(key.ID)
			markAndLog(c, p, key, route, inbound, http.StatusBadGateway, start, true, nil, err.Error())
			return
		}
		p.MarkSuccess(key.ID)
		markAndLog(c, p, key, route, inbound, http.StatusOK, start, true, nil, "")
	} else {
		// Non-streaming: buffer, convert, write.
		upstreamBody, err := io.ReadAll(resp.Body)
		if err != nil {
			p.MarkFailure(key.ID)
			markAndLog(c, p, key, route, inbound, http.StatusBadGateway, start, false, nil, err.Error())
			writeOpenAIError(c, http.StatusBadGateway, "upstream_error", "failed to read upstream response")
			return
		}

		converted, err := protocol.ConvertResponse(upstreamProto, inbound, upstreamBody)
		if err != nil {
			p.MarkFailure(key.ID)
			markAndLog(c, p, key, route, inbound, http.StatusBadGateway, start, false, nil, err.Error())
			writeOpenAIError(c, http.StatusBadGateway, "conversion_error",
				fmt.Sprintf("failed to convert response from %s to %s: %v", upstreamProto, inbound, err))
			return
		}

		// Set appropriate content type for the target protocol.
		switch inbound {
		case config.ProtocolMessages:
			c.Writer.Header().Set("Content-Type", "application/json")
		default:
			c.Writer.Header().Set("Content-Type", "application/json")
		}
		c.Writer.WriteHeader(resp.StatusCode)
		c.Writer.Write(converted)

		p.MarkSuccess(key.ID)
		markAndLog(c, p, key, route, inbound, resp.StatusCode, start, false, usageFromResponse(inbound, converted), "")
	}
}

// upstreamPathFor returns the canonical upstream path for a protocol.
func upstreamPathFor(proto config.Protocol) string {
	switch proto {
	case config.ProtocolMessages:
		return "/v1/messages"
	case config.ProtocolResponses:
		return "/v1/responses"
	default:
		return "/v1/chat/completions"
	}
}

type requestHead struct {
	Body     []byte
	Model    string
	Stream   bool
	Parsed   bool
	HasModel bool
	Mapped   bool
}

// inspectAndMapRequestBody parses a JSON request body just enough to find
// top-level "model" and "stream". If a configured model mapping matches, it
// rewrites the JSON body and returns the mapped model. Invalid JSON or missing
// model is logged and forwarded unchanged.
func inspectAndMapRequestBody(path string, body []byte) requestHead {
	head := requestHead{Body: body}
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		log.Printf("warn: model mapping skipped for %s: request body is not valid JSON: %v", path, err)
		return head
	}
	head.Parsed = true

	if stream, ok := m["stream"].(bool); ok {
		head.Stream = stream
	}

	model, ok := m["model"].(string)
	if !ok || strings.TrimSpace(model) == "" {
		log.Printf("warn: model mapping skipped for %s: request JSON has no string model field", path)
		return head
	}
	head.Model = model
	head.HasModel = true

	mapped, ok := config.LookupModelMapping(model)
	if !ok {
		return head
	}
	m["model"] = mapped
	out, err := json.Marshal(m)
	if err != nil {
		log.Printf("warn: model mapping %q -> %q skipped for %s: remarshal failed: %v", model, mapped, path, err)
		return head
	}
	head.Body = out
	head.Model = mapped
	head.Mapped = true
	log.Printf("model mapping applied for %s: %q -> %q", path, model, mapped)
	return head
}

func passthroughRoute(model string, inbound config.Protocol) config.ModelRoute {
	id := strings.TrimSpace(model)
	if id == "" {
		id = "passthrough"
	}
	return config.ModelRoute{
		ID:        id,
		Name:      id,
		Upstream:  config.UpstreamGo,
		Protocol:  inbound,
		RealModel: model,
		Group:     "go",
	}
}

// rewriteModel returns body with the top-level "model" field replaced. It
// re-marshals compact JSON; on any failure the original body is returned with
// ok=false so the caller keeps the original (model name may be a prefix match
// upstream, but that is acceptable degradation).
func rewriteModel(body []byte, realModel string) ([]byte, bool) {
	if realModel == "" {
		return body, false
	}
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		return body, false
	}
	m["model"] = realModel
	out, err := json.Marshal(m)
	if err != nil {
		return body, false
	}
	return out, true
}

// setContentLength pins the outbound request length after any body rewrite.
func setContentLength(req *http.Request, n int) {
	req.ContentLength = int64(n)
	req.Header.Set("Content-Length", strconv.Itoa(n))
}

// copyForwardHeaders forwards client headers to the upstream request while
// stripping hop-by-hop and client auth headers.
func copyForwardHeaders(dst, src http.Header) {
	skip := map[string]bool{
		"Authorization":  true,
		"X-Api-Key":      true,
		"Api-Key":        true,
		"Content-Length": true,
		"Host":           true,
	}
	for k, vs := range src {
		if skip[http.CanonicalHeaderKey(k)] || isHopHeader(k) {
			continue
		}
		for _, v := range vs {
			dst.Add(k, v)
		}
	}
}

// injectUpstreamAuth attaches the selected upstream key to the outbound
// request using both Authorization and X-Api-Key for compatibility.
func injectUpstreamAuth(h http.Header, keyValue string) {
	keyValue = strings.TrimSpace(keyValue)
	if keyValue == "" {
		return
	}
	h.Set("Authorization", "Bearer "+keyValue)
	h.Set("X-Api-Key", keyValue)
}

// shouldMarkUpstreamFailure reports whether a response status should count as a
// key failure and trigger cooldown bookkeeping.
func shouldMarkUpstreamFailure(status int) bool {
	switch status {
	case http.StatusPaymentRequired, http.StatusUnauthorized, http.StatusForbidden, http.StatusTooManyRequests:
		return true
	default:
		return status >= 500
	}
}

// shouldRetryWithNextKey reports whether a failed upstream response should be
// retried with another available key before returning it to the client.
func shouldRetryWithNextKey(status int) bool {
	return shouldMarkUpstreamFailure(status)
}

// isHopHeader reports whether a header should be stripped on proxy hop.
func isHopHeader(k string) bool {
	switch strings.ToLower(k) {
	case "connection", "keep-alive", "proxy-authenticate", "proxy-authorization",
		"te", "trailer", "transfer-encoding", "upgrade":
		return true
	}
	return false
}

func copyResponseHeaders(c *gin.Context, resp *http.Response) {
	for k, vs := range resp.Header {
		if isHopHeader(k) {
			continue
		}
		for _, v := range vs {
			c.Writer.Header().Add(k, v)
		}
	}
}

func copyErrString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

const maxUsageErrorLen = 2048

var sensitiveErrorPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)Bearer\s+[A-Za-z0-9._~+\-/=]+`),
	regexp.MustCompile(`(?i)((?:api[_-]?key|token|secret|credential)["'\s:=]+)([^"'\s,}]+)`),
}

// summarizeUpstreamError extracts a compact, redacted error message from an
// upstream error response body for admin usage logs.
func summarizeUpstreamError(status int, body []byte) string {
	msg := extractUpstreamErrorMessage(body)
	if msg == "" {
		msg = strings.TrimSpace(strings.ToValidUTF8(string(body), "�"))
	}
	msg = strings.Join(strings.Fields(msg), " ")
	if msg == "" {
		msg = http.StatusText(status)
	}
	return trimUsageError(fmt.Sprintf("upstream returned HTTP %d: %s", status, redactUsageError(msg)))
}

func extractUpstreamErrorMessage(body []byte) string {
	var payload any
	if err := json.Unmarshal(body, &payload); err != nil {
		return ""
	}
	if msg := findErrorMessage(payload); msg != "" {
		return msg
	}
	compact, err := json.Marshal(payload)
	if err != nil {
		return ""
	}
	return string(compact)
}

func findErrorMessage(v any) string {
	switch x := v.(type) {
	case map[string]any:
		for _, key := range []string{"error", "message", "detail", "error_description", "code", "type"} {
			if val, ok := x[key]; ok {
				if msg := findErrorMessage(val); msg != "" {
					return msg
				}
			}
		}
	case []any:
		var parts []string
		for _, item := range x {
			if msg := findErrorMessage(item); msg != "" {
				parts = append(parts, msg)
			}
		}
		return strings.Join(parts, "; ")
	case string:
		return strings.TrimSpace(x)
	case float64, bool, nil:
		return fmt.Sprint(x)
	}
	return ""
}

func redactUsageError(s string) string {
	for _, pattern := range sensitiveErrorPatterns {
		s = pattern.ReplaceAllString(s, "${1}[redacted]")
	}
	return s
}

func trimUsageError(s string) string {
	if len(s) <= maxUsageErrorLen {
		return s
	}
	return s[:maxUsageErrorLen-1] + "…"
}

// markAndLog writes a usage log row. It never blocks the response path on DB errors.
func markAndLog(c *gin.Context, p *pool.Picker, key *store.Key, route config.ModelRoute,
	proto config.Protocol, status int, start time.Time, stream bool, usage *usageAccounting, errMsg string) {
	var tokenID uint
	var tokenName string
	if tokAny, exists := c.Get("token"); exists {
		if tok, ok := tokAny.(*store.Token); ok {
			tokenID = tok.ID
			tokenName = tok.Name
		}
	}
	if usage == nil {
		usage = &usageAccounting{}
	}
	totalCost := estimateUsageCost(route, usage)
	entry := store.UsageLog{
		TokenID:             tokenID,
		TokenName:           tokenName,
		KeyID:               key.ID,
		Model:               route.ID,
		Protocol:            string(proto),
		StatusCode:          status,
		DurationMs:          time.Since(start).Milliseconds(),
		Stream:              stream,
		InputTokens:         usage.InputTokens,
		OutputTokens:        usage.OutputTokens,
		CacheTokens:         usage.CacheTokens,
		CacheReadTokens:     usage.CacheReadTokens,
		CacheCreationTokens: usage.CacheCreationTokens,
		TotalTokens:         usage.TotalTokens,
		TotalCost:           totalCost,
		ActualCost:          totalCost,
		AccountCost:         totalCost,
		Error:               errMsg,
	}
	_ = store.DB().Create(&entry).Error
}

type usageAccounting struct {
	InputTokens          int
	OutputTokens         int
	CacheTokens          int
	CacheReadTokens      int
	CacheCreationTokens  int
	TotalTokens          int
	CacheIncludedInInput bool
}

func usageFromResponse(proto config.Protocol, body []byte) *usageAccounting {
	var raw map[string]any
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil
	}
	u, _ := raw["usage"].(map[string]any)
	if len(u) == 0 {
		return nil
	}
	acct := &usageAccounting{}
	switch proto {
	case config.ProtocolMessages, config.ProtocolResponses:
		acct.InputTokens = numberField(u, "input_tokens")
		acct.OutputTokens = numberField(u, "output_tokens")
	default:
		acct.InputTokens = numberField(u, "prompt_tokens")
		acct.OutputTokens = numberField(u, "completion_tokens")
	}
	acct.CacheReadTokens, acct.CacheIncludedInInput = cacheReadTokens(u)
	acct.CacheCreationTokens = cacheCreationTokens(u)
	acct.CacheTokens = firstNumberField(u, "cache_tokens", "cached_tokens", "total_cache_tokens")
	if separate := acct.CacheReadTokens + acct.CacheCreationTokens; acct.CacheTokens < separate {
		acct.CacheTokens = separate
	}
	acct.TotalTokens = numberField(u, "total_tokens")
	if acct.TotalTokens == 0 {
		acct.TotalTokens = acct.InputTokens + acct.OutputTokens
		if !acct.CacheIncludedInInput {
			acct.TotalTokens += acct.CacheTokens
		}
	}
	if acct.InputTokens == 0 && acct.OutputTokens == 0 && acct.CacheTokens == 0 && acct.TotalTokens == 0 {
		return nil
	}
	return acct
}

func cacheReadTokens(u map[string]any) (int, bool) {
	direct := firstNumberField(u,
		"cache_read_input_tokens",
		"input_cache_read_tokens",
		"cache_read_tokens",
		"prompt_cache_hit_tokens",
	)
	nested := 0
	for _, key := range []string{"prompt_tokens_details", "input_tokens_details"} {
		if details := objectField(u, key); details != nil {
			nested = maxInt(nested, numberField(details, "cached_tokens"))
		}
	}
	if nested > 0 {
		return maxInt(direct, nested), true
	}
	return direct, false
}

func cacheCreationTokens(u map[string]any) int {
	total := firstNumberField(u,
		"cache_creation_input_tokens",
		"cache_write_input_tokens",
		"input_cache_write_tokens",
		"cache_creation_tokens",
		"prompt_cache_miss_tokens",
	)
	if details := objectField(u, "cache_creation"); details != nil {
		for _, v := range details {
			total += numberValue(v)
		}
	}
	if details := objectField(u, "cache_creation_input_tokens_details"); details != nil {
		for _, v := range details {
			total += numberValue(v)
		}
	}
	return total
}

func numberField(m map[string]any, key string) int {
	return numberValue(m[key])
}

func firstNumberField(m map[string]any, keys ...string) int {
	for _, key := range keys {
		if n := numberField(m, key); n > 0 {
			return n
		}
	}
	return 0
}

func objectField(m map[string]any, key string) map[string]any {
	if v, ok := m[key].(map[string]any); ok {
		return v
	}
	return nil
}

func numberValue(v any) int {
	switch value := v.(type) {
	case float64:
		return int(value)
	case int:
		return value
	case int64:
		return int(value)
	case json.Number:
		n, _ := value.Int64()
		return int(n)
	default:
		return 0
	}
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func estimateUsageCost(route config.ModelRoute, usage *usageAccounting) float64 {
	if usage == nil || route.Pricing == nil {
		return 0
	}
	prompt := priceField(route.Pricing, "prompt")
	completion := priceField(route.Pricing, "completion")
	cacheRead := priceField(route.Pricing, "input_cache_read", "cache_read", "prompt_cache_read")
	cacheCreation := priceField(route.Pricing, "input_cache_write", "cache_write", "prompt_cache_write", "input_cache_creation")
	inputTokens := usage.InputTokens
	if usage.CacheIncludedInInput {
		inputTokens = maxInt(0, inputTokens-usage.CacheTokens)
	}
	cost := float64(inputTokens)*prompt +
		float64(usage.OutputTokens)*completion +
		float64(usage.CacheReadTokens)*cacheRead +
		float64(usage.CacheCreationTokens)*cacheCreation
	if cost <= 0 || math.IsNaN(cost) || math.IsInf(cost, 0) {
		return 0
	}
	return cost
}

func priceField(pricing map[string]string, keys ...string) float64 {
	for _, key := range keys {
		raw := strings.TrimSpace(pricing[key])
		if raw == "" {
			continue
		}
		v, err := strconv.ParseFloat(raw, 64)
		if err != nil || v < 0 {
			continue
		}
		return v
	}
	return 0
}

// writeOpenAIError emits an OpenAI-style error envelope.
func writeOpenAIError(c *gin.Context, status int, errType, message string) {
	c.JSON(status, gin.H{
		"error": gin.H{
			"type":    errType,
			"message": message,
		},
	})
}
