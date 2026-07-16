package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/xb0or/opencode-GO/config"
	"github.com/xb0or/opencode-GO/internal/router"
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
// NOTE(stability): This function is intentionally the ONLY orchestrator in
// proxy.go. It delegates to:
//   - decode.go         — request body parsing
//   - internal/router/  — model resolution & body rewriting
//   - protocol/         — request/response conversion
//   - pool/             — key picking & failure tracking
//   - stream.go         — response streaming (same/cross protocol)
//   - usage.go          — usage accounting & cost estimation
// Do NOT add business logic here — it belongs in the layer above.
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

	head := parseRequestBody(c.Request.URL.Path, body)
	upstreamBody := head.Body

	resolution := router.Resolve(head.Model, inbound)
	route := resolution.Route
	routed := !resolution.IsPassthrough
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
	rewritten, ok := router.RewriteRequestModel(upstreamBody, route.RealModel)
	if ok {
		upstreamBody = rewritten
	}
	if rewritten, ok := router.EnableRequestStreamUsage(upstreamBody, upstreamProto, head.Stream); ok {
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
