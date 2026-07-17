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

// attemptResult is the structured return value from a single upstream attempt.
// The outer loop is the sole writer of client responses — helpers must not
// write to the client directly.
type attemptResult struct {
	// Response is set when the upstream returned a response that should be
	// forwarded to the client (success or last-upstream error).
	Response *http.Response

	// Status is the HTTP status code from the upstream (or a synthetic one).
	Status int

	// Err is set when a non-recoverable error occurred.
	Err error

	// Retryable indicates whether the caller should try the next upstream.
	Retryable bool

	// Terminal indicates that the request should be aborted immediately
	// (e.g., client disconnected). No further upstreams should be tried.
	Terminal bool

	// Handled indicates that a successful response was written to the client.
	Handled bool
}

// proxyRequest is the shared handler with full cross-protocol conversion.
//
// NOTE(stability): This function is intentionally the ONLY orchestrator in
// proxy.go. It delegates to:
//   - decode.go             — request body parsing
//   - internal/router/      — model resolution & body rewriting
//   - protocol/             — request/response conversion
//   - pool/                 — key picking & failure tracking
//   - stream.go             — response streaming (same/cross protocol)
//   - usage.go              — usage accounting & cost estimation
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

	// Stash the route's original group for logging/debugging.
	// The actual group used per-upstream is resolved inside the failover loop.
	c.Set("group", route.Group)

	// Save the original route before the failover loop so we can fall back
	// to the original Group when no explicit UpstreamGroups mapping exists.
	baseRoute := route

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
	var lastResult attemptResult
	var attemptedAny bool

	for ui, currentUpstream := range upstreamsToTry {
		// Resolve the key-pool group for this upstream.
		// When no explicit UpstreamGroups mapping exists and this upstream
		// is the original primary upstream, fall back to baseRoute.Group
		// for backward compatibility with old {Group: "premium"} configs.
		upstreamGroup := route.UpstreamGroup(currentUpstream)
		if upstreamGroup == string(currentUpstream) && currentUpstream == baseRoute.Upstream && baseRoute.Group != "" {
			upstreamGroup = baseRoute.Group
		}

		// Check token permission for this upstream's resolved group.
		// Each upstream may map to a different group via UpstreamGroups,
		// so we must re-check every iteration.
		// Permission-denied upstreams are silently skipped — they do NOT
		// overwrite lastResult so a real upstream failure is preserved.
		if tokAny, exists := c.Get("token"); exists {
			if tok, ok := tokAny.(*store.Token); ok && !pool.GroupAllowed(tok, upstreamGroup) {
				if ui+1 < len(upstreamsToTry) {
					continue
				}
				// Last upstream and no upstream was ever attempted — return 403.
				if !attemptedAny {
					writeOpenAIError(c, http.StatusForbidden, "permission_denied",
						"this token is not allowed to use group: "+upstreamGroup)
					return
				}
				// A real upstream was attempted — fall through to return its result.
				break
			}
		}
		attemptedAny = true
		route.Group = upstreamGroup

		// --------------- Ollama upstream ---------------
		if currentUpstream == config.UpstreamOllama {
			// Pass the original body — proxyOllamaRequest handles all
			// conversion internally. Do NOT call buildUpstreamBody here
			// to avoid double conversion (the outer loop would convert
			// to Chat, then the helper would try to convert again).
			result := proxyOllamaRequest(c, p, route, inbound, originalBody, head, start)
			if result.Terminal {
				// Client disconnected — stop immediately.
				return
			}
			if result.Handled {
				return
			}
			lastResult = result
			if result.Retryable && ui+1 < len(upstreamsToTry) {
				continue
			}
			// Non-retryable failure or last upstream — fall through to 502
			// or preserved error response.
			break
		}

		// --------------- Go upstream ---------------
		result := proxyGoUpstream(c, p, route, inbound, head, originalBody, start, upstreamPath, currentUpstream, ui, upstreamsToTry)
		if result.Terminal {
			return
		}
		if result.Handled {
			return
		}
		lastResult = result
		if result.Retryable {
			continue
		}
		break
	}

	// All upstreams failed. If the last upstream returned a real error response
	// (not a synthetic 502), preserve its control headers (Retry-After,
	// RateLimit-*, etc.) but use a generic error message for the body to avoid
	// leaking provider/channel details. Representation headers (Content-Length,
	// Content-Encoding, etc.) are deliberately NOT copied — they describe the
	// upstream's body, not the replacement body we will write.
	if lastResult.Response != nil {
		copyControlHeaders(c, lastResult.Response)
		_ = lastResult.Response.Body.Close()
		writeOpenAIError(c, lastResult.Status, upstreamErrorType(lastResult.Status), genericUpstreamMessage(lastResult.Status))
		return
	}
	if lastResult.Err != nil {
		writeOpenAIError(c, lastResult.Status, "upstream_error", genericUpstreamMessage(lastResult.Status))
		return
	}
	// No upstream was ever attempted — all were skipped due to permission.
	if !attemptedAny {
		writeOpenAIError(c, http.StatusForbidden, "permission_denied",
			"this token is not allowed to use any upstream group")
		return
	}
	writeOpenAIError(c, http.StatusBadGateway, "upstream_error", genericUpstreamMessage(http.StatusBadGateway))
}

// proxyGoUpstream handles a single Go upstream attempt, returning an
// attemptResult. It does NOT write to the client directly — the outer
// loop is the sole response writer.
func proxyGoUpstream(c *gin.Context, p *pool.Picker, route config.ModelRoute,
	inbound config.Protocol, head requestHead, originalBody []byte, start time.Time,
	upstreamPath string, currentUpstream config.Upstream, ui int, upstreamsToTry []config.Upstream) attemptResult {

	// Build per-upstream body from the original inbound body.
	upstreamBody := buildUpstreamBody(originalBody, route, inbound, head, upstreamPath, route.Protocol)
	if upstreamBody == nil {
		return attemptResult{
			Status:    http.StatusInternalServerError,
			Err:       errBodyConversion,
			Retryable: ui+1 < len(upstreamsToTry),
		}
	}

	baseURL := config.BaseURLFor(currentUpstream)
	// Use upstreamPathFor to ensure the URL path matches the target
	// protocol, not the inbound path. This is critical for cross-protocol
	// requests (e.g. inbound Messages → Go upstream speaking Chat).
	target := baseURL + upstreamPathFor(route.Protocol)

	attempts, err := p.PickAttempts(route.Group)
	if err != nil {
		return attemptResult{
			Status:    http.StatusServiceUnavailable,
			Err:       err,
			Retryable: ui+1 < len(upstreamsToTry),
		}
	}

	for i := range attempts {
		key := &attempts[i]
		p.MarkUsed(key.ID)

		// Create a fresh timeout context per key attempt so a timeout
		// on one key does not poison the context for subsequent keys.
		ctx, cancel := upstreamRequestContext(c.Request.Context(), head.Stream)
		defer cancel()

		req, err := http.NewRequestWithContext(ctx, c.Request.Method, target, bytes.NewReader(upstreamBody))
		if err != nil {
			markAndLog(c, p, key, route, inbound, http.StatusInternalServerError, start, head.Stream, nil, err.Error())
			return attemptResult{
				Status:    http.StatusInternalServerError,
				Err:       err,
				Retryable: false,
			}
		}
		copyForwardHeaders(req.Header, c.Request.Header)
		setContentLength(req, len(upstreamBody))
		injectUpstreamAuth(req.Header, key.Value)

		upstreamClient := upstream.NewClientForProxy(key.ProxyURL)
		resp, err := upstreamClient.Do(req)
		if err != nil {
			if status, _, message, ok := classifyProxyContextError(err); ok {
				markAndLog(c, p, key, route, inbound, status, start, head.Stream, nil, message)
				if status == statusClientClosedRequest {
					return attemptResult{Terminal: true}
				}
				if i+1 < len(attempts) {
					continue
				}
				return attemptResult{
					Status:    status,
					Err:       err,
					Retryable: ui+1 < len(upstreamsToTry),
				}
			}
			markKeyFailure(p, key, http.StatusBadGateway, nil)
			lastErrMsg := "failed to reach upstream: " + err.Error()
			markAndLog(c, p, key, route, inbound, http.StatusBadGateway, start, head.Stream, nil, lastErrMsg)
			if i+1 < len(attempts) {
				continue
			}
			return attemptResult{
				Status:    http.StatusBadGateway,
				Err:       err,
				Retryable: ui+1 < len(upstreamsToTry),
			}
		}

		if shouldRetryWithNextKey(resp.StatusCode) && i+1 < len(attempts) {
			body, readErr := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if readErr != nil {
				markKeyFailure(p, key, resp.StatusCode, nil)
				markAndLog(c, p, key, route, inbound, http.StatusBadGateway, start, head.Stream, nil, readErr.Error())
			} else {
				markKeyFailure(p, key, resp.StatusCode, body)
				markAndLog(c, p, key, route, inbound, resp.StatusCode, start, head.Stream, nil, summarizeUpstreamError(resp.StatusCode, body))
			}
			continue
		}

		// Failure status — if there are more upstreams to try, break out
			// of the key loop and try the next upstream. On the last upstream,
			// preserve the status and headers but use a generic error message.
			if shouldMarkUpstreamFailure(resp.StatusCode) {
				body, readErr := io.ReadAll(resp.Body)
				_ = resp.Body.Close()
				if readErr != nil {
					markKeyFailure(p, key, resp.StatusCode, nil)
					markAndLog(c, p, key, route, inbound, http.StatusBadGateway, start, head.Stream, nil, readErr.Error())
				} else {
					markKeyFailure(p, key, resp.StatusCode, body)
					markAndLog(c, p, key, route, inbound, resp.StatusCode, start, head.Stream, nil, summarizeUpstreamError(resp.StatusCode, body))
				}
				if ui+1 < len(upstreamsToTry) {
					return attemptResult{
						Response:  resp,
						Status:    resp.StatusCode,
						Retryable: true,
					}
				}
				// Last upstream — preserve the original error response
				// for status/headers, but use generic error message for body.
				return attemptResult{
					Response:  resp,
					Status:    resp.StatusCode,
					Retryable: false,
				}
			}

		// Success
		defer resp.Body.Close()
		if crossProtocol(route, inbound, head) {
			proxyCrossProtocolResponse(c, resp, head.Stream, inbound, route.Protocol, p, key, route, start)
		} else {
			proxySameProtocolResponse(c, resp, head.Stream, p, key, route, inbound, start)
		}
		return attemptResult{Handled: true}
	}

	// All keys for this upstream failed.
	return attemptResult{
		Status:    http.StatusBadGateway,
		Retryable: ui+1 < len(upstreamsToTry),
	}
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

// copyControlHeaders copies only control/retry headers from an upstream error
// response, deliberately skipping representation headers (Content-Length,
// Content-Encoding, Content-Type, ETag, Digest, Content-MD5) that describe
// the upstream's body — not the replacement body we will write.
//
// Headers preserved:
//   - Retry-After, RetryAfter
//   - WWW-Authenticate
//   - RateLimit-*, X-RateLimit-*
//   - X-Request-Id (for tracing)
func copyControlHeaders(c *gin.Context, resp *http.Response) {
	for k, vs := range resp.Header {
		if isHopHeader(k) {
			continue
		}
		lower := strings.ToLower(k)
		switch {
		case lower == "retry-after",
			lower == "www-authenticate",
			lower == "x-request-id":
			// pass
		case strings.HasPrefix(lower, "ratelimit-"),
			strings.HasPrefix(lower, "x-ratelimit-"):
			// pass
		default:
			continue
		}
		for _, v := range vs {
			c.Writer.Header().Add(k, v)
		}
	}
}

// Sentinel errors for attemptResult.
var (
	errBodyConversion    = &attemptError{"failed to build upstream request body"}
	errPoolGroupNotAllowed = func(group string) error {
		return &attemptError{"token not allowed to use group: " + group}
	}
)

type attemptError struct{ msg string }

func (e *attemptError) Error() string { return e.msg }
