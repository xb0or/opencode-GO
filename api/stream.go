package api

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
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
// proxySameProtocolResponse handles the upstream response for same-protocol
// requests. For streaming (SSE) responses, it pre-reads only the first valid
// SSE event to confirm the stream started, then flushes that event and pipes
// the remaining response body directly to the client — without buffering the
// entire response in memory.
//
// body is the pre-read upstream response body when the caller has already
// buffered it (e.g. from an error path). For success paths where streaming
// is possible, body is nil and the function reads from resp.Body directly.
//
// Returns a responseResult so the caller can decide whether to failover.
func proxySameProtocolResponse(c *gin.Context, resp *http.Response, stream bool,
	p *pool.Picker, key *store.Key, route config.ModelRoute, inbound config.Protocol,
	start time.Time, body []byte) responseResult {

	if resp.StatusCode >= 400 {
		// For error responses, body should already be read by the caller.
		if body == nil {
			body, _ = io.ReadAll(io.LimitReader(resp.Body, maxErrorBodyRead))
			_ = resp.Body.Close()
		}
		errMsg := summarizeUpstreamError(resp.StatusCode, body)
		if shouldMarkUpstreamFailure(resp.StatusCode) {
			markKeyFailure(p, key, resp.StatusCode, body)
		}
		markAndLog(c, p, key, route, inbound, resp.StatusCode, start, stream, nil, errMsg)
		retryable := !isClientErrorNonRetryable(resp.StatusCode)
		return responseResult{
			Err:       fmt.Errorf("upstream returned %d", resp.StatusCode),
			Retryable: retryable,
		}
	}

	// Determine if the response is actually SSE by checking the Content-Type.
	isSSE := strings.Contains(resp.Header.Get("Content-Type"), "text/event-stream")

	if !stream && !isSSE {
		// Non-streaming: read the full body (with a size limit) and validate.
		if body == nil {
			body, _ = io.ReadAll(io.LimitReader(resp.Body, maxNonStreamBodyRead))
			_ = resp.Body.Close()
		}
		if !isValidJSONForProtocol(body, inbound) {
			markKeyFailure(p, key, http.StatusBadGateway, body)
			errMsg := fmt.Sprintf("upstream returned non-JSON response (Content-Type: %s)", resp.Header.Get("Content-Type"))
			markAndLog(c, p, key, route, inbound, http.StatusBadGateway, start, false, nil, errMsg)
			return responseResult{
				Err:       fmt.Errorf("invalid response body for protocol %s", inbound),
				Retryable: true,
			}
		}
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

	// Streaming SSE: pre-read the first valid SSE event from the body, then
	// pipe the rest directly to the client.
	if body != nil {
		// Body was already fully read by the caller — use the buffered version.
		return proxySameProtocolSSEBuffered(c, resp, p, key, route, inbound, start, body)
	}
	return proxySameProtocolSSELive(c, resp, p, key, route, inbound, start)
}

// proxySameProtocolSSEBuffered handles SSE responses where the body was already
// fully read into memory by the caller. It validates the first event, then
// writes the entire buffered body to the client.
func proxySameProtocolSSEBuffered(c *gin.Context, resp *http.Response,
	p *pool.Picker, key *store.Key, route config.ModelRoute, inbound config.Protocol,
	start time.Time, body []byte) responseResult {

	_, valid, preReadErr := findFirstValidSSEData(body)
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
	return responseResult{ResponseStarted: true}
}

// proxySameProtocolSSELive handles SSE responses by streaming directly from
// resp.Body to the client. It pre-reads the first valid SSE event (bounded
// by maxSSERead) to confirm the stream started, commits the response headers
// + first event, then pipes the remaining body with io.Copy.
//
// This is the true streaming path: data is flushed to the client as soon as
// it arrives from the upstream, without buffering the full response.
func proxySameProtocolSSELive(c *gin.Context, resp *http.Response,
	p *pool.Picker, key *store.Key, route config.ModelRoute, inbound config.Protocol,
	start time.Time) responseResult {

	// Pre-read the first valid SSE event with a bounded reader.
	bufReader := bufio.NewReader(resp.Body)
	firstEvent, valid, preReadErr := readFirstSSEEvent(bufReader)
	if !valid {
		_ = resp.Body.Close()
		markKeyFailure(p, key, http.StatusBadGateway, nil)
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

	// First valid event found — commit response headers and first event.
	copyResponseHeaders(c, resp)
	c.Writer.WriteHeader(resp.StatusCode)
	// Write the pre-read first event immediately and flush.
	_, writeErr := c.Writer.Write(firstEvent)
	if writeErr != nil {
		_ = resp.Body.Close()
		markAndLog(c, p, key, route, inbound, resp.StatusCode, start, true, nil, "client write error: "+writeErr.Error())
		return responseResult{ResponseStarted: true}
	}
	c.Writer.Flush()

	// Pipe the remaining response body to the client while capturing the
	// tail for usage extraction. We keep the last 64 KB of output so we can
	// find the final usage event without buffering the entire response.
	tail := newTailBuffer(64 << 10)
	mw := io.MultiWriter(c.Writer, tail)
	_, copyErr := io.Copy(mw, bufReader)
	_ = resp.Body.Close()

	// After streaming, we can no longer failover — response is committed.
	usage := usageFromSSEBuffer(inbound, tail.bytes())
	if usage == nil {
		usage = usageFromSSEBuffer(inbound, firstEvent) // best-effort partial
	}
	if copyErr != nil {
		markAndLog(c, p, key, route, inbound, resp.StatusCode, start, true, usage, "stream copy error: "+copyErr.Error())
	} else {
		p.MarkSuccess(key.ID)
		markAndLog(c, p, key, route, inbound, resp.StatusCode, start, true, usage, "")
	}
	return responseResult{ResponseStarted: true}
}

// tailBuffer is a ring buffer that keeps the last N bytes written to it.
// It's used to capture the tail of an SSE stream for usage extraction
// without buffering the entire response in memory.
type tailBuffer struct {
	buf  []byte
	size int
}

func newTailBuffer(size int) *tailBuffer {
	return &tailBuffer{buf: make([]byte, 0, size), size: size}
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.buf = append(t.buf, p...)
	if len(t.buf) > t.size {
		t.buf = t.buf[len(t.buf)-t.size:]
	}
	return len(p), nil
}

func (t *tailBuffer) bytes() []byte {
	return t.buf
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
		// Non-retryable client errors must NOT trigger failover.
		retryable := !isClientErrorNonRetryable(resp.StatusCode)
		return responseResult{
			Err:       fmt.Errorf("upstream returned %d", resp.StatusCode),
			Retryable: retryable,
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

// Size limits for reading upstream response bodies.
const (
	// maxErrorBodyRead limits how much of an error response we read for
	// logging purposes. Error bodies are typically small JSON objects.
	maxErrorBodyRead = 64 << 10 // 64 KiB

	// maxNonStreamBodyRead limits non-streaming success responses.
	maxNonStreamBodyRead = 32 << 20 // 32 MiB

	// maxSSERead limits how much we pre-read to find the first valid SSE
	// event before committing the response. Most first events arrive in
	// the first few KB; 1 MiB is a generous upper bound.
	maxSSERead = 1 << 20 // 1 MiB
)

// readFirstSSEEvent reads from a bufio.Reader until it finds the first valid
// SSE data event (a line starting with "data:" with non-empty, non-[DONE]
// content). It reads at most maxSSERead bytes. Returns the raw bytes of the
// first event (including any preceding lines/blank lines) so the caller can
// write them to the client without data loss.
func readFirstSSEEvent(r *bufio.Reader) ([]byte, bool, error) {
	var buf bytes.Buffer
	for {
		line, err := r.ReadBytes('\n')
		if err != nil && err != io.EOF {
			return nil, false, err
		}
		buf.Write(line)
		trimmed := bytes.TrimSpace(line)
		if bytes.HasPrefix(trimmed, []byte("data:")) {
			data := bytes.TrimSpace(bytes.TrimPrefix(trimmed, []byte("data:")))
			if len(data) > 0 && !bytes.Equal(data, []byte("[DONE]")) {
				// Found the first valid event. Return everything read so far.
				return buf.Bytes(), true, nil
			}
		}
		if err == io.EOF {
			break
		}
		if buf.Len() > maxSSERead {
			return nil, false, fmt.Errorf("no valid SSE event found within %d bytes", maxSSERead)
		}
	}
	return nil, false, nil
}

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