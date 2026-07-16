package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httputil"
	"net/url"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/xb0or/opencode-GO/config"
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
func proxyOllamaRequest(c *gin.Context, p *pool.Picker, route config.ModelRoute,
	inbound config.Protocol, upstreamBody []byte, head requestHead, start time.Time) {

	// Rewrite model to the real upstream model id (e.g. ollama-llama3.3 → llama3.3:70b).
	rewritten, ok := rewriteModel(upstreamBody, route.RealModel)
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
	if rewritten, ok := enableStreamUsage(upstreamBody, config.ProtocolChat, stream); ok {
		upstreamBody = rewritten
	}

	// Pick key from the Ollama pool.
	attempts, err := p.PickAttempts(route.Group)
	if err != nil {
		writeOpenAIError(c, http.StatusServiceUnavailable, "no_upstream_key_error",
			"no available upstream key for group "+route.Group)
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
					return
				}
				markKeyFailure(p, key, http.StatusBadGateway, nil)
				if i+1 < len(attempts) {
					continue
				}
				writeOpenAIError(c, http.StatusBadGateway, "upstream_error", genericUpstreamMessage(http.StatusBadGateway))
				return
			}

			if shouldRetryWithNextKey(resp.StatusCode) && i+1 < len(attempts) {
				errBody, _ := io.ReadAll(resp.Body)
				_ = resp.Body.Close()
				markKeyFailure(p, key, resp.StatusCode, errBody)
				lastErrMsg = summarizeUpstreamError(resp.StatusCode, errBody)
				continue
			}

			proxyCrossProtocolResponse(c, resp, stream, inbound, config.ProtocolChat, p, key, route, start)
			return
		}

		// --------------- Same-protocol (Chat → Chat) transparent ReverseProxy ---------------
		targetURL, _ := url.Parse(target)
		rp := &httputil.ReverseProxy{
			Director: func(r *http.Request) {
				r.URL.Scheme = targetURL.Scheme
				r.URL.Host = targetURL.Host
				r.URL.Path = targetURL.Path
				r.Host = targetURL.Host
				r.Body = io.NopCloser(bytes.NewReader(upstreamBody))
				r.ContentLength = int64(len(upstreamBody))
				copyForwardHeaders(r.Header, c.Request.Header)
				injectUpstreamAuth(r.Header, key.Value)
			},
			Transport: upstreamClient.Transport,
			ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
				if status, errType, message, ok := classifyProxyContextError(err); ok {
					writeOpenAIError(c, status, errType, message)
					return
				}
				writeOpenAIError(c, http.StatusBadGateway, "upstream_error", genericUpstreamMessage(http.StatusBadGateway))
			},
		}

		// Wrap the response writer to capture the status code for bookkeeping.
		lw := &statusCaptureWriter{ResponseWriter: c.Writer, code: http.StatusOK}
		rp.ServeHTTP(lw, c.Request)

		code := lw.code

		if code < 400 {
			p.MarkSuccess(key.ID)
		} else if shouldMarkUpstreamFailure(code) {
			markKeyFailure(p, key, code, nil)
		}
		markAndLog(c, p, key, route, inbound, code, start, stream, nil, lastErrMsg)
		return
	}

	if lastErrMsg == "" {
		lastErrMsg = "all upstream keys failed"
	}
	writeOpenAIError(c, http.StatusBadGateway, "upstream_error", genericUpstreamMessage(http.StatusBadGateway))
}

// statusCaptureWriter wraps gin.ResponseWriter to capture the WriteHeader
// status code set by httputil.ReverseProxy.
type statusCaptureWriter struct {
	gin.ResponseWriter
	code int
	once sync.Once
}

func (w *statusCaptureWriter) WriteHeader(code int) {
	w.once.Do(func() { w.code = code })
	w.ResponseWriter.WriteHeader(code)
}