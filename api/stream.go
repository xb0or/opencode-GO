package api

import (
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/xb0or/opencode-GO/config"
	"github.com/xb0or/opencode-GO/pool"
	"github.com/xb0or/opencode-GO/protocol"
	"github.com/xb0or/opencode-GO/store"
)

// ---------------------------------------------------------------------------
// Same-Protocol Response Handling
// ---------------------------------------------------------------------------
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

// ---------------------------------------------------------------------------
// Cross-Protocol Response Handling
// ---------------------------------------------------------------------------
// proxyCrossProtocolResponse buffers the upstream response, converts it through
// the IR, and writes the converted response to the client.
// body is the pre-read upstream response body (already read by the caller).
func proxyCrossProtocolResponse(c *gin.Context, resp *http.Response, stream bool,
	inbound, upstreamProto config.Protocol,
	p *pool.Picker, key *store.Key, route config.ModelRoute, start time.Time, body []byte) {

	if resp.StatusCode >= 400 {
		// Don't convert error responses; return a generic error to hide
		// upstream provider/channel details from the client.
		if shouldMarkUpstreamFailure(resp.StatusCode) {
			markKeyFailure(p, key, resp.StatusCode, body)
		}
		markAndLog(c, p, key, route, inbound, resp.StatusCode, start, stream, nil, summarizeUpstreamError(resp.StatusCode, body))
		writeOpenAIError(c, resp.StatusCode, upstreamErrorType(resp.StatusCode), genericUpstreamMessage(resp.StatusCode))
		return
	}

	if stream {
		// ---- SSE Reader (buffer full upstream stream) ----
		// Decode the upstream stream first so we can report a clean error to
		// the client (without having already committed a 200 status) when the
		// upstream payload is not a valid SSE/JSON stream — e.g. an HTML
		// gateway error page served with HTTP 200.
		streamResp, convErr := protocol.DecodeStreamBuffer(upstreamProto, body)
		if convErr != nil {
			markKeyFailure(p, key, http.StatusBadGateway, body)
			errMsg := fmt.Sprintf("upstream %s stream response could not be decoded: %v; body: %s",
				upstreamProto, convErr, previewBody(body))
			markAndLog(c, p, key, route, inbound, http.StatusBadGateway, start, true, nil, errMsg)
			writeOpenAIError(c, http.StatusBadGateway, "upstream_error",
				"upstream returned a non-streaming response that could not be decoded")
			return
		}

		// ---- SSE Writer (emit converted stream to client) ----
		// Commit the SSE headers only after a successful decode.
		c.Writer.Header().Set("Content-Type", "text/event-stream")
		c.Writer.Header().Set("Cache-Control", "no-cache")
		c.Writer.Header().Set("Connection", "keep-alive")
		c.Writer.WriteHeader(http.StatusOK)
		usage := usageFromSSEBuffer(upstreamProto, body)
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
		// ---- Non-SSE (buffer full response) ----
		converted, err := protocol.ConvertResponse(upstreamProto, inbound, body)
		if err != nil {
			// Upstream returned a body that is not valid JSON for its protocol
			// (commonly an HTML error page from an upstream proxy/CDN). Report
			// the real cause instead of the opaque JSON parse error.
			markKeyFailure(p, key, http.StatusBadGateway, body)
			errMsg := fmt.Sprintf("upstream %s response could not be decoded: %v; body: %s",
				upstreamProto, err, previewBody(body))
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
		usage := usageFromResponse(upstreamProto, body)
		if usage == nil {
			usage = usageFromResponse(inbound, converted)
		}
		markAndLog(c, p, key, route, inbound, resp.StatusCode, start, false, usage, "")
	}
}