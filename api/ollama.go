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
// proxyOllamaRequest handles an Ollama Cloud-routed request.
// When handled is non-nil, the function sets *handled = true only when it
// writes a successful response (2xx) to the client. On failure (4xx/5xx)
// it marks the key failure and returns without setting *handled, so the
// caller can try the next upstream in a multi-upstream failover.
func proxyOllamaRequest(c *gin.Context, p *pool.Picker, route config.ModelRoute,
	inbound config.Protocol, upstreamBody []byte, head requestHead, start time.Time,
	handled *bool) {

	setHandled := func() {
		if handled != nil {
			*handled = true
		}
	}

	// Rewrite model to the real upstream model id (e.g. gpt-oss:120b).
	rewritten, ok := router.RewriteRequestModel(upstreamBody, route.RealModel)
	if ok {
		upstreamBody = rewritten
	}

	// Cross-protocol: Ollama speaks Chat natively. Any inbound protocol that
	// is not Chat must be converted to Chat before forwarding.
	crossProtocol := inbound != config.ProtocolChat

	if crossProtocol {
		converted, convErr := protocol.ConvertRequest(inbound, config.ProtocolChat, upstreamBody)
		if convErr != nil {
			writeOpenAIError(c, http.StatusBadRequest, "conversion_error",
				fmt.Sprintf("failed to convert request from %s to chat: %v", inbound, convErr))
			setHandled()
			return
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
	attempts, err := p.PickAttempts(route.Group)
	if err != nil {
		writeOpenAIError(c, http.StatusServiceUnavailable, "no_upstream_key_error",
			"no available upstream key for group "+route.Group)
		setHandled()
		return
	}

	baseURL := config.BaseURLFor(config.UpstreamOllama)
	// Ollama Cloud speaks Chat protocol — always send to /v1/chat/completions.
	target := baseURL + "/v1/chat/completions"
	lastErrMsg := ""

	for i := range attempts {
		key := &attempts[i]
		p.MarkUsed(key.ID)

		ctx, cancel := upstreamRequestContext(c.Request.Context(), stream)
		defer cancel()

		req, err := http.NewRequestWithContext(ctx, c.Request.Method, target, bytes.NewReader(upstreamBody))
		if err != nil {
			markAndLog(c, p, key, route, inbound, http.StatusInternalServerError, start, stream, nil, err.Error())
			writeOpenAIError(c, http.StatusInternalServerError, "internal_error", "failed to build upstream request")
			setHandled()
			return
		}
		copyForwardHeaders(req.Header, c.Request.Header)
		setContentLength(req, len(upstreamBody))
		injectUpstreamAuth(req.Header, key.Value)

		upstreamClient := upstream.NewClientForProxy(key.ProxyURL)

		// --------------- Cross-protocol (Messages/Responses → Chat) ---------------
		if crossProtocol {
			resp, doErr := upstreamClient.Do(req)
			if doErr != nil {
				lastErrMsg = "failed to reach upstream: " + doErr.Error()
				if status, errType, message, ok := classifyProxyContextError(doErr); ok {
					if i+1 < len(attempts) && status != statusClientClosedRequest {
						continue
					}
					writeOpenAIError(c, status, errType, message)
					setHandled()
					return
				}
				markKeyFailure(p, key, http.StatusBadGateway, nil)
				if i+1 < len(attempts) {
					continue
				}
				writeOpenAIError(c, http.StatusBadGateway, "upstream_error", genericUpstreamMessage(http.StatusBadGateway))
				setHandled()
				return
			}

			if shouldRetryWithNextKey(resp.StatusCode) && i+1 < len(attempts) {
				errBody, _ := io.ReadAll(resp.Body)
				_ = resp.Body.Close()
				markKeyFailure(p, key, resp.StatusCode, errBody)
				lastErrMsg = summarizeUpstreamError(resp.StatusCode, errBody)
				continue
			}

			// Retryable failure on the last key — don't write a response.
			if shouldMarkUpstreamFailure(resp.StatusCode) {
				errBody, _ := io.ReadAll(resp.Body)
				_ = resp.Body.Close()
				markKeyFailure(p, key, resp.StatusCode, errBody)
				lastErrMsg = summarizeUpstreamError(resp.StatusCode, errBody)
				markAndLog(c, p, key, route, inbound, resp.StatusCode, start, stream, nil, lastErrMsg)
				break
			}

			proxyCrossProtocolResponse(c, resp, stream, inbound, config.ProtocolChat, p, key, route, start)
			setHandled()
			return
		}

		// --------------- Same-protocol (Chat → Chat) ---------------
		resp, doErr := upstreamClient.Do(req)
		if doErr != nil {
			lastErrMsg = "failed to reach upstream: " + doErr.Error()
			if status, errType, message, ok := classifyProxyContextError(doErr); ok {
				if i+1 < len(attempts) && status != statusClientClosedRequest {
					continue
				}
				writeOpenAIError(c, status, errType, message)
				setHandled()
				return
			}
			markKeyFailure(p, key, http.StatusBadGateway, nil)
			if i+1 < len(attempts) {
				continue
			}
			// Last key failed — return without setting handled so caller can retry upstream
			lastErrMsg = "failed to reach upstream"
			markAndLog(c, p, key, route, inbound, http.StatusBadGateway, start, stream, nil, lastErrMsg)
			break
		}

		if shouldRetryWithNextKey(resp.StatusCode) && i+1 < len(attempts) {
			errBody, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			markKeyFailure(p, key, resp.StatusCode, errBody)
			lastErrMsg = summarizeUpstreamError(resp.StatusCode, errBody)
			continue
		}

		// Retryable failure on the last key — don't stream the error response.
		// Let the caller try the next upstream (or return 502 if none left).
		if shouldMarkUpstreamFailure(resp.StatusCode) {
			errBody, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			markKeyFailure(p, key, resp.StatusCode, errBody)
			lastErrMsg = summarizeUpstreamError(resp.StatusCode, errBody)
			markAndLog(c, p, key, route, inbound, resp.StatusCode, start, stream, nil, lastErrMsg)
			break
		}

		// Success — stream the response
		proxySameProtocolResponse(c, resp, stream, p, key, route, inbound, start)
		setHandled()
		return
	}

	if lastErrMsg == "" {
		lastErrMsg = "all upstream keys failed"
	}
	// Don't set handled — let the caller try the next upstream
}