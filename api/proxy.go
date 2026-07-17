package api

import (
	"bytes"
	"context"
	"encoding/json"
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
//
// Do NOT add business logic here — it belongs in the layer above.
//
// Flow:
//  1. Read & parse the client body to find the requested model.
//  2. Resolve the model route.
//  3. For each upstream in the failover list:
//     a. Determine the upstream's key-pool group and re-check token permission.
//     b. Convert the request body from the inbound protocol to the upstream
//        protocol (if different), starting from the original inbound body.
//     c. Rewrite the model field, enable stream options, strip tool_choice.
//     d. Pick a KEY from the upstream's group.
//     e. Forward to the upstream with the KEY injected.
//     f. If cross-protocol: buffer the upstream response and convert it back.
//        If same-protocol: stream verbatim.
//     g. Bookkeeping: mark the KEY success/failure, write a usage log.
//  4. If all upstreams fail, return 502.
func proxyRequest(c *gin.Context, p *pool.Picker, inbound config.Protocol, upstreamPath string) {
	start := time.Now()

	// Read the original inbound body — this is immutable and used as the
	// starting point for per-upstream conversion.
	body, err := io.ReadAll(c.Request.Body)
	if err != nil {
		writeOpenAIError(c, http.StatusBadRequest, "invalid_request_error", "failed to read request body")
		return
	}
	_ = c.Request.Body.Close()

	head := parseRequestBody(c.Request.URL.Path, body)
	originalBody := head.Body

	resolution := router.Resolve(head.Model, inbound)
	route := resolution.Route
	routed := !resolution.IsPassthrough
	if routed && !route.IsEnabled() {
		writeOpenAIError(c, http.StatusForbidden, "model_disabled",
			"model is disabled by administrator: "+route.ID)
		return
	}

	// Permission check on the original route group.
	originalGroup := route.Group
	if tokAny, exists := c.Get("token"); exists {
		if tok, ok := tokAny.(*store.Token); ok && !pool.GroupAllowed(tok, originalGroup) {
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{
				"error": gin.H{
					"type":    "permission_denied",
					"message": "this token is not allowed to use group: " + originalGroup,
				},
			})
			return
		}
	}

	// Stash group for logging/debugging after the model route is known.
	c.Set("group", originalGroup)

	// -----------------------------------------------------------------------
	// Multi-upstream failover loop
	// -----------------------------------------------------------------------
	// Try each upstream in config order. The first one that succeeds wins.
	// On failure (network error, 4xx/5xx status) try the next upstream.
	// Each upstream is tried at most once — no circular retry.
	// SSE streaming that has already started sending data is never switched.
	upstreamsToTry := route.Upstreams
	if len(upstreamsToTry) == 0 {
		upstreamsToTry = []config.Upstream{route.Upstream}
	}
	var lastErrMsg string

	for ui, currentUpstream := range upstreamsToTry {
		route.Upstream = currentUpstream

		// Resolve the key-pool group for this upstream.
		upstreamGroup := route.UpstreamGroup(currentUpstream)

		// Re-check token permission for fallback groups.
		if upstreamGroup != originalGroup {
			if tokAny, exists := c.Get("token"); exists {
				if tok, ok := tokAny.(*store.Token); ok && !pool.GroupAllowed(tok, upstreamGroup) {
					lastErrMsg = "token not allowed to use group: " + upstreamGroup
					if ui+1 < len(upstreamsToTry) {
						continue
					}
					writeOpenAIError(c, http.StatusForbidden, "permission_denied", lastErrMsg)
					return
				}
			}
		}
		route.Group = upstreamGroup

		// --------------- Ollama upstream ---------------
		if currentUpstream == config.UpstreamOllama {
			// Build per-upstream body from the original inbound body.
			upstreamBody := buildUpstreamBody(originalBody, route, inbound, head, upstreamPath, config.ProtocolChat)
			if upstreamBody == nil {
				lastErrMsg = "failed to build upstream request body"
				if ui+1 < len(upstreamsToTry) {
					continue
				}
				writeOpenAIError(c, http.StatusInternalServerError, "internal_error", lastErrMsg)
				return
			}

			var handled bool
			proxyOllamaRequest(c, p, route, inbound, upstreamBody, head, start, &handled)
			if handled {
				return
			}
			lastErrMsg = "ollama upstream failed"
			continue
		}

		// --------------- Go upstream ---------------
		// Build per-upstream body from the original inbound body.
		upstreamBody := buildUpstreamBody(originalBody, route, inbound, head, upstreamPath, route.Protocol)
		if upstreamBody == nil {
			lastErrMsg = "failed to build upstream request body"
			if ui+1 < len(upstreamsToTry) {
				continue
			}
			writeOpenAIError(c, http.StatusInternalServerError, "internal_error", lastErrMsg)
			return
		}

		baseURL := config.BaseURLFor(route.Upstream)
		target := baseURL + upstreamPath

		attempts, err := p.PickAttempts(route.Group)
		if err != nil {
			lastErrMsg = "no available upstream key for group " + route.Group
			if ui+1 < len(upstreamsToTry) {
				continue
			}
			writeOpenAIError(c, http.StatusServiceUnavailable, "no_upstream_key_error", lastErrMsg)
			return
		}

		ctx, cancel := upstreamRequestContext(c.Request.Context(), head.Stream)
		defer cancel()

		for i := range attempts {
			key := &attempts[i]
			p.MarkUsed(key.ID)

			req, err := http.NewRequestWithContext(ctx, c.Request.Method, target, bytes.NewReader(upstreamBody))
			if err != nil {
				markAndLog(c, p, key, route, inbound, http.StatusInternalServerError, start, head.Stream, nil, err.Error())
				lastErrMsg = "failed to build upstream request: " + err.Error()
				break
			}
			copyForwardHeaders(req.Header, c.Request.Header)
			setContentLength(req, len(upstreamBody))
			injectUpstreamAuth(req.Header, key.Value)

			upstreamClient := upstream.NewClientForProxy(key.ProxyURL)
			resp, err := upstreamClient.Do(req)
			if err != nil {
				lastErrMsg = "failed to reach upstream: " + err.Error()
				if status, _, message, ok := classifyProxyContextError(err); ok {
					lastErrMsg = message
					markAndLog(c, p, key, route, inbound, status, start, head.Stream, nil, message)
					if status == statusClientClosedRequest {
						// Client disconnected — stop immediately, don't try next upstream.
						return
					}
					if i+1 < len(attempts) {
						continue
					}
					// Last key failed — try next upstream
					break
				}
				markKeyFailure(p, key, http.StatusBadGateway, nil)
				markAndLog(c, p, key, route, inbound, http.StatusBadGateway, start, head.Stream, nil, lastErrMsg)
				if i+1 < len(attempts) {
					continue
				}
				// Last key failed — try next upstream
				break
			}

			if shouldRetryWithNextKey(resp.StatusCode) && i+1 < len(attempts) {
				body, readErr := io.ReadAll(resp.Body)
				_ = resp.Body.Close()
				if readErr != nil {
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

			// Failure status — if there are more upstreams to try, break out
			// of the key loop and try the next upstream. On the last upstream,
			// stream the error response to the client as-is.
			if shouldMarkUpstreamFailure(resp.StatusCode) && ui+1 < len(upstreamsToTry) {
				body, readErr := io.ReadAll(resp.Body)
				_ = resp.Body.Close()
				if readErr != nil {
					markKeyFailure(p, key, resp.StatusCode, nil)
					lastErrMsg = "failed to read upstream error response: " + readErr.Error()
					markAndLog(c, p, key, route, inbound, http.StatusBadGateway, start, head.Stream, nil, lastErrMsg)
				} else {
					markKeyFailure(p, key, resp.StatusCode, body)
					lastErrMsg = summarizeUpstreamError(resp.StatusCode, body)
					markAndLog(c, p, key, route, inbound, resp.StatusCode, start, head.Stream, nil, lastErrMsg)
				}
				break
			}

			// Success — cancel() is safe here because:
			// - For streaming: upstreamRequestContext returns a no-op cancel.
			// - For non-streaming: the response body is fully consumed by
			//   proxyCrossProtocolResponse or proxySameProtocolResponse before
			//   we reach this point (they buffer or stream to completion).
			defer resp.Body.Close()
			if crossProtocol(route, inbound, head) {
				proxyCrossProtocolResponse(c, resp, head.Stream, inbound, route.Protocol, p, key, route, start)
			} else {
				proxySameProtocolResponse(c, resp, head.Stream, p, key, route, inbound, start)
			}
			return
		}

		// All keys for this upstream failed — try the next upstream if any.
		if ui+1 < len(upstreamsToTry) {
			continue
		}
	}

	if lastErrMsg == "" {
		lastErrMsg = "all upstreams failed"
	}
	writeOpenAIError(c, http.StatusBadGateway, "upstream_error", genericUpstreamMessage(http.StatusBadGateway))
}

// crossProtocol reports whether the request needs cross-protocol conversion
// for the given route. It is called per-upstream because each upstream may
// speak a different protocol.
func crossProtocol(route config.ModelRoute, inbound config.Protocol, head requestHead) bool {
	return head.HasModel && inbound != route.Protocol
}

// buildUpstreamBody constructs the request body for a specific upstream,
// starting from the original inbound body. This ensures each upstream gets
// a fresh conversion from the original client body, not a body that was
// already converted for a previous upstream.
//
// Returns nil on conversion failure.
func buildUpstreamBody(originalBody []byte, route config.ModelRoute, inbound config.Protocol, head requestHead, upstreamPath string, upstreamProto config.Protocol) []byte {
	body := originalBody

	// Cross-protocol conversion: if the inbound protocol differs from the
	// upstream protocol, convert through the IR.
	if head.HasModel && inbound != upstreamProto {
		converted, err := protocol.ConvertRequest(inbound, upstreamProto, body)
		if err != nil {
			return nil
		}
		body = converted
		// Re-parse stream flag from the converted body.
		var h struct {
			Stream bool `json:"stream"`
		}
		_ = json.Unmarshal(body, &h)
		head.Stream = h.Stream
	}

	// Rewrite the model field to the upstream's real model id.
	if rewritten, ok := router.RewriteRequestModel(body, route.RealModel); ok {
		body = rewritten
	}

	// Enable stream_options.include_usage for Chat streaming.
	if rewritten, ok := router.EnableRequestStreamUsage(body, upstreamProto, head.Stream); ok {
		body = rewritten
	}

	// Reasoning/thinking models (e.g. DeepSeek) reject non-auto tool_choice
	// values while thinking mode is active. Strip tool_choice to avoid an
	// HTTP 400 from the upstream before the request is sent.
	if upstreamProto == config.ProtocolChat {
		if stripped, ok := protocol.StripToolChoiceForReasoning(body, route.RealModel); ok {
			body = stripped
		}
	}

	return body
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
