package api

import (
	"bufio"
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
	if routed && !route.IsEnabled() {
		writeOpenAIError(c, http.StatusForbidden, "model_disabled",
			"model is disabled by administrator: "+route.ID)
		return
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
	if rewritten, ok := enableStreamUsage(upstreamBody, upstreamProto, head.Stream); ok {
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
	usage, firstResponseMs, copyErr := proxyStreamAndCaptureUsage(c.Writer, resp.Body, inbound, start)

	if resp.StatusCode < 400 && copyErr == nil {
		p.MarkSuccess(key.ID)
		markAndLog(c, p, key, route, inbound, resp.StatusCode, start, stream, usage, "", firstResponseMs)
	} else if shouldMarkUpstreamFailure(resp.StatusCode) {
		p.MarkFailure(key.ID)
		markAndLog(c, p, key, route, inbound, resp.StatusCode, start, stream, usage, copyErrString(copyErr), firstResponseMs)
	} else {
		markAndLog(c, p, key, route, inbound, resp.StatusCode, start, stream, usage, copyErrString(copyErr), firstResponseMs)
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
		//
		// We must buffer the body before writing the SSE headers, because if
		// the upstream returned an error page (e.g. a Cloudflare 502 HTML
		// page served with HTTP 200) we cannot decode it and must surface a
		// meaningful error instead of a confusing "invalid character" message
		// after having already committed a 200 status to the client.
		upstreamBody, err := io.ReadAll(resp.Body)
		if err != nil {
			p.MarkFailure(key.ID)
			markAndLog(c, p, key, route, inbound, http.StatusBadGateway, start, true, nil, err.Error())
			writeOpenAIError(c, http.StatusBadGateway, "upstream_error", "failed to read upstream response")
			return
		}

		// Decode the upstream stream first so we can report a clean error to
		// the client (without having already committed a 200 status) when the
		// upstream payload is not a valid SSE/JSON stream — e.g. an HTML
		// gateway error page served with HTTP 200.
		streamResp, convErr := protocol.DecodeStreamBuffer(upstreamProto, upstreamBody)
		if convErr != nil {
			p.MarkFailure(key.ID)
			errMsg := fmt.Sprintf("upstream %s stream response could not be decoded: %v; body: %s",
				upstreamProto, convErr, previewBody(upstreamBody))
			markAndLog(c, p, key, route, inbound, http.StatusBadGateway, start, true, nil, errMsg)
			writeOpenAIError(c, http.StatusBadGateway, "upstream_error",
				"upstream returned a non-streaming response that could not be decoded")
			return
		}

		// Commit the SSE headers only after a successful decode.
		c.Writer.Header().Set("Content-Type", "text/event-stream")
		c.Writer.Header().Set("Cache-Control", "no-cache")
		c.Writer.Header().Set("Connection", "keep-alive")
		c.Writer.WriteHeader(http.StatusOK)
		usage := usageFromSSEBuffer(upstreamProto, upstreamBody)
		if usage == nil {
			usage = usageFromIRUsage(streamResp)
		}
		if emitErr := protocol.EmitStreamResponse(c.Writer, inbound, streamResp); emitErr != nil {
			// Headers/body already partially written; just log.
			markAndLog(c, p, key, route, inbound, http.StatusOK, start, true, usage, "stream emit error: "+emitErr.Error())
			return
		}
		p.MarkSuccess(key.ID)
		markAndLog(c, p, key, route, inbound, http.StatusOK, start, true, usage, "")
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
			// Upstream returned a body that is not valid JSON for its protocol
			// (commonly an HTML error page from an upstream proxy/CDN). Report
			// the real cause instead of the opaque JSON parse error.
			p.MarkFailure(key.ID)
			errMsg := fmt.Sprintf("upstream %s response could not be decoded: %v; body: %s",
				upstreamProto, err, previewBody(upstreamBody))
			markAndLog(c, p, key, route, inbound, http.StatusBadGateway, start, false, nil, errMsg)
			writeOpenAIError(c, http.StatusBadGateway, "upstream_error",
				"upstream returned a non-JSON response that could not be decoded")
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
		usage := usageFromResponse(upstreamProto, upstreamBody)
		if usage == nil {
			usage = usageFromResponse(inbound, converted)
		}
		markAndLog(c, p, key, route, inbound, resp.StatusCode, start, false, usage, "")
	}
}

// previewBody returns a compact, redacted, length-limited preview of an
// upstream response body for inclusion in error logs. It collapses whitespace
// and strips control characters so HTML error pages are readable.
func previewBody(body []byte) string {
	const maxPreview = 512
	s := strings.ToValidUTF8(string(body), "�")
	s = strings.Map(func(r rune) rune {
		if r < 0x20 && r != '\n' && r != '\t' {
			return ' '
		}
		return r
	}, s)
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > maxPreview {
		s = s[:maxPreview] + "…"
	}
	return s
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

// enableStreamUsage asks upstream protocols that support it to include final
// usage accounting in SSE streams so admin usage logs can record token counts.
func enableStreamUsage(body []byte, proto config.Protocol, stream bool) ([]byte, bool) {
	if !stream {
		return body, false
	}
	if proto != config.ProtocolChat {
		return body, false
	}
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		return body, false
	}
	opts := objectField(m, "stream_options")
	if opts == nil {
		opts = map[string]any{}
	}
	opts["include_usage"] = true
	m["stream_options"] = opts
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
		"Authorization":   true,
		"X-Api-Key":       true,
		"Api-Key":         true,
		"Content-Length":  true,
		"Host":            true,
		"Accept-Encoding": true,
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
	proto config.Protocol, status int, start time.Time, stream bool, usage *usageAccounting, errMsg string, firstResponseMs ...int64) {
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
	baseCost := estimateUsageCost(route, usage)
	groupMultiplier := config.GroupMultiplier(route.Group)
	finalCost := baseCost * groupMultiplier
	if groupMultiplier <= 0 || math.IsNaN(finalCost) || math.IsInf(finalCost, 0) {
		groupMultiplier = 1
		finalCost = baseCost
	}
	frt := int64(0)
	if len(firstResponseMs) > 0 && firstResponseMs[0] > 0 {
		frt = firstResponseMs[0]
	}
	pricing := usagePricing(route)
	entry := store.UsageLog{
		RequestID:           usageRequestID(c, key, start),
		TokenID:             tokenID,
		TokenName:           tokenName,
		KeyID:               key.ID,
		Model:               route.ID,
		Group:               route.Group,
		Protocol:            string(proto),
		IPAddress:           c.ClientIP(),
		StatusCode:          status,
		DurationMs:          time.Since(start).Milliseconds(),
		FirstResponseMs:     frt,
		Stream:              stream,
		InputTokens:         usage.InputTokens,
		OutputTokens:        usage.OutputTokens,
		CacheTokens:         usage.CacheTokens,
		CacheReadTokens:     usage.CacheReadTokens,
		CacheCreationTokens: usage.CacheCreationTokens,
		TotalTokens:         usage.TotalTokens,
		TotalCost:           baseCost,
		ActualCost:          finalCost,
		AccountCost:         finalCost,
		InputUnitPrice:      pricing.Prompt,
		OutputUnitPrice:     pricing.Completion,
		CacheReadUnitPrice:  pricing.CacheRead,
		CacheWriteUnitPrice: pricing.CacheCreation,
		GroupMultiplier:     groupMultiplier,
		BillingMode:         "token",
		Error:               errMsg,
	}
	_ = store.DB().Create(&entry).Error
}

func usageRequestID(c *gin.Context, key *store.Key, start time.Time) string {
	for _, header := range []string{"X-Request-Id", "X-Request-ID", "Request-Id", "Request-ID"} {
		if v := strings.TrimSpace(c.Writer.Header().Get(header)); v != "" {
			return v
		}
		if v := strings.TrimSpace(c.GetHeader(header)); v != "" {
			return v
		}
	}
	keyID := uint(0)
	if key != nil {
		keyID = key.ID
	}
	return fmt.Sprintf("req_%d_%d", start.UnixNano(), keyID)
}

type usageAccounting struct {
	InputTokens          int
	OutputTokens         int
	CacheTokens          int
	CacheReadTokens      int
	CacheCreationTokens  int
	TotalTokens          int
	CacheIncludedInInput bool
	TotalExplicit        bool
}

func usageFromResponse(proto config.Protocol, body []byte) *usageAccounting {
	var raw map[string]any
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil
	}
	u, _ := raw["usage"].(map[string]any)
	return usageFromRawMap(u, proto)
}

func usageFromIRUsage(resp *protocol.IRResponse) *usageAccounting {
	if resp == nil || resp.Usage == nil {
		return nil
	}
	u := resp.Usage
	acct := &usageAccounting{
		InputTokens:   u.PromptTokens,
		OutputTokens:  u.CompletionTokens,
		TotalTokens:   u.TotalTokens,
		TotalExplicit: u.TotalTokens > 0,
	}
	acct.recomputeTotalIfNeeded()
	if acct.InputTokens == 0 && acct.OutputTokens == 0 && acct.TotalTokens == 0 {
		return nil
	}
	return acct
}

func proxyStreamAndCaptureUsage(dst io.Writer, src io.Reader, proto config.Protocol, start time.Time) (*usageAccounting, int64, error) {
	scanner := bufio.NewScanner(src)
	scanner.Buffer(make([]byte, 256*1024), 4*1024*1024)
	var usage *usageAccounting
	var firstResponseMs int64
	for scanner.Scan() {
		line := append([]byte(nil), scanner.Bytes()...)
		if _, err := dst.Write(append(line, '\n')); err != nil {
			return usage, firstResponseMs, err
		}
		if firstResponseMs == 0 && isSSEDataLine(line) {
			firstResponseMs = time.Since(start).Milliseconds()
		}
		if nextUsage := usageFromSSELine(proto, line); nextUsage != nil {
			usage = mergeUsageAccounting(usage, nextUsage)
		}
	}
	if err := scanner.Err(); err != nil {
		return usage, firstResponseMs, err
	}
	return usage, firstResponseMs, nil
}

func isSSEDataLine(line []byte) bool {
	if !bytes.HasPrefix(line, []byte("data: ")) {
		return false
	}
	payload := bytes.TrimSpace(line[6:])
	return len(payload) > 0 && !bytes.Equal(payload, []byte("[DONE]"))
}

func mergeUsageAccounting(base, next *usageAccounting) *usageAccounting {
	if base == nil {
		return next
	}
	if next == nil {
		return base
	}
	if next.InputTokens > 0 {
		base.InputTokens = next.InputTokens
	}
	if next.OutputTokens > 0 {
		base.OutputTokens = next.OutputTokens
	}
	if next.CacheTokens > 0 {
		base.CacheTokens = next.CacheTokens
	}
	if next.CacheReadTokens > 0 {
		base.CacheReadTokens = next.CacheReadTokens
	}
	if next.CacheCreationTokens > 0 {
		base.CacheCreationTokens = next.CacheCreationTokens
	}
	base.CacheIncludedInInput = base.CacheIncludedInInput || next.CacheIncludedInInput
	if next.TotalExplicit {
		base.TotalTokens = next.TotalTokens
		base.TotalExplicit = true
	} else {
		base.recomputeTotalIfNeeded()
	}
	return base
}

func (u *usageAccounting) recomputeTotalIfNeeded() {
	if u == nil || u.TotalExplicit {
		return
	}
	u.TotalTokens = u.InputTokens + u.OutputTokens + u.CacheReadTokens
}

func usageFromSSEBuffer(proto config.Protocol, body []byte) *usageAccounting {
	scanner := bufio.NewScanner(bytes.NewReader(body))
	scanner.Buffer(make([]byte, 256*1024), 4*1024*1024)
	var usage *usageAccounting
	for scanner.Scan() {
		if next := usageFromSSELine(proto, scanner.Bytes()); next != nil {
			usage = mergeUsageAccounting(usage, next)
		}
	}
	return usage
}

func usageFromSSELine(proto config.Protocol, line []byte) *usageAccounting {
	if !bytes.HasPrefix(line, []byte("data: ")) {
		return nil
	}
	payload := bytes.TrimSpace(line[6:])
	if len(payload) == 0 || bytes.Equal(payload, []byte("[DONE]")) {
		return nil
	}
	var raw map[string]any
	if err := json.Unmarshal(payload, &raw); err != nil {
		return nil
	}
	switch proto {
	case config.ProtocolChat:
		return usageFromRawMap(objectField(raw, "usage"), proto)
	case config.ProtocolMessages:
		if usage := usageFromRawMap(objectField(raw, "usage"), proto); usage != nil {
			return usage
		}
		if msg := objectField(raw, "message"); msg != nil {
			return usageFromRawMap(objectField(msg, "usage"), proto)
		}
	case config.ProtocolResponses:
		if response := objectField(raw, "response"); response != nil {
			if usage := usageFromRawMap(objectField(response, "usage"), proto); usage != nil {
				return usage
			}
		}
		return usageFromRawMap(objectField(raw, "usage"), proto)
	}
	return nil
}

func usageFromRawMap(u map[string]any, _ config.Protocol) *usageAccounting {
	if len(u) == 0 {
		return nil
	}
	acct := &usageAccounting{}
	rawInputTokens := firstNumberField(u, "prompt_tokens", "input_tokens")
	acct.OutputTokens = firstNumberField(u, "completion_tokens", "output_tokens")
	acct.CacheReadTokens, acct.CacheIncludedInInput = cacheReadTokens(u)
	var cacheCreationIncluded bool
	acct.CacheCreationTokens, cacheCreationIncluded = cacheCreationTokens(u)
	acct.InputTokens = rawInputTokens
	if acct.CacheIncludedInInput && acct.CacheReadTokens > 0 {
		acct.InputTokens = maxInt(0, acct.InputTokens-acct.CacheReadTokens)
	}
	if !cacheCreationIncluded && acct.CacheCreationTokens > 0 {
		acct.InputTokens += acct.CacheCreationTokens
	}
	// CacheTokens is intentionally the cache-read/hit amount only. Cache
	// creation/write tokens are tracked separately but billed as regular input,
	// so they must not be mixed into cache-hit counters.
	acct.CacheTokens = acct.CacheReadTokens
	acct.TotalTokens = numberField(u, "total_tokens")
	acct.TotalExplicit = acct.TotalTokens > 0
	acct.recomputeTotalIfNeeded()
	return acct
}

func cacheReadTokens(u map[string]any) (int, bool) {
	direct, directKey := firstNumberFieldWithKey(u,
		"cache_read_input_tokens",
		"input_cache_read_tokens",
		"cache_read_tokens",
		"prompt_cache_hit_tokens",
		"prompt_cache_read_tokens",
		"cached_tokens",
	)
	directIncluded := directKey == "prompt_cache_hit_tokens" ||
		directKey == "prompt_cache_read_tokens" ||
		directKey == "cached_tokens"
	nested := 0
	for _, key := range []string{"prompt_tokens_details", "input_tokens_details"} {
		if details := objectField(u, key); details != nil {
			nested = maxInt(nested, firstNumberField(details,
				"cached_tokens",
				"cache_read_tokens",
				"cache_read_input_tokens",
				"input_cache_read_tokens",
				"read_tokens",
			))
		}
	}
	if nested > 0 {
		return maxInt(direct, nested), true
	}
	return direct, directIncluded
}

func cacheCreationTokens(u map[string]any) (int, bool) {
	total, directKey := firstNumberFieldWithKey(u,
		"cache_creation_input_tokens",
		"cache_write_input_tokens",
		"input_cache_write_tokens",
		"cache_creation_tokens",
		"prompt_cache_miss_tokens",
		"prompt_cache_write_tokens",
	)
	directIncluded := directKey == "prompt_cache_miss_tokens" ||
		directKey == "prompt_cache_write_tokens"
	detailTotal := 0
	if details := objectField(u, "cache_creation"); details != nil {
		for _, v := range details {
			detailTotal += numberValue(v)
		}
	}
	if details := objectField(u, "cache_creation_input_tokens_details"); details != nil {
		for _, v := range details {
			detailTotal += numberValue(v)
		}
	}
	total = maxInt(total, detailTotal)
	nested := 0
	for _, key := range []string{"prompt_tokens_details", "input_tokens_details"} {
		if details := objectField(u, key); details != nil {
			nested = maxInt(nested, firstNumberField(details,
				"cache_creation_tokens",
				"cache_creation_input_tokens",
				"cache_write_tokens",
				"cache_write_input_tokens",
				"input_cache_write_tokens",
				"created_tokens",
			))
		}
	}
	if nested > 0 {
		return maxInt(total, nested), true
	}
	return total, directIncluded
}

func numberField(m map[string]any, key string) int {
	return numberValue(m[key])
}

func firstNumberField(m map[string]any, keys ...string) int {
	n, _ := firstNumberFieldWithKey(m, keys...)
	return n
}

func firstNumberFieldWithKey(m map[string]any, keys ...string) (int, string) {
	for _, key := range keys {
		if n := numberField(m, key); n > 0 {
			return n, key
		}
	}
	return 0, ""
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
	case string:
		n, _ := strconv.ParseFloat(strings.TrimSpace(value), 64)
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
	pricing := usagePricing(route)
	inputTokens := usage.InputTokens
	cost := float64(inputTokens)*pricing.Prompt +
		float64(usage.OutputTokens)*pricing.Completion +
		float64(usage.CacheReadTokens)*pricing.CacheRead
	if cost <= 0 || math.IsNaN(cost) || math.IsInf(cost, 0) {
		return 0
	}
	return cost
}

type pricingSnapshot struct {
	Prompt        float64
	Completion    float64
	CacheRead     float64
	CacheCreation float64
}

func usagePricing(route config.ModelRoute) pricingSnapshot {
	if route.Pricing == nil {
		return pricingSnapshot{}
	}
	return pricingSnapshot{
		Prompt:        priceField(route.Pricing, "prompt"),
		Completion:    priceField(route.Pricing, "completion"),
		CacheRead:     priceField(route.Pricing, "input_cache_read", "cache_read", "prompt_cache_read"),
		CacheCreation: priceField(route.Pricing, "input_cache_write", "cache_write", "prompt_cache_write", "input_cache_creation"),
	}
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
