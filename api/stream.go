package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/xb0or/opencode-GO/config"
	"github.com/xb0or/opencode-GO/pool"
	"github.com/xb0or/opencode-GO/protocol"
	"github.com/xb0or/opencode-GO/store"
)

// responseResult is the structured return value from response handling
// functions. When Err is set and ResponseStarted is false, the caller
// may try the next upstream. When ResponseStarted is true, the response
// has been partially or fully committed to the client and no failover
// is possible.
type responseResult struct {
	Err             error
	Retryable       bool
	ResponseStarted bool
}

// ---------------------------------------------------------------------------
// Same-Protocol Response Handling
// ---------------------------------------------------------------------------
// proxySameProtocolResponse streams the upstream response verbatim to the
// client. body is the pre-read upstream response body (caller already read it).
// Returns a responseResult so the caller can decide whether to failover.
func proxySameProtocolResponse(c *gin.Context, resp *http.Response, stream bool,
	p *pool.Picker, key *store.Key, route config.ModelRoute, inbound config.Protocol,
	start time.Time, body []byte) responseResult {

	if resp.StatusCode >= 400 {
		errMsg := summarizeUpstreamError(resp.StatusCode, body)
		if shouldMarkUpstreamFailure(resp.StatusCode) {
			markKeyFailure(p, key, resp.StatusCode, body)
		}
		markAndLog(c, p, key, route, inbound, resp.StatusCode, start, stream, nil, errMsg)
		// Return error without writing — caller decides failover.
		return responseResult{
			Err:       fmt.Errorf("upstream returned %d", resp.StatusCode),
			Retryable: true,
		}
	}

	// Determine if the response is actually SSE by checking the Content-Type.
	// The request's stream flag may be wrong (e.g. invalid JSON body that
	// couldn't be parsed), but the upstream's Content-Type is authoritative.
	isSSE := strings.Contains(resp.Header.Get("Content-Type"), "text/event-stream")

	if !stream && !isSSE {
		// Non-streaming: validate the body is valid JSON for the protocol.
		if !isValidJSONForProtocol(body, inbound) {
			markKeyFailure(p, key, http.StatusBadGateway, body)
			errMsg := fmt.Sprintf("upstream returned non-JSON response (Content-Type: %s)", resp.Header.Get("Content-Type"))
			markAndLog(c, p, key, route, inbound, http.StatusBadGateway, start, false, nil, errMsg)
			return responseResult{
				Err:       fmt.Errorf("invalid response body for protocol %s", inbound),
				Retryable: true,
			}
		}
		// Valid — write to client.
		copyResponseHeaders(c, resp)
		c.Writer.WriteHeader(resp.StatusCode)
		_, writeErr := c.Writer.Write(body)
		usage := usageFromResponse(inbound, body)
		errMsg := copyErrString(writeErr)
		if resp.StatusCode < 400 && writeErr == nil {
			p.MarkSuccess(key.ID)
			markAndLog(c, p, key, route, inbound, resp.StatusCode, start, false, usage, "")
		} else {
			markAndLog(c, p, key, route, inbound, resp.StatusCode, start, false, usage, errMsg)
		}
		return responseResult{ResponseStarted: true}
	}

	// Streaming: pre-read the first valid SSE event before committing.
	// The body is already fully read into memory. We need to find the first
	// valid data: line to confirm the stream has started.
	firstEventBytes, valid, preReadErr := findFirstValidSSEData(body)
	if !valid {
		markKeyFailure(p, key, http.StatusBadGateway, body)
		errMsg := "upstream returned 200 but no valid SSE data before connection close"
		if preReadErr != nil {
			errMsg = preReadErr.Error()
		}
		markAndLog(c, p, key, route, inbound, http.StatusBadGateway, start, true, nil, errMsg)
		return responseResult{
			Err:       fmt.Errorf("no valid SSE event received"),
			Retryable: true,
		}
	}

	// First valid event found — commit to client and stream the full body.
	copyResponseHeaders(c, resp)
	c.Writer.WriteHeader(resp.StatusCode)
	_, writeErr := c.Writer.Write(body)
	usage := usageFromSSEBuffer(inbound, body)
	if usage == nil {
		usage = usageFromResponse(inbound, body)
	}
	if resp.StatusCode < 400 && writeErr == nil {
		p.MarkSuccess(key.ID)
		markAndLog(c, p, key, route, inbound, resp.StatusCode, start, true, usage, "")
	} else {
		markAndLog(c, p, key, route, inbound, resp.StatusCode, start, true, usage, copyErrString(writeErr))
	}
	_ = firstEventBytes
	return responseResult{ResponseStarted: true}
}

// ---------------------------------------------------------------------------
// Cross-Protocol Response Handling
// ---------------------------------------------------------------------------
// proxyCrossProtocolResponse converts the upstream response through the IR
// and writes the converted response to the client.
// body is the pre-read upstream response body.
func proxyCrossProtocolResponse(c *gin.Context, resp *http.Response, stream bool,
	inbound, upstreamProto config.Protocol,
	p *pool.Picker, key *store.Key, route config.ModelRoute, start time.Time, body []byte) responseResult {

	if resp.StatusCode >= 400 {
		if shouldMarkUpstreamFailure(resp.StatusCode) {
			markKeyFailure(p, key, resp.StatusCode, body)
		}
		markAndLog(c, p, key, route, inbound, resp.StatusCode, start, stream, nil,
			summarizeUpstreamError(resp.StatusCode, body))
		return responseResult{
			Err:       fmt.Errorf("upstream returned %d", resp.StatusCode),
			Retryable: true,
		}
	}

	if stream {
		// Decode the upstream stream. If it fails, return error without
		// committing to the client — caller can try next upstream.
		streamResp, convErr := protocol.DecodeStreamBuffer(upstreamProto, body)
		if convErr != nil {
			markKeyFailure(p, key, http.StatusBadGateway, body)
			errMsg := fmt.Sprintf("upstream %s stream response could not be decoded: %v; body: %s",
				upstreamProto, convErr, previewBody(body))
			markAndLog(c, p, key, route, inbound, http.StatusBadGateway, start, true, nil, errMsg)
			return responseResult{
				Err:       fmt.Errorf("stream decode failed: %w", convErr),
				Retryable: true,
			}
		}

		// Valid — commit SSE headers and emit converted stream.
		c.Writer.Header().Set("Content-Type", "text/event-stream")
		c.Writer.Header().Set("Cache-Control", "no-cache")
		c.Writer.Header().Set("Connection", "keep-alive")
		c.Writer.WriteHeader(http.StatusOK)
		usage := usageFromSSEBuffer(upstreamProto, body)
		if usage == nil {
			usage = usageFromIRUsage(streamResp)
		}
		if emitErr := protocol.EmitStreamResponse(c.Writer, inbound, streamResp); emitErr != nil {
			markAndLog(c, p, key, route, inbound, http.StatusOK, start, true, usage,
				"stream emit error: "+emitErr.Error())
			return responseResult{ResponseStarted: true}
		}
		p.MarkSuccess(key.ID)
		markAndLog(c, p, key, route, inbound, http.StatusOK, start, true, usage, "")
		return responseResult{ResponseStarted: true}
	}

	// Non-streaming: convert the response.
	converted, err := protocol.ConvertResponse(upstreamProto, inbound, body)
	if err != nil {
		markKeyFailure(p, key, http.StatusBadGateway, body)
		errMsg := fmt.Sprintf("upstream %s response could not be decoded: %v; body: %s",
			upstreamProto, err, previewBody(body))
		markAndLog(c, p, key, route, inbound, http.StatusBadGateway, start, false, nil, errMsg)
		return responseResult{
			Err:       fmt.Errorf("response conversion failed: %w", err),
			Retryable: true,
		}
	}

	// Valid — write converted response to client.
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
	return responseResult{ResponseStarted: true}
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// isValidJSONForProtocol checks whether the body is valid JSON suitable
// for the given protocol. For Chat/Messages, it verifies the body can be
// unmarshaled as a JSON object. This catches Cloudflare HTML error pages
// and other non-JSON responses served with HTTP 200.
func isValidJSONForProtocol(body []byte, proto config.Protocol) bool {
	if len(body) == 0 {
		return false
	}
	// Quick check: JSON objects start with '{', arrays with '['
	trimmed := strings.TrimSpace(string(body))
	if len(trimmed) == 0 {
		return false
	}
	if trimmed[0] != '{' && trimmed[0] != '[' {
		return false
	}
	// Verify it's valid JSON by attempting to unmarshal into a generic map.
	var v interface{}
	if err := jsonUnmarshal(body, &v); err != nil {
		return false
	}
	return true
}

// jsonUnmarshal is a thin wrapper around json.Unmarshal.
func jsonUnmarshal(data []byte, v interface{}) error {
	return json.Unmarshal(data, v)
}

// findFirstValidSSEData scans the body for the first valid SSE data line
// (a line starting with "data: " with non-empty, non-[DONE] content).
// Returns the byte offset where the data line starts, whether a valid
// event was found, and any error.
func findFirstValidSSEData(body []byte) (int, bool, error) {
	lines := strings.Split(string(body), "\n")
	offset := 0
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "data:") {
			data := strings.TrimSpace(strings.TrimPrefix(trimmed, "data:"))
			if data != "" && data != "[DONE]" {
				return offset, true, nil
			}
		}
		offset += len(line) + 1 // +1 for the \n
	}
	return 0, false, nil
}