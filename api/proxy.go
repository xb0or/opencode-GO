package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/xb0or/opencode-GO/config"
	"github.com/xb0or/opencode-GO/pool"
	"github.com/xb0or/opencode-GO/protocol"
	"github.com/xb0or/opencode-GO/store"
	"github.com/xb0or/opencode-GO/upstream"
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
	// Reasoning/thinking models (e.g. DeepSeek) reject non-auto tool_choice
	// values while thinking mode is active. Strip tool_choice to avoid an
	// HTTP 400 from the upstream before the request is sent.
	if upstreamProto == config.ProtocolChat {
		if stripped, ok := protocol.StripToolChoiceForReasoning(upstreamBody, route.RealModel); ok {
			upstreamBody = stripped
		}
	}

	// Determine the upstream endpoint path.  If the client called /v1/messages
	// but the model speaks "chat" upstream, we must hit /v1/chat/completions.
	actualUpstreamPath := upstreamPath
	if crossProtocol {
		actualUpstreamPath = upstreamPathFor(upstreamProto)
	}

	baseURL := config.BaseURLFor(route.Upstream)
	target := baseURL + actualUpstreamPath

	ctx, cancel := upstreamRequestContext(c.Request.Context(), head.Stream)
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
			lastErrMsg = "failed to reach upstream: " + err.Error()
			if status, errType, message, ok := classifyProxyContextError(err); ok {
				lastErrMsg = message
				markAndLog(c, p, key, route, inbound, status, start, head.Stream, nil, message)
				if i+1 < len(attempts) && status != statusClientClosedRequest {
					continue
				}
				writeOpenAIError(c, status, errType, message)
				return
			}
			markKeyFailure(p, key, http.StatusBadGateway, nil)
			markAndLog(c, p, key, route, inbound, http.StatusBadGateway, start, head.Stream, nil, lastErrMsg)
			if i+1 < len(attempts) {
				continue
			}
			// Don't expose the raw network error (which may contain the
			// upstream host/URL) to the client.
			writeOpenAIError(c, http.StatusBadGateway, "upstream_error", genericUpstreamMessage(http.StatusBadGateway))
			return
		}

		if shouldRetryWithNextKey(resp.StatusCode) && i+1 < len(attempts) {
			body, readErr := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if readErr != nil {
				if status, errType, message, ok := classifyProxyContextError(readErr); ok {
					lastErrMsg = message
					markAndLog(c, p, key, route, inbound, status, start, head.Stream, nil, message)
					writeOpenAIError(c, status, errType, message)
					return
				}
				markKeyFailure(p, key, resp.StatusCode, nil)
				lastErrMsg = "failed to read upstream error response: " + readErr.Error()
				markAndLog(c, p, key, route, inbound, http.StatusBadGateway, start, head.Stream, nil, lastErrMsg)
			} else {
				markKeyFailure(p, key, resp.StatusCode, body)
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
	writeOpenAIError(c, http.StatusBadGateway, "upstream_error", genericUpstreamMessage(http.StatusBadGateway))
}

// proxySameProtocolResponse streams the upstream response verbatim to the client.
func proxySameProtocolResponse(c *gin.Context, resp *http.Response, stream bool,
	p *pool.Picker, key *store.Key, route config.ModelRoute, inbound config.Protocol, start time.Time) {

	if resp.StatusCode >= 400 {
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			if status, errType, message, ok := classifyProxyContextError(err); ok {
				markAndLog(c, p, key, route, inbound, status, start, stream, nil, message)
				writeOpenAIError(c, status, errType, message)
				return
			}
			markKeyFailure(p, key, http.StatusBadGateway, nil)
			markAndLog(c, p, key, route, inbound, http.StatusBadGateway, start, stream, nil, err.Error())
			writeOpenAIError(c, http.StatusBadGateway, "upstream_error", genericUpstreamMessage(http.StatusBadGateway))
			return
		}
		errMsg := summarizeUpstreamError(resp.StatusCode, body)
		if shouldMarkUpstreamFailure(resp.StatusCode) {
			markKeyFailure(p, key, resp.StatusCode, body)
		}
		markAndLog(c, p, key, route, inbound, resp.StatusCode, start, stream, nil, errMsg)
		// Don't pass the raw upstream error body to the client — it may
		// expose provider/channel information. Return a generic error
		// envelope instead; the raw detail is kept in the admin usage log.
		writeOpenAIError(c, resp.StatusCode, upstreamErrorType(resp.StatusCode), genericUpstreamMessage(resp.StatusCode))
		return
	}

	if !stream {
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			if status, errType, message, ok := classifyProxyContextError(err); ok {
				markAndLog(c, p, key, route, inbound, status, start, false, nil, message)
				writeOpenAIError(c, status, errType, message)
				return
			}
			markKeyFailure(p, key, http.StatusBadGateway, nil)
			markAndLog(c, p, key, route, inbound, http.StatusBadGateway, start, false, nil, err.Error())
			writeOpenAIError(c, http.StatusBadGateway, "upstream_error", "failed to read upstream response")
			return
		}
		copyResponseHeaders(c, resp)
		c.Writer.WriteHeader(resp.StatusCode)
		_, writeErr := c.Writer.Write(body)
		usage := usageFromResponse(inbound, body)
		errMsg := copyErrString(writeErr)
		if errMsg == "" && resp.StatusCode >= http.StatusBadRequest {
			errMsg = summarizeUpstreamError(resp.StatusCode, body)
		}
		if resp.StatusCode < 400 && writeErr == nil {
			p.MarkSuccess(key.ID)
			markAndLog(c, p, key, route, inbound, resp.StatusCode, start, false, usage, "")
		} else if shouldMarkUpstreamFailure(resp.StatusCode) {
			markKeyFailure(p, key, resp.StatusCode, body)
			markAndLog(c, p, key, route, inbound, resp.StatusCode, start, false, usage, errMsg)
		} else {
			markAndLog(c, p, key, route, inbound, resp.StatusCode, start, false, usage, errMsg)
		}
		return
	}

	copyResponseHeaders(c, resp)
	c.Writer.WriteHeader(resp.StatusCode)
	usage, firstResponseMs, copyErr := proxyStreamAndCaptureUsage(c.Writer, resp.Body, inbound, start)

	if resp.StatusCode < 400 && copyErr == nil {
		p.MarkSuccess(key.ID)
		markAndLog(c, p, key, route, inbound, resp.StatusCode, start, stream, usage, "", firstResponseMs)
	} else if status, _, message, ok := classifyProxyContextError(copyErr); ok {
		markAndLog(c, p, key, route, inbound, status, start, stream, usage, message, firstResponseMs)
	} else if shouldMarkUpstreamFailure(resp.StatusCode) {
		markKeyFailure(p, key, resp.StatusCode, nil)
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
		// Don't convert error responses; return a generic error to hide
		// upstream provider/channel details from the client.
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			if status, errType, message, ok := classifyProxyContextError(err); ok {
				markAndLog(c, p, key, route, inbound, status, start, stream, nil, message)
				writeOpenAIError(c, status, errType, message)
				return
			}
			markKeyFailure(p, key, http.StatusBadGateway, nil)
			markAndLog(c, p, key, route, inbound, http.StatusBadGateway, start, stream, nil, err.Error())
			writeOpenAIError(c, http.StatusBadGateway, "upstream_error", genericUpstreamMessage(http.StatusBadGateway))
			return
		}
		if shouldMarkUpstreamFailure(resp.StatusCode) {
			markKeyFailure(p, key, resp.StatusCode, body)
		}
		markAndLog(c, p, key, route, inbound, resp.StatusCode, start, stream, nil, summarizeUpstreamError(resp.StatusCode, body))
		writeOpenAIError(c, resp.StatusCode, upstreamErrorType(resp.StatusCode), genericUpstreamMessage(resp.StatusCode))
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
			if status, errType, message, ok := classifyProxyContextError(err); ok {
				markAndLog(c, p, key, route, inbound, status, start, true, nil, message)
				writeOpenAIError(c, status, errType, message)
				return
			}
			markKeyFailure(p, key, http.StatusBadGateway, nil)
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
			markKeyFailure(p, key, http.StatusBadGateway, upstreamBody)
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
			if status, errType, message, ok := classifyProxyContextError(err); ok {
				markAndLog(c, p, key, route, inbound, status, start, false, nil, message)
				writeOpenAIError(c, status, errType, message)
				return
			}
			markKeyFailure(p, key, http.StatusBadGateway, nil)
			markAndLog(c, p, key, route, inbound, http.StatusBadGateway, start, false, nil, err.Error())
			writeOpenAIError(c, http.StatusBadGateway, "upstream_error", "failed to read upstream response")
			return
		}

		converted, err := protocol.ConvertResponse(upstreamProto, inbound, upstreamBody)
		if err != nil {
			// Upstream returned a body that is not valid JSON for its protocol
			// (commonly an HTML error page from an upstream proxy/CDN). Report
			// the real cause instead of the opaque JSON parse error.
			markKeyFailure(p, key, http.StatusBadGateway, upstreamBody)
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

// upstreamErrorType maps an upstream HTTP status to an OpenAI-style error type
// that is safe to return to the client.
func upstreamErrorType(status int) string {
	switch {
	case status == http.StatusTooManyRequests:
		return "rate_limit_error"
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return "permission_denied"
	case status == http.StatusRequestTimeout, status == http.StatusGatewayTimeout:
		return "upstream_timeout"
	case status >= 500:
		return "upstream_error"
	default:
		return "upstream_error"
	}
}

// genericUpstreamMessage returns a client-safe error message that does not
// expose upstream provider/channel details (e.g. "Error from provider
// (DeepSeek)"). The raw upstream error is still recorded in the admin usage
// log via summarizeUpstreamError for debugging.
func genericUpstreamMessage(status int) string {
	switch {
	case status == http.StatusTooManyRequests:
		return "upstream rate limit reached, please retry later"
	case status == http.StatusUnauthorized:
		return "upstream authentication failed"
	case status == http.StatusForbidden:
		return "upstream access denied"
	case status == http.StatusRequestTimeout, status == http.StatusGatewayTimeout:
		return "upstream request timed out"
	case status >= 500:
		return "upstream service error"
	default:
		return "upstream request failed"
	}
}

const statusClientClosedRequest = 499

func upstreamRequestContext(parent context.Context, stream bool) (context.Context, context.CancelFunc) {
	if stream {
		return parent, func() {}
	}
	timeout := upstream.Timeout()
	if timeout <= 0 {
		return parent, func() {}
	}
	return context.WithTimeout(parent, timeout)
}

func classifyProxyContextError(err error) (int, string, string, bool) {
	if err == nil {
		return 0, "", "", false
	}
	if errors.Is(err, context.Canceled) {
		return statusClientClosedRequest, "client_closed_request", "client canceled request", true
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return http.StatusGatewayTimeout, "upstream_timeout", "upstream request timed out", true
	}
	return 0, "", "", false
}

func markKeyFailure(p *pool.Picker, key *store.Key, status int, body []byte) {
	if p == nil || key == nil {
		return
	}
	p.MarkFailureWithQuota(key.ID, status, body, key.QuotaSnapshot)
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

// writeOpenAIError emits an OpenAI-style error envelope.
func writeOpenAIError(c *gin.Context, status int, errType, message string) {
	c.JSON(status, gin.H{
		"error": gin.H{
			"type":    errType,
			"message": message,
		},
	})
}
