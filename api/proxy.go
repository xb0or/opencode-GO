package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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

	// ErrorType is an optional explicit error type string (e.g. "conversion_error").
	// When set, it takes precedence over the status-based error type mapping.
	ErrorType string

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
		// http.MaxBytesReader (set in bodyLimitMiddleware) returns a
		// *http.MaxBytesError when the body exceeds the limit; that must
		// be reported as 413, not 400.
		var maxBytesErr *http.MaxBytesError
		if errors.As(err, &maxBytesErr) {
			writeOpenAIError(c, http.StatusRequestEntityTooLarge,
				"request_too_large", "request body exceeds the maximum allowed size")
			return
		}
		writeOpenAIError(c, http.StatusBadRequest, "invalid_request_error", "failed to read request body")
		return
	}
	_ = c.Request.Body.Close()

	head := parseRequestBody(c.Request.URL.Path, body)
	originalBody := head.Body

	resolution := router.Resolve(head.Model, inbound)
	if resolution.NotFound {
		writeOpenAIError(c, http.StatusNotFound, "model_not_found",
			"model not found: "+head.Model)
		return
	}
	route := resolution.Route
	routed := !resolution.IsPassthrough
	if routed && !route.IsEnabled() {
		writeOpenAIError(c, http.StatusForbidden, "model_disabled",
			"model is disabled by administrator: "+route.ID)
		return
	}

	// Stash the route's original group for logging/debugging.
	// The actual group used per-upstream is resolved inside the failover loop
	// via ResolveUpstreamGroup (G1).
	c.Set("group", route.Group)

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
		// G1: Resolve the key-pool group for this upstream using the single
		// authoritative resolver. The result is passed to every downstream
		// consumer (token permission, PickAttempts, Go/Ollama handlers,
		// usage log) so they all use the same group.
		upstreamGroup := route.ResolveUpstreamGroup(currentUpstream)

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
		// P1-1: do NOT mutate the original route — create a per-iteration
		// copy so the resolved group for one upstream does not leak into
		// the next iteration's ResolveUpstreamGroup call.
		routeCopy := route
		routeCopy.Group = upstreamGroup

		// --------------- Ollama upstream ---------------
		if currentUpstream == config.UpstreamOllama {
			// Pass the original body — proxyOllamaRequest handles all
			// conversion internally. Do NOT call buildUpstreamBody here
			// to avoid double conversion (the outer loop would convert
			// to Chat, then the helper would try to convert again).
			// G1: upstreamGroup is pre-resolved by the outer loop and passed
			// in so Ollama does NOT re-resolve via TargetGroup.
			result := proxyOllamaRequest(c, p, routeCopy, inbound, originalBody, head, start, upstreamGroup)
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
			break
		}

		// --------------- Go upstream ---------------
		result := proxyGoUpstream(c, p, routeCopy, inbound, head, originalBody, start, upstreamPath, currentUpstream, ui, upstreamsToTry, upstreamGroup)
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
		errType := lastResult.ErrorType
		if errType == "" {
			errType = "upstream_error"
		}
		writeOpenAIError(c, lastResult.Status, errType, genericUpstreamMessage(lastResult.Status))
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
	upstreamPath string, currentUpstream config.Upstream, ui int, upstreamsToTry []config.Upstream,
	upstreamGroup string) attemptResult {

	// Build per-upstream body from the original inbound body.
	upstreamProto := route.TargetProtocol(currentUpstream)
	upstreamBody := buildUpstreamBody(originalBody, route, inbound, head, upstreamPath, upstreamProto, currentUpstream)
	if upstreamBody == nil {
		return attemptResult{
			Status:    http.StatusBadRequest,
			Err:       errBodyConversion,
			ErrorType: "conversion_error",
			Retryable: false,
		}
	}

	baseURL := config.BaseURLFor(currentUpstream)
	// Use upstreamPathFor to ensure the URL path matches the target
	// protocol, not the inbound path. This is critical for cross-protocol
	// requests (e.g. inbound Messages → Go upstream speaking Chat).
	target := baseURL + upstreamPathFor(upstreamProto)

	attempts, err := p.PickAttempts(upstreamGroup)
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
		// Cancel immediately when this iteration ends (not deferred to
		// function return) to avoid accumulating timers across keys.
		ctx, cancel := upstreamRequestContext(c.Request.Context(), head.Stream)

		req, err := http.NewRequestWithContext(ctx, c.Request.Method, target, bytes.NewReader(upstreamBody))
		if err != nil {
			cancel()
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
			cancel()
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
			body, readErr := io.ReadAll(io.LimitReader(resp.Body, maxErrorBodyRead))
			_ = resp.Body.Close()
			cancel()
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
			body, readErr := io.ReadAll(io.LimitReader(resp.Body, maxErrorBodyRead))
			_ = resp.Body.Close()
			cancel()
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

		// P0-4 + P1-3: ALL remaining HTTP >= 400 responses must NOT enter
		// the cross-protocol streaming path. But we distinguish two retry
		// strategies:
		//   - 408 Request Timeout: the upstream timed out. Switching to the
		//     NEXT upstream may help, but do NOT retry the same key.
		//   - All other 4xx (400/404/405/409/410/412/413/414/415/416/422/
		//     423/424/425/426/428/431/451 etc.): non-retryable. The request
		//     itself is invalid or the upstream state won't change by
		//     switching keys/providers.
		// (Retryable errors like 401/402/403/429/5xx were already handled
		// above by shouldRetryWithNextKey/shouldMarkUpstreamFailure.)
		if resp.StatusCode >= 400 {
			body, readErr := io.ReadAll(io.LimitReader(resp.Body, maxErrorBodyRead))
			_ = resp.Body.Close()
			cancel()
			if readErr != nil {
				markAndLog(c, p, key, route, inbound, resp.StatusCode, start, head.Stream, nil, readErr.Error())
			} else {
				markAndLog(c, p, key, route, inbound, resp.StatusCode, start, head.Stream, nil, summarizeUpstreamError(resp.StatusCode, body))
			}
			// P1-3: 408 is upstream-retryable (try the next upstream) but
			// not key-retryable (don't re-use the same key).
			retryable := resp.StatusCode == http.StatusRequestTimeout && ui+1 < len(upstreamsToTry)
			return attemptResult{
				Response:  resp,
				Status:    resp.StatusCode,
				Err:       fmt.Errorf("upstream returned %d", resp.StatusCode),
				Retryable: retryable,
			}
		}

		// Success — delegate to the response handler. For same-protocol SSE,
		// the handler streams directly from resp.Body without buffering.
		// For cross-protocol or non-streaming, the handler reads resp.Body.
		// IMPORTANT: do NOT cancel() before the response handler reads
		// resp.Body — cancelling the context will abort an in-flight body
		// read. The handler closes resp.Body when done; cancel() is called
		// after the handler returns.
		var rr responseResult
		if crossProtocol(route, inbound, head, currentUpstream) {
			if head.Stream {
				// Cross-protocol SSE streaming: do NOT buffer the full
				// upstream body. Pre-read the first SSE event for
				// validation / failover, commit headers, then run the
				// incremental Decoder → IR → Encoder → Flush pipeline
				// over the remaining resp.Body. This avoids the old
				// buffer-then-re-emit pattern that delayed the entire
				// response until the upstream finished.
				rr = proxyCrossProtocolStream(c, resp, inbound, upstreamProto, p, key, route, start)
				cancel()
			} else {
				// Cross-protocol non-streaming: buffer full response.
				responseBody, readErr := io.ReadAll(io.LimitReader(resp.Body, maxNonStreamBodyRead))
				_ = resp.Body.Close()
				cancel()
				if readErr != nil {
					markKeyFailure(p, key, http.StatusBadGateway, nil)
					markAndLog(c, p, key, route, inbound, http.StatusBadGateway, start, head.Stream, nil, readErr.Error())
					if i+1 < len(attempts) {
						continue
					}
					return attemptResult{
						Status:    http.StatusBadGateway,
						Err:       readErr,
						Retryable: ui+1 < len(upstreamsToTry),
					}
				}
				rr = proxyCrossProtocolResponse(c, resp, head.Stream, inbound, upstreamProto, p, key, route, start, responseBody)
			}
		} else {
			// Same-protocol: pass resp with body still open for live streaming.
			// proxySameProtocolResponse will close resp.Body and we cancel
			// the context after it returns.
			rr = proxySameProtocolResponse(c, resp, head.Stream, p, key, route, inbound, start, nil)
			cancel()
		}

		if rr.ResponseStarted {
			return attemptResult{Handled: true}
		}
		// Respect the response handler's retry classification. A non-retryable
		// client error (400/404/409/413/415/422) must NOT switch keys or
		// upstreams — the same request will fail the same way elsewhere,
		// and switching providers can cause semantic drift. Preserve the
		// original upstream status code so the client sees the real error
		// (e.g. 400) instead of a synthetic 502.
		if !rr.Retryable {
			return attemptResult{
				Response:  resp,
				Status:    resp.StatusCode,
				Err:       rr.Err,
				Retryable: false,
			}
		}
		// Response not started — can try next key or upstream.
		if i+1 < len(attempts) {
			continue
		}
		return attemptResult{
			Status:    http.StatusBadGateway,
			Err:       rr.Err,
			Retryable: ui+1 < len(upstreamsToTry),
		}
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
func crossProtocol(route config.ModelRoute, inbound config.Protocol, head requestHead, currentUpstream config.Upstream) bool {
	return head.HasModel && inbound != route.TargetProtocol(currentUpstream)
}

// buildUpstreamBody constructs the request body for a specific upstream,
// starting from the original inbound body. This ensures each upstream gets
// a fresh conversion from the original client body, not a body that was
// already converted for a previous upstream.
//
// Returns nil on conversion failure.
// buildUpstreamBody constructs the request body for a specific upstream,
// starting from the original inbound body. This ensures each upstream gets
// a fresh conversion from the original client body, not a body that was
// already converted for a previous upstream.
//
// Returns nil on conversion failure.
func buildUpstreamBody(originalBody []byte, route config.ModelRoute, inbound config.Protocol, head requestHead, upstreamPath string, upstreamProto config.Protocol, currentUpstream config.Upstream) []byte {
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
	realModel := route.TargetRealModel(currentUpstream)
	if rewritten, ok := router.RewriteRequestModel(body, realModel); ok {
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
		if stripped, ok := protocol.StripToolChoiceForReasoning(body, realModel); ok {
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
