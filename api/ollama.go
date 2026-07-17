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
// Ollama Cloud (https://ollama.com) exposes an OpenAI-compatible
// /v1/chat/completions endpoint with Bearer auth, identical wire format to
// the Go upstream. This means same-protocol (Chat→Chat) is a pure transparent
// reverse proxy — no body conversion needed.
//
// Flow:
//  1. Rewrite the model field to the real Ollama Cloud model id.
//  2. Cross-protocol conversion: when inbound != Chat (e.g. Messages or
//     Responses), convert the request body from the inbound protocol to Chat.
//  3. Enable stream_options.include_usage for Chat streaming.
//  4. Pick a key from the Ollama pool (each key is an Ollama API key).
//  5. Same-protocol (Chat→Chat): use httputil.ReverseProxy for zero-buffer
//     transparent SSE passthrough — SSE data blocks flush direct to client.
//  6. Cross-protocol (Messages/Responses → Chat): read full response, convert
//     back to inbound protocol, relay to client.
//  7. Bookkeeping: mark key success/failure, write usage log.
//
// IMPORTANT: This function does NOT write to the client directly. It returns
// an attemptResult that the outer loop uses to decide what to do. The outer
// loop is the sole writer of client responses.
func proxyOllamaRequest(c *gin.Context, p *pool.Picker, route config.ModelRoute,
	inbound config.Protocol, upstreamBody []byte, head requestHead, start time.Time) attemptResult {

	// Rewrite model to the real upstream model id (e.g. gpt-oss:120b).
	realModel := route.TargetRealModel(config.UpstreamOllama)
	rewritten, ok := router.RewriteRequestModel(upstreamBody, realModel)
	if ok {
		upstreamBody = rewritten
	}

	// Cross-protocol: Ollama speaks Chat natively. Any inbound protocol that
	// is not Chat must be converted to Chat before forwarding.
	crossProtocol := inbound != config.ProtocolChat

	if crossProtocol {
		converted, convErr := protocol.ConvertRequest(inbound, config.ProtocolChat, upstreamBody)
		if convErr != nil {
			return attemptResult{
				Status:    http.StatusBadRequest,
				Err:       fmt.Errorf("failed to convert request from %s to chat: %w", inbound, convErr),
				Retryable: true,
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
	// in the final SSE chunk (best-effort — transparent proxy may not parse it).
	if rewritten, ok := router.EnableRequestStreamUsage(upstreamBody, config.ProtocolChat, stream); ok {
		upstreamBody = rewritten
	}

	// Pick key from the Ollama pool.
	attempts, err := p.PickAttempts(route.TargetGroup(config.UpstreamOllama))
	if err != nil {
		return attemptResult{
			Status:    http.StatusServiceUnavailable,
			Err:       fmt.Errorf("no available upstream key for group %s: %w", route.TargetGroup(config.UpstreamOllama), err),
			Retryable: true,
		}
	}

	baseURL := config.BaseURLFor(config.UpstreamOllama)
	// Ollama Cloud speaks Chat protocol — always send to /v1/chat/completions.
	target := baseURL + "/v1/chat/completions"

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

			if crossProtocol {
				return proxyOllamaCrossProtocolKey(c, p, key, route, inbound, stream, start,
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
// (Messages/Responses → Chat) Ollama requests. It does NOT call cancel() —
// the caller is responsible for canceling the context.
func proxyOllamaCrossProtocolKey(c *gin.Context, p *pool.Picker, key *store.Key,
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
		errBody, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		markKeyFailure(p, key, resp.StatusCode, errBody)
		return attemptResult{Retryable: true}
	}

	if shouldMarkUpstreamFailure(resp.StatusCode) {
		errBody, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		markKeyFailure(p, key, resp.StatusCode, errBody)
		markAndLog(c, p, key, route, inbound, resp.StatusCode, start, stream, nil, summarizeUpstreamError(resp.StatusCode, errBody))
		return attemptResult{
			Response:  resp,
			Status:   resp.StatusCode,
			Retryable: true,
		}
	}

	// Success — read the response body, then convert.
	responseBody, readErr := io.ReadAll(resp.Body)
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
	rr := proxyCrossProtocolResponse(c, resp, stream, inbound, config.ProtocolChat, p, key, route, start, responseBody)
	if rr.ResponseStarted {
		return attemptResult{Handled: true}
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
		errBody, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		markKeyFailure(p, key, resp.StatusCode, errBody)
		return attemptResult{Retryable: true}
	}

	if shouldMarkUpstreamFailure(resp.StatusCode) {
		errBody, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		markKeyFailure(p, key, resp.StatusCode, errBody)
		markAndLog(c, p, key, route, inbound, resp.StatusCode, start, stream, nil, summarizeUpstreamError(resp.StatusCode, errBody))
		return attemptResult{
			Response:  resp,
			Status:   resp.StatusCode,
			Retryable: true,
		}
	}

	// Success — pre-read response body and delegate to handler.
	responseBody, readErr := io.ReadAll(resp.Body)
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
	rr := proxySameProtocolResponse(c, resp, stream, p, key, route, inbound, start, responseBody)
	if rr.ResponseStarted {
		return attemptResult{Handled: true}
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