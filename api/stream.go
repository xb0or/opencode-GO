package api

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
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

	_, valid, preReadErr := findFirstValidContentSSEData(body, inbound)
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

	// P0-2 (round-8): For Responses protocol, parse the terminal state of
	// the entire buffered body BEFORE writing anything to the client. If
	// the stream contains response.failed or lacks a valid terminal, the
	// response has not been committed yet — failover is still possible.
	if inbound == config.ProtocolResponses {
		tracker := &responsesSSETracker{}
		_, _ = tracker.Write(body)
		terminalErr := tracker.Finalize()
		if terminalErr != nil {
			markKeyFailure(p, key, http.StatusBadGateway, body)
			markAndLog(c, p, key, route, inbound, http.StatusBadGateway, start, true,
				nil, "responses stream terminal check failed: "+terminalErr.Error())
			return responseResult{
				Err:       terminalErr,
				Retryable: true,
			}
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
	// Size the bufio.Reader to maxSSERead so ReadSlice inside
	// readFirstSSEEvent can return single lines up to the limit without
	// hitting ErrBufferFull prematurely. Bytes read ahead but not returned
	// by readFirstSSEEvent stay in this bufReader for the subsequent
	// io.Copy — no data is lost.
	bufReader := bufio.NewReaderSize(resp.Body, maxSSERead)
	firstEvent, valid, preReadErr := readFirstContentSSEEvent(bufReader, inbound)
	if !valid {
		_ = resp.Body.Close()
		markKeyFailure(p, key, http.StatusBadGateway, nil)
		errMsg := "upstream returned 200 but no valid SSE data before connection close"
		if preReadErr != nil {
			errMsg = preReadErr.Error()
		}
		markAndLog(c, p, key, route, inbound, http.StatusBadGateway, start, true, nil, errMsg)
		return responseResult{
			Err:       fmt.Errorf("no valid SSE event received: %s", errMsg),
			Retryable: true,
		}
	}

	// First valid event found — commit response headers and first event.
	copyResponseHeaders(c, resp)
	c.Writer.WriteHeader(resp.StatusCode)
	// Write the pre-read first event immediately and flush. Because
	// readFirstSSEEvent now returns the complete event (including the
	// terminating blank line), the client can dispatch it immediately.
	_, writeErr := c.Writer.Write(firstEvent)
	if writeErr != nil {
		_ = resp.Body.Close()
		markAndLog(c, p, key, route, inbound, resp.StatusCode, start, true, nil, "client write error: "+writeErr.Error())
		return responseResult{ResponseStarted: true}
	}
	c.Writer.Flush()

	// P0-2 (round-8): For Responses protocol, track the SSE terminal state
	// as we forward the stream. Only response.completed/incomplete permits
	// MarkSuccess. A response.failed (even after content was committed)
	// must mark the key as failed.
	var tracker *responsesSSETracker
	if inbound == config.ProtocolResponses {
		tracker = &responsesSSETracker{}
		// Feed the already-pre-read first event to the tracker.
		_, _ = tracker.Write(firstEvent)
	}

	// Pipe the remaining response body to the client while capturing the
	// tail for usage extraction. We keep the last 64 KB of output so we can
	// find the final usage event without buffering the entire response.
	//
	// The writer is wrapped in a flushingWriter so every Write is
	// immediately flushed to the underlying HTTP connection. Without this,
	// gin.ResponseWriter.Write buffers small writes and SSE events would
	// only depart when the buffer fills or the stream ends — defeating
	// real-time streaming for small events.
	tail := newTailBuffer(64 << 10)
	fw := &flushingWriter{ResponseWriter: c.Writer}
	var writers []io.Writer
	if tracker != nil {
		// Tracker goes FIRST so it sees the chunk even if the client write
		// fails (ensuring we record a failure terminal in the same chunk).
		writers = []io.Writer{tracker, fw, tail}
	} else {
		writers = []io.Writer{fw, tail}
	}
	mw := io.MultiWriter(writers...)
	_, copyErr := io.Copy(mw, bufReader)
	_ = resp.Body.Close()

	// After streaming, we can no longer failover — response is committed.
	usage := usageFromSSEBuffer(inbound, tail.bytes())
	if usage == nil {
		usage = usageFromSSEBuffer(inbound, firstEvent) // best-effort partial
	}

	// P0-2 (round-8): evaluate terminal state BEFORE deciding MarkSuccess.
	if tracker != nil {
		terminalErr := tracker.Finalize()
		switch {
		case tracker.terminal == responsesTerminalFailed:
			// Upstream reported failure — mark key as failed, do NOT MarkSuccess.
			markKeyFailure(p, key, http.StatusBadGateway, nil)
			markAndLog(c, p, key, route, inbound, resp.StatusCode, start, true,
				usage, "upstream responses stream failed after commit")
			return responseResult{ResponseStarted: true}
		case copyErr != nil:
			// Client write error (e.g. disconnect) — don't punish the key.
			markAndLog(c, p, key, route, inbound, resp.StatusCode, start, true,
				usage, "stream copy error: "+copyErr.Error())
			return responseResult{ResponseStarted: true}
		case terminalErr != nil:
			// No valid terminal event (ErrUnexpectedEOF or parse error).
			markKeyFailure(p, key, http.StatusBadGateway, nil)
			markAndLog(c, p, key, route, inbound, resp.StatusCode, start, true,
				usage, "invalid responses stream terminal: "+terminalErr.Error())
			return responseResult{ResponseStarted: true}
		default:
			// response.completed or response.incomplete — healthy terminal.
			p.MarkSuccess(key.ID)
			markAndLog(c, p, key, route, inbound, resp.StatusCode, start, true, usage, "")
			return responseResult{ResponseStarted: true}
		}
	}

	// Non-Responses protocols: original logic.
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
		// Incremental cross-protocol streaming — see proxyCrossProtocolStream.
		// This branch is reached only when the caller could NOT pass an open
		// resp.Body (e.g. body was pre-read for error handling). We fall back
		// to the incremental converter fed from the pre-read buffer.
		firstEvent, rest := splitFirstSSEEvent(body)
		if len(firstEvent) == 0 {
			markKeyFailure(p, key, http.StatusBadGateway, body)
			errMsg := "upstream cross-protocol stream produced no SSE data"
			markAndLog(c, p, key, route, inbound, http.StatusBadGateway, start, true, nil, errMsg)
			return responseResult{
				Err:       fmt.Errorf("%s", errMsg),
				Retryable: true,
			}
		}
		// P0-2: do NOT commit HTTP 200 yet. Run the incremental converter with a
		// staging buffer and an onFirstEvent hook (see proxyCrossProtocolStream
		// for the full rationale).
		committed := false
		var staging bytes.Buffer
		sw := &switchWriter{staging: &staging}
		onFirstEvent := func() error {
			c.Writer.Header().Set("Content-Type", "text/event-stream")
			c.Writer.Header().Set("Cache-Control", "no-cache")
			c.Writer.Header().Set("Connection", "keep-alive")
			c.Writer.WriteHeader(http.StatusOK)
			if staging.Len() > 0 {
				if _, werr := c.Writer.Write(staging.Bytes()); werr != nil {
					return werr
				}
				c.Writer.Flush()
			}
			sw.real = newFlushingWriter(c.Writer)
			committed = true
			return nil
		}
		var restReader io.Reader
		if rest != nil {
			restReader = bytes.NewReader(rest)
		}
		acc, convErr := protocol.StreamConvertIncremental(
			string(upstreamProto), string(inbound),
			firstEvent, restReader, sw, nil, onFirstEvent,
		)
		if convErr != nil {
			if !committed {
				markKeyFailure(p, key, http.StatusBadGateway, nil)
				markAndLog(c, p, key, route, inbound, http.StatusBadGateway, start, true,
					usageFromIRUsage(acc), "stream decode failed: "+convErr.Error())
				return responseResult{
					Err:       fmt.Errorf("stream decode failed: %w", convErr),
					Retryable: true,
				}
			}
			markKeyFailure(p, key, http.StatusBadGateway, nil)
			markAndLog(c, p, key, route, inbound, http.StatusOK, start, true,
				usageFromIRUsage(acc), "stream convert error: "+convErr.Error())
			return responseResult{ResponseStarted: true}
		}
		if !committed {
			markKeyFailure(p, key, http.StatusBadGateway, nil)
			markAndLog(c, p, key, route, inbound, http.StatusBadGateway, start, true,
				nil, "cross-protocol stream produced no valid target events")
			return responseResult{
				Err:       fmt.Errorf("stream produced no valid target events"),
				Retryable: true,
			}
		}
		p.MarkSuccess(key.ID)
		markAndLog(c, p, key, route, inbound, http.StatusOK, start, true,
			usageFromIRUsage(acc), "")
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

// readFirstSSEEvent reads from a bufio.Reader until it finds AND completes the
// first valid SSE data event. An SSE event is terminated by a blank line
// (an empty line or a line containing only whitespace) per the SSE spec.
// Returning only the data: line without the terminating blank line would
// cause the client to wait for more data before dispatching the event.
//
// The caller MUST size the bufio.Reader to at least maxSSERead (via
// bufio.NewReaderSize) so ReadSlice can return lines up to the limit. Bytes
// read ahead but not returned remain in the caller's bufio.Reader and are
// available for subsequent io.Copy — readFirstSSEEvent does NOT wrap the
// reader in its own buffer, so no data is lost.
//
// It enforces a hard cap of maxSSERead bytes total: a malicious upstream
// that sends a huge line with no newline cannot exhaust memory. Returns the
// raw bytes of the first event (including the terminating blank line) so
// the caller can write them to the client without data loss.
func readFirstSSEEvent(r *bufio.Reader) ([]byte, bool, error) {
	var buf bytes.Buffer
	foundData := false
	for {
		line, err := r.ReadSlice('\n')
		if err != nil && err != bufio.ErrBufferFull && err != io.EOF {
			return nil, false, err
		}
		// Enforce the hard cap regardless of how the line was read.
		if buf.Len()+len(line) > maxSSERead {
			return nil, false, fmt.Errorf("no valid SSE event found within %d bytes", maxSSERead)
		}
		buf.Write(line)
		trimmed := bytes.TrimSpace(line)

		if !foundData {
			if bytes.HasPrefix(trimmed, []byte("data:")) {
				data := bytes.TrimSpace(bytes.TrimPrefix(trimmed, []byte("data:")))
				if len(data) > 0 && !bytes.Equal(data, []byte("[DONE]")) {
					foundData = true
				}
			}
		}

		if foundData && len(trimmed) == 0 {
			// Blank line terminates the event — return everything including it.
			return buf.Bytes(), true, nil
		}

		if err == io.EOF {
			// Stream ended. If we found a data line but no terminating blank
			// line, return what we have so the client isn't stalled.
			if foundData {
				return buf.Bytes(), true, nil
			}
			break
		}
		if err == bufio.ErrBufferFull {
			// A single line exceeded maxSSERead — bail out.
			return nil, false, fmt.Errorf("no valid SSE event found within %d bytes", maxSSERead)
		}
	}
	return nil, false, nil
}

// readFirstContentSSEEvent wraps readFirstSSEEvent with Responses-protocol
// awareness. For Responses streams, pure lifecycle events
// (response.created/response.in_progress/response.queued) do NOT count as
// "valid first events" — they carry no content and must not trigger HTTP 200
// commit. We keep reading until we find either a content event or a terminal
// failure event (response.failed/response.incomplete). If a failure terminal
// arrives before any content, valid=false and the caller can failover.
//
// For non-Responses protocols, behaviour is identical to readFirstSSEEvent.
func readFirstContentSSEEvent(r *bufio.Reader, inbound config.Protocol) ([]byte, bool, error) {
	if inbound != config.ProtocolResponses {
		return readFirstSSEEvent(r)
	}
	// Responses: accumulate lifecycle events but keep scanning for content.
	var accumulated bytes.Buffer
	for {
		event, valid, err := readFirstSSEEvent(r)
		if err != nil {
			return nil, false, err
		}
		if !valid {
			return nil, false, nil
		}
		// Parse the event type from the data: line.
		etype := sseEventType(event)
		switch etype {
		case "response.failed", "response.incomplete":
			// Failure terminal before any content — do NOT commit 200.
			return nil, false, fmt.Errorf("upstream responses stream failed: %s", etype)
		case "response.created", "response.in_progress", "response.queued":
			// Pure lifecycle event — keep scanning.
			accumulated.Write(event)
			continue
		default:
			// Content event (or any other type) — this is a valid commit
			// point. Return accumulated lifecycle events + this event.
			accumulated.Write(event)
			return accumulated.Bytes(), true, nil
		}
	}
}

// sseEventType extracts the "type" field from an SSE event's data: line.
// Returns "" if the event has no type field or is not JSON.
func sseEventType(event []byte) string {
	lines := bytes.Split(event, []byte("\n"))
	for _, line := range lines {
		trimmed := bytes.TrimSpace(line)
		if !bytes.HasPrefix(trimmed, []byte("data:")) {
			continue
		}
		payload := bytes.TrimSpace(bytes.TrimPrefix(trimmed, []byte("data:")))
		if len(payload) == 0 || bytes.Equal(payload, []byte("[DONE]")) {
			continue
		}
		// Parse JSON to extract type.
		var v struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(payload, &v) == nil {
			return v.Type
		}
	}
	return ""
}

// findFirstValidContentSSEData wraps findFirstValidSSEData with Responses-
// protocol awareness, mirroring readFirstContentSSEEvent for the buffered
// path. For Responses streams, if the first valid data event is a pure
// lifecycle event (response.created/etc.) and the stream ends without any
// content event or with a failure terminal, valid=false.
func findFirstValidContentSSEData(body []byte, inbound config.Protocol) (int, bool, error) {
	if inbound != config.ProtocolResponses {
		return findFirstValidSSEData(body)
	}
	// Responses: scan all data: lines for a content event or failure terminal.
	lines := strings.Split(string(body), "\n")
	offset := 0
	hasContent := false
	hasFailure := false
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "data:") {
			offset += len(line) + 1
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(trimmed, "data:"))
		if len(payload) == 0 || payload == "[DONE]" {
			offset += len(line) + 1
			continue
		}
		var v struct {
			Type string `json:"type"`
		}
		if json.Unmarshal([]byte(payload), &v) == nil {
			switch v.Type {
			case "response.failed", "response.incomplete":
				hasFailure = true
			case "response.created", "response.in_progress", "response.queued":
				// lifecycle — skip
			default:
				hasContent = true
			}
		} else {
			// Non-JSON data — treat as content.
			hasContent = true
		}
		offset += len(line) + 1
	}
	if hasFailure && !hasContent {
		return 0, false, fmt.Errorf("upstream responses stream failed: no content before terminal")
	}
	if !hasContent {
		return 0, false, nil
	}
	// Has content — delegate to the original to find the offset.
	return findFirstValidSSEData(body)
}

// errClientWriteAfterCommit is returned by onFirstEvent when HTTP 200 has
// been committed (WriteHeader) but writing the staging buffer to the client
// failed (e.g. client disconnect). It is a client-side problem: the caller
// must NOT markKeyFailure or failover, only return ResponseStarted.
var errClientWriteAfterCommit = errors.New("client write failed after HTTP headers committed")

// switchWriter writes to a staging buffer until sw.real is set, then writes
// to real. This supports P0-2: the cross-protocol converter writes its first
// events to staging; once onFirstEvent commits HTTP 200, sw.real is set and
// subsequent writes go directly to the client.
type switchWriter struct {
	staging *bytes.Buffer
	real    io.Writer
}

func (sw *switchWriter) Write(p []byte) (int, error) {
	if sw.real != nil {
		return sw.real.Write(p)
	}
	return sw.staging.Write(p)
}

// flushingWriter wraps a gin.ResponseWriter and flushes after every Write so
// SSE events are pushed to the client immediately instead of accumulating
// in the HTTP buffer. This is critical for real-time streaming: without
// per-write flushing, small SSE events batch up and only depart when the
// buffer fills or the stream ends. gin.ResponseWriter implements Flush().
type flushingWriter struct {
	gin.ResponseWriter
}

func newFlushingWriter(w gin.ResponseWriter) *flushingWriter {
	return &flushingWriter{ResponseWriter: w}
}

func (fw *flushingWriter) Write(p []byte) (int, error) {
	n, err := fw.ResponseWriter.Write(p)
	if n > 0 {
		fw.ResponseWriter.Flush()
	}
	return n, err
}

// Flush triggers an explicit flush of the underlying response writer.
func (fw *flushingWriter) Flush() {
	fw.ResponseWriter.Flush()
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

// ---------------------------------------------------------------------------
// Incremental cross-protocol streaming (open resp.Body path)
// ---------------------------------------------------------------------------

// proxyCrossProtocolStream handles cross-protocol SSE streaming WITHOUT
// buffering the full upstream response. It pre-reads only the first SSE event
// (for validation / failover), commits headers, then runs the incremental
// Decoder → IR → Encoder → Flush pipeline over the remaining resp.Body.
//
// resp.Body is closed by this function.
func proxyCrossProtocolStream(c *gin.Context, resp *http.Response,
	inbound, upstreamProto config.Protocol,
	p *pool.Picker, key *store.Key, route config.ModelRoute, start time.Time) responseResult {

	bufReader := bufio.NewReaderSize(resp.Body, maxSSERead)
	firstEvent, found, readErr := readFirstSSEEvent(bufReader)
	if readErr != nil || !found {
		errBody, _ := io.ReadAll(io.LimitReader(bufReader, maxErrorBodyRead))
		_ = resp.Body.Close()
		markKeyFailure(p, key, http.StatusBadGateway, errBody)
		errMsg := fmt.Sprintf("upstream cross-protocol stream could not be read: %v; body: %s",
			readErr, previewBody(errBody))
		markAndLog(c, p, key, route, inbound, http.StatusBadGateway, start, true, nil, errMsg)
		return responseResult{
			Err:       fmt.Errorf("stream pre-read failed: %w", readErr),
			Retryable: true,
		}
	}
	if len(firstEvent) == 0 {
		_ = resp.Body.Close()
		markKeyFailure(p, key, http.StatusBadGateway, nil)
		errMsg := "upstream cross-protocol stream produced no SSE data"
		markAndLog(c, p, key, route, inbound, http.StatusBadGateway, start, true, nil, errMsg)
		return responseResult{
			Err:       fmt.Errorf("%s", errMsg),
			Retryable: true,
		}
	}

	// P0-2: do NOT commit HTTP 200 yet. Run the incremental converter with a
	// staging buffer and an onFirstEvent hook. The hook fires after the first
	// target event is successfully emitted to staging — only then do we commit
	// HTTP 200 + SSE headers and flush the staging buffer to the client. If the
	// upstream stream cannot be decoded into at least one valid target event,
	// onFirstEvent is never called, conversion aborts, and we can failover.
	committed := false
	var staging bytes.Buffer
	// The converter writes to a switchable writer: staging first, then the
	// real client writer after onFirstEvent commits.
	sw := &switchWriter{staging: &staging}

	onFirstEvent := func() error {
		// P0-3: mark committed IMMEDIATELY after WriteHeader so the caller
		// never enters the failover branch after headers are sent, even
		// if the subsequent staging-buffer write fails (e.g. client
		// disconnect). A failed write after commit is a client-side
		// problem, not an upstream problem, so it must NOT trigger
		// markKeyFailure or failover.
		c.Writer.Header().Set("Content-Type", "text/event-stream")
		c.Writer.Header().Set("Cache-Control", "no-cache")
		c.Writer.Header().Set("Connection", "keep-alive")
		c.Writer.WriteHeader(http.StatusOK)
		committed = true
		// Flush the staging buffer to the real client.
		if staging.Len() > 0 {
			_, werr := c.Writer.Write(staging.Bytes())
			if werr != nil {
				// Return a sentinel error so the caller knows the
				// response was committed but the client is gone.
				return errClientWriteAfterCommit
			}
			c.Writer.Flush()
		}
		// Switch subsequent writes to the real writer (with per-write flush).
		sw.real = newFlushingWriter(c.Writer)
		return nil
	}

	acc, convErr := protocol.StreamConvertIncremental(
		string(upstreamProto), string(inbound),
		firstEvent, bufReader, sw, nil, onFirstEvent,
	)
	_ = resp.Body.Close()
	if convErr != nil {
		if !committed {
			// P0-2/P0-3: conversion failed before any valid target event was
			// emitted. The client has received nothing — failover is still
			// possible. Mark the key as failed (P0-3 requirement).
			markKeyFailure(p, key, http.StatusBadGateway, nil)
			errMsg := "cross-protocol stream decode failed: " + convErr.Error()
			markAndLog(c, p, key, route, inbound, http.StatusBadGateway, start, true,
				usageFromIRUsage(acc), errMsg)
			return responseResult{
				Err:       fmt.Errorf("stream decode failed: %w", convErr),
				Retryable: true,
			}
		}
		// P0-3: conversion failed after commit. The client already has a
		// partial 200 SSE response. Distinguish:
		//   - client write failure (errClientWriteAfterCommit): the upstream
		//     is healthy, do NOT markKeyFailure or failover;
		//   - upstream decoder error: mark the key as failed — the client
		//     must NOT receive success terminal events (handled by onError).
		if errors.Is(convErr, errClientWriteAfterCommit) {
			markAndLog(c, p, key, route, inbound, http.StatusOK, start, true,
				usageFromIRUsage(acc), "client write failed after commit: "+convErr.Error())
			return responseResult{ResponseStarted: true}
		}
		markKeyFailure(p, key, http.StatusBadGateway, nil)
		markAndLog(c, p, key, route, inbound, http.StatusOK, start, true,
			usageFromIRUsage(acc), "stream convert error: "+convErr.Error())
		return responseResult{ResponseStarted: true}
	}
	if !committed {
		// No error but also no valid target event — treat as a failed stream.
		markKeyFailure(p, key, http.StatusBadGateway, nil)
		markAndLog(c, p, key, route, inbound, http.StatusBadGateway, start, true,
			nil, "cross-protocol stream produced no valid target events")
		return responseResult{
			Err:       fmt.Errorf("stream produced no valid target events"),
			Retryable: true,
		}
	}
	p.MarkSuccess(key.ID)
	markAndLog(c, p, key, route, inbound, http.StatusOK, start, true,
		usageFromIRUsage(acc), "")
	return responseResult{ResponseStarted: true}
}

// splitFirstSSEEvent splits a pre-read SSE buffer into the first complete
// SSE event (including its terminating blank line) and the remaining bytes.
// If no complete event is found, returns (nil, body).
func splitFirstSSEEvent(body []byte) (firstEvent, rest []byte) {
	// An SSE event is terminated by a blank line (\n\n or \r\n\r\n).
	idx := bytes.Index(body, []byte("\n\n"))
	if idx >= 0 {
		return body[:idx+2], body[idx+2:]
	}
	idx = bytes.Index(body, []byte("\r\n\r\n"))
	if idx >= 0 {
		return body[:idx+4], body[idx+4:]
	}
	// No terminating blank line — treat the whole buffer as one event if it
	// contains a data: line.
	if bytes.Contains(body, []byte("data:")) {
		return body, nil
	}
	return nil, body
}