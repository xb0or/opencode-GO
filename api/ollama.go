package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/xb0or/opencode-GO/config"
	"github.com/xb0or/opencode-GO/internal/router"
	"github.com/xb0or/opencode-GO/pool"
	"github.com/xb0or/opencode-GO/protocol"
	"github.com/xb0or/opencode-GO/store"
	"github.com/xb0or/opencode-GO/upstream"
)

// proxyOllamaRequest handles an Ollama Cloud-routed request after the model has
// been resolved. It is called from proxyRequest — BEFORE the Go-specific
// PickAttempts / rewriteModel / ConvertRequest chain — so this function owns
// the entire Ollama request lifecycle and must not rely on any work already
// done by proxyRequest for the Go path.
//
// Ollama Cloud (https://ollama.com) exposes OpenAI-compatible endpoints
// (/v1/chat/completions, /v1/responses, /v1/messages) with Bearer auth.
// The target protocol is determined by route.TargetProtocol(ollama), which
// defaults to Chat if no per-upstream Target is configured.
//
// Flow:
//  1. Determine the upstream protocol via TargetProtocol.
//  2. Rewrite the model field to the real Ollama Cloud model id.
//  3. Cross-protocol conversion: when inbound != upstreamProto, convert the
//     request body from the inbound protocol to upstreamProto.
//  4. Enable stream_options.include_usage for Chat streaming.
//  5. Pick a key from the Ollama pool (each key is an Ollama API key).
//  6. Same-protocol: transparent reverse proxy with live SSE streaming.
//  7. Cross-protocol: read full response, convert back to inbound protocol.
//  8. Bookkeeping: mark key success/failure, write usage log.
//
// IMPORTANT: This function does NOT write to the client directly. It returns
// an attemptResult that the outer loop uses to decide what to do. The outer
// loop is the sole writer of client responses.
func proxyOllamaRequest(c *gin.Context, p *pool.Picker, route config.ModelRoute,
	inbound config.Protocol, upstreamBody []byte, head requestHead, start time.Time,
	resolvedGroup string) attemptResult {

	// G1: Use the group pre-resolved by the outer failover loop. Do NOT
	// re-resolve via route.TargetGroup — that would use a different priority
	// chain and could select a different group than the one used for token
	// permission and usage logging.
	_ = resolvedGroup // used below in PickAttempts

	// Determine the upstream protocol — respects TargetProtocol override.
	upstreamProto := route.TargetProtocol(config.UpstreamOllama)

	// Rewrite model to the real upstream model id (e.g. gpt-oss:120b).
	realModel := route.TargetRealModel(config.UpstreamOllama)
	rewritten, ok := router.RewriteRequestModel(upstreamBody, realModel)
	if ok {
		upstreamBody = rewritten
	}

	// Cross-protocol: when the inbound protocol differs from the upstream
	// protocol, convert the request body before forwarding.
	crossProto := head.HasModel && inbound != upstreamProto

	if crossProto {
		converted, convErr := protocol.ConvertRequest(inbound, upstreamProto, upstreamBody)
		if convErr != nil {
			return attemptResult{
				Status:    http.StatusBadRequest,
				Err:       fmt.Errorf("failed to convert request from %s to %s: %w", inbound, upstreamProto, convErr),
				ErrorType: "conversion_error",
				Retryable: false,
			}
		}
		upstreamBody = converted
	}

	// Parse stream flag from the (possibly converted) body.
	var parsedBody struct {
		Stream bool `json:"stream"`
	}
	_ = json.Unmarshal(upstreamBody, &parsedBody)
	stream := parsedBody.Stream

	// Enable stream_options.include_usage for Chat streaming so we get usage
	// in the final SSE chunk.
	if rewritten, ok := router.EnableRequestStreamUsage(upstreamBody, upstreamProto, stream); ok {
		upstreamBody = rewritten
	}

	// Pick key from the Ollama pool using the pre-resolved group (G1).
	attempts, err := p.PickAttempts(resolvedGroup)
	if err != nil {
		return attemptResult{
			Status:    http.StatusServiceUnavailable,
			Err:       fmt.Errorf("no available upstream key for group %s: %w", resolvedGroup, err),
			Retryable: true,
		}
	}

	baseURL := config.BaseURLFor(config.UpstreamOllama)
	// Use the correct endpoint path for the target protocol.
	target := baseURL + upstreamPathFor(upstreamProto)

	for i := range attempts {
		key := &attempts[i]
		p.MarkUsed(key.ID)

		// Create a fresh timeout context per key attempt. The cancel func
		// is called at every exit point via a closure to avoid accumulating
		// timers across multi-key retries.
		ctx, cancel := upstreamRequestContext(c.Request.Context(), stream)

		// attemptKey runs a single key attempt and returns the result.
		// cancel() is deferred here so it always runs when this lambda
		// returns — covering all code paths without scattering cancel()
		// calls across dozens of branches.
		result := func() attemptResult {
			defer cancel()

			req, reqErr := http.NewRequestWithContext(ctx, c.Request.Method, target, bytes.NewReader(upstreamBody))
			if reqErr != nil {
				markAndLog(c, p, key, route, inbound, http.StatusInternalServerError, start, stream, nil, reqErr.Error())
				return attemptResult{
					Status:    http.StatusInternalServerError,
					Err:       reqErr,
					Retryable: false,
				}
			}
			copyForwardHeaders(req.Header, c.Request.Header)
			setContentLength(req, len(upstreamBody))
			injectUpstreamAuth(req.Header, key.Value)

			upstreamClient := upstream.NewClientForProxy(key.ProxyURL)

			if crossProto {
				return proxyOllamaCrossProtocolKey(c, p, key, route, inbound, upstreamProto, stream, start,
					upstreamClient, req, i, attempts)
			}
			return proxyOllamaSameProtocolKey(c, p, key, route, inbound, stream, start,
				upstreamClient, req, i, attempts)
		}()

		// Check the result of this key attempt.
		if result.Terminal || result.Handled {
			return result
		}
		if result.Retryable && i+1 < len(attempts) {
			continue
		}
		// Non-retryable or last key — return the result to the outer loop.
		return result
	}

	return attemptResult{
		Status:    http.StatusBadGateway,
		Retryable: true,
	}
}

// proxyOllamaCrossProtocolKey handles a single key attempt for cross-protocol
// Ollama requests (e.g. Messages → Chat, or Responses → Chat). It does NOT
// call cancel() — the caller is responsible for canceling the context.
func proxyOllamaCrossProtocolKey(c *gin.Context, p *pool.Picker, key *store.Key,
	route config.ModelRoute, inbound config.Protocol, upstreamProto config.Protocol,
	stream bool, start time.Time,
	upstreamClient *http.Client, req *http.Request, i int, attempts []store.Key) attemptResult {

	resp, doErr := upstreamClient.Do(req)
	if doErr != nil {
		if status, _, message, ok := classifyProxyContextError(doErr); ok {
			markAndLog(c, p, key, route, inbound, status, start, stream, nil, message)
			if status == statusClientClosedRequest {
				return attemptResult{Terminal: true}
			}
			if i+1 < len(attempts) {
				return attemptResult{Retryable: true}
			}
			return attemptResult{
				Status:    status,
				Err:       doErr,
				Retryable: true,
			}
		}
		markKeyFailure(p, key, http.StatusBadGateway, nil)
		if i+1 < len(attempts) {
			return attemptResult{Retryable: true}
		}
		lastErrMsg := "failed to reach upstream: " + doErr.Error()
		markAndLog(c, p, key, route, inbound, http.StatusBadGateway, start, stream, nil, lastErrMsg)
		return attemptResult{
			Status:    http.StatusBadGateway,
			Err:       doErr,
			Retryable: true,
		}
	}

	// Ensure resp.Body is always closed explicitly — do NOT use defer
	// here because the for loop would accumulate deferred calls until
	// the function returns, leaking connections on multi-key retries.
	if shouldRetryWithNextKey(resp.StatusCode) && i+1 < len(attempts) {
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBodyRead))
		_ = resp.Body.Close()
		markKeyFailure(p, key, resp.StatusCode, errBody)
		return attemptResult{Retryable: true}
	}

	if shouldMarkUpstreamFailure(resp.StatusCode) {
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBodyRead))
		_ = resp.Body.Close()
		markKeyFailure(p, key, resp.StatusCode, errBody)
		markAndLog(c, p, key, route, inbound, resp.StatusCode, start, stream, nil, summarizeUpstreamError(resp.StatusCode, errBody))
		return attemptResult{
			Response:  resp,
			Status:   resp.StatusCode,
			Retryable: true,
		}
	}

	// P0-4: ALL remaining HTTP >= 400 responses (those not already handled
	// by shouldRetryWithNextKey/shouldMarkUpstreamFailure) must NOT enter
	// the cross-protocol streaming path. Preserve the original status.
	if resp.StatusCode >= 400 {
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBodyRead))
		_ = resp.Body.Close()
		markAndLog(c, p, key, route, inbound, resp.StatusCode, start, stream, nil, summarizeUpstreamError(resp.StatusCode, errBody))
		return attemptResult{
			Response:  resp,
			Status:    resp.StatusCode,
			Err:       fmt.Errorf("upstream returned %d", resp.StatusCode),
			Retryable: false,
		}
	}

	// Success — read the response body, then convert.
	if stream {
		// Cross-protocol SSE streaming: avoid buffering the full upstream
		// body. Use the incremental Decoder → IR → Encoder → Flush pipeline.
		rr := proxyCrossProtocolStream(c, resp, inbound, upstreamProto, p, key, route, start)
		if rr.ResponseStarted {
			return attemptResult{Handled: true}
		}
		if !rr.Retryable {
			return attemptResult{
				Response:  resp,
				Status:    resp.StatusCode,
				Err:       rr.Err,
				Retryable: false,
			}
		}
		if i+1 < len(attempts) {
			return attemptResult{Retryable: true}
		}
		return attemptResult{
			Status:    http.StatusBadGateway,
			Err:       rr.Err,
			Retryable: true,
		}
	}
	responseBody, readErr := io.ReadAll(io.LimitReader(resp.Body, maxNonStreamBodyRead))
	_ = resp.Body.Close()
	if readErr != nil {
		markKeyFailure(p, key, http.StatusBadGateway, nil)
		markAndLog(c, p, key, route, inbound, http.StatusBadGateway, start, stream, nil, readErr.Error())
		if i+1 < len(attempts) {
			return attemptResult{Retryable: true}
		}
		return attemptResult{
			Status:    http.StatusBadGateway,
			Err:       readErr,
			Retryable: true,
		}
	}
	rr := proxyCrossProtocolResponse(c, resp, stream, inbound, upstreamProto, p, key, route, start, responseBody)
	if rr.ResponseStarted {
		return attemptResult{Handled: true}
	}
	// Respect the response handler's retry classification. A non-retryable
	// client error (400/404/409/413/415/422) must NOT switch keys or
	// upstreams. Preserve the original status code so the client sees
	// the real error instead of a synthetic 502.
	if !rr.Retryable {
		return attemptResult{
			Response:  resp,
			Status:    resp.StatusCode,
			Err:       rr.Err,
			Retryable: false,
		}
	}
	if i+1 < len(attempts) {
		return attemptResult{Retryable: true}
	}
	return attemptResult{
		Status:    http.StatusBadGateway,
		Err:       rr.Err,
		Retryable: true,
	}
}

// proxyOllamaSameProtocolKey handles a single key attempt for same-protocol
// (Chat → Chat) Ollama requests. It does NOT call cancel() — the caller is
// responsible for canceling the context.
func proxyOllamaSameProtocolKey(c *gin.Context, p *pool.Picker, key *store.Key,
	route config.ModelRoute, inbound config.Protocol, stream bool, start time.Time,
	upstreamClient *http.Client, req *http.Request, i int, attempts []store.Key) attemptResult {

	resp, doErr := upstreamClient.Do(req)
	if doErr != nil {
		if status, _, message, ok := classifyProxyContextError(doErr); ok {
			markAndLog(c, p, key, route, inbound, status, start, stream, nil, message)
			if status == statusClientClosedRequest {
				return attemptResult{Terminal: true}
			}
			if i+1 < len(attempts) {
				return attemptResult{Retryable: true}
			}
			return attemptResult{
				Status:    status,
				Err:       doErr,
				Retryable: true,
			}
		}
		markKeyFailure(p, key, http.StatusBadGateway, nil)
		if i+1 < len(attempts) {
			return attemptResult{Retryable: true}
		}
		lastErrMsg := "failed to reach upstream: " + doErr.Error()
		markAndLog(c, p, key, route, inbound, http.StatusBadGateway, start, stream, nil, lastErrMsg)
		return attemptResult{
			Status:    http.StatusBadGateway,
			Err:       doErr,
			Retryable: true,
		}
	}

	// Ensure resp.Body is always closed explicitly — do NOT use defer
	// here because the for loop would accumulate deferred calls until
	// the function returns, leaking connections on multi-key retries.
	if shouldRetryWithNextKey(resp.StatusCode) && i+1 < len(attempts) {
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBodyRead))
		_ = resp.Body.Close()
		markKeyFailure(p, key, resp.StatusCode, errBody)
		return attemptResult{Retryable: true}
	}

	if shouldMarkUpstreamFailure(resp.StatusCode) {
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBodyRead))
		_ = resp.Body.Close()
		markKeyFailure(p, key, resp.StatusCode, errBody)
		markAndLog(c, p, key, route, inbound, resp.StatusCode, start, stream, nil, summarizeUpstreamError(resp.StatusCode, errBody))
		return attemptResult{
			Response:  resp,
			Status:   resp.StatusCode,
			Retryable: true,
		}
	}

	// Success — delegate to response handler.
	// For same-protocol, pass resp with body still open for live streaming.
	// proxySameProtocolResponse will close resp.Body. The deferred cancel()
	// in the caller's lambda fires after this function returns, which is
	// safe because resp.Body is already closed by the handler.
	rr := proxySameProtocolResponse(c, resp, stream, p, key, route, inbound, start, nil)
	if rr.ResponseStarted {
		return attemptResult{Handled: true}
	}
	// Respect the response handler's retry classification. A non-retryable
	// client error (400/404/409/413/415/422) must NOT switch keys or
	// upstreams. Preserve the original status code so the client sees
	// the real error instead of a synthetic 502.
	if !rr.Retryable {
		return attemptResult{
			Response:  resp,
			Status:    resp.StatusCode,
			Err:       rr.Err,
			Retryable: false,
		}
	}
	if i+1 < len(attempts) {
		return attemptResult{Retryable: true}
	}
	return attemptResult{
		Status:    http.StatusBadGateway,
		Err:       rr.Err,
		Retryable: true,
	}
}