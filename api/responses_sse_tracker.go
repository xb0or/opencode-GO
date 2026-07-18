package api

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/xb0or/opencode-GO/protocol"
)

// responsesTerminalState tracks the final state of a Responses SSE stream.
type responsesTerminalState uint8

const (
	responsesTerminalNone       responsesTerminalState = iota
	responsesTerminalCompleted                          // response.completed
	responsesTerminalIncomplete                          // response.incomplete
	responsesTerminalFailed                              // response.failed (highest priority)
)

// responsesSSETracker observes SSE data as it is forwarded to the client and
// records the terminal state of a Responses API stream. It is used by the
// same-protocol SSE path to decide whether MarkSuccess is safe: only
// response.completed or response.incomplete (without a prior response.failed)
// permit MarkSuccess.
//
// The tracker NEVER blocks or interrupts the client write — it is a passive
// observer. Call Finalize() after the stream copy is done to get the verdict.
type responsesSSETracker struct {
	pending  []byte // incomplete line bytes carried across Write calls
	terminal responsesTerminalState
	parseErr error
}

// Write feeds a chunk of SSE data to the tracker. It processes complete lines
// (terminated by \n) and buffers partial lines for the next call. The tracker
// never returns an error from Write — it must not interrupt the client stream.
func (t *responsesSSETracker) Write(p []byte) (int, error) {
	t.feed(p)
	return len(p), nil
}

// feed processes a chunk, splitting on newlines and parsing complete data:
// lines. Partial lines are retained in t.pending for the next call.
func (t *responsesSSETracker) feed(p []byte) {
	// Combine pending + new chunk.
	data := append(t.pending, p...)
	t.pending = nil

	for {
		idx := strings.IndexByte(string(data), '\n')
		if idx < 0 {
			// No complete line — buffer for next call.
			// Cap pending at maxSSERead to prevent unbounded memory growth
			// from a malicious upstream sending huge lines without newlines.
			if len(data) > maxSSERead {
				t.parseErr = fmt.Errorf("responses tracker: line exceeded %d bytes", maxSSERead)
				t.pending = data[:maxSSERead] // keep capped
				return
			}
			t.pending = data
			return
		}

		line := data[:idx]
		data = data[idx+1:]

		t.processLine(line)
	}
}

// feedTrailingLine processes any remaining buffered data as a final line
// (stream ended without a trailing newline).
func (t *responsesSSETracker) feedTrailingLine() {
	if len(t.pending) > 0 {
		t.processLine(t.pending)
		t.pending = nil
	}
}

// processLine parses a single SSE line and updates the terminal state.
func (t *responsesSSETracker) processLine(line []byte) {
	trimmed := strings.TrimSpace(string(line))
	if trimmed == "" {
		return
	}
	if !strings.HasPrefix(trimmed, "data:") {
		return
	}
	payload := strings.TrimSpace(strings.TrimPrefix(trimmed, "data:"))
	if payload == "" || payload == "[DONE]" {
		// [DONE] is NOT a terminal state for Responses — ignore it.
		return
	}

	var event struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal([]byte(payload), &event); err != nil {
		// Not valid JSON — not a Responses event we care about.
		return
	}

	switch event.Type {
	case "response.completed":
		if t.terminal != responsesTerminalFailed {
			t.terminal = responsesTerminalCompleted
		}
	case "response.incomplete":
		if t.terminal != responsesTerminalFailed {
			t.terminal = responsesTerminalIncomplete
		}
	case "response.failed":
		// Failed has the highest priority — once seen, it cannot be
		// overridden by a later completed/incomplete.
		t.terminal = responsesTerminalFailed
	}
}

// Finalize processes any trailing buffered data and returns the verdict.
//
//   - response.failed seen → protocol.ErrUpstreamResponseFailed
//   - parse error occurred → that error
//   - no terminal event seen → io.ErrUnexpectedEOF
//   - response.completed or response.incomplete → nil (success)
func (t *responsesSSETracker) Finalize() error {
	t.feedTrailingLine()

	switch {
	case t.terminal == responsesTerminalFailed:
		return fmt.Errorf("%w: response.failed terminal in stream", protocol.ErrUpstreamResponseFailed)
	case t.parseErr != nil:
		return t.parseErr
	case t.terminal == responsesTerminalNone:
		return io.ErrUnexpectedEOF
	default:
		return nil
	}
}