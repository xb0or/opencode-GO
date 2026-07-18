package protocol

import (
	"bytes"
	"errors"
	"fmt"
	"io"
)

// ---------------------------------------------------------------------------
// Incremental cross-protocol SSE streaming
//
// StreamConvertIncremental converts an upstream SSE stream from upProto to
// dstProto event-by-event, flushing after each emitted event so the client
// receives data as soon as the upstream produces it.
//
// The caller pre-reads the first complete SSE event (for validation /
// failover) and passes it as firstEvent. The rest of the upstream body is
// passed as rest. StreamConvertIncremental feeds firstEvent ++ rest to the
// upstream decoder, translates each decoded IRStreamEvent to the target
// protocol, and writes it to dst, calling flush after each batch.
//
// P0-2 first-event validation: the optional onFirstEvent callback is invoked
// exactly once, after the first target event bytes have been successfully
// written to dst (and before flush). If onFirstEvent returns a non-nil error,
// the conversion aborts immediately — the caller may treat this as a
// failover-eligible error (no committed response yet). The caller uses
// onFirstEvent to commit HTTP 200 + text/event-stream headers and swap any
// staging buffer for the real destination writer.
//
// Failover is only allowed before the first target event is emitted — once
// onFirstEvent returns nil (or is nil), the response is committed and
// subsequent upstream errors cannot trigger failover. On decoder error after
// commit, onError emits only the stream terminator (no success terminal
// events), so a failed generation is not mistaken for a successful one.
// ---------------------------------------------------------------------------

// ErrStreamConvertInvalid is returned when the upstream stream cannot be
// decoded — the caller should treat this as a failover-eligible error if
// no bytes have been written to the client yet.
var ErrStreamConvertInvalid = errors.New("stream: upstream SSE could not be decoded")

// ErrUpstreamResponseFailed indicates the upstream Responses API sent a
// response.failed event (or an equivalent failure terminal). The stream was
// syntactically valid but the upstream reported a logical failure. Callers
// must NOT treat this as a success: mark the key/upstream as failed even if
// HTTP 200 was already committed to the client. When the response is not yet
// committed, the caller may failover to the next upstream.
var ErrUpstreamResponseFailed = errors.New("upstream response failed")

// StreamConvertIncremental incrementally converts an upstream SSE stream to
// the target protocol. It returns the accumulated IRResponse (for usage
// logging) and an error if the stream could not be processed.
//
// firstEvent is the pre-read first SSE event bytes (including the trailing
// blank line). rest is the remaining upstream body reader. dst receives the
// converted SSE output. flush is called after each emitted event batch.
//
// onFirstEvent, if non-nil, is called once after the first target event has
// been written to dst. If it returns an error, conversion aborts and that
// error is returned to the caller (wrapped as ErrStreamConvertInvalid if it
// is not already a recognized error). This lets the caller validate that the
// upstream stream decodes to at least one valid target event before
// committing HTTP 200.
func StreamConvertIncremental(
	upProto, dstProto string,
	firstEvent []byte,
	rest io.Reader,
	dst io.Writer,
	flush func(),
	onFirstEvent func() error,
) (*IRResponse, error) {
	// Combine pre-read first event with the rest of the body.
	combined := io.MultiReader(bytes.NewReader(firstEvent), rest)

	// Accumulated response for usage tracking.
	acc := &IRResponse{}

	// Wrap dst so that the first successful write triggers onFirstEvent.
	writer := dst
	if onFirstEvent != nil {
		writer = &firstEventWriter{dst: dst, onFirst: onFirstEvent}
	}

	// Create the target emitter.
	emitter, err := newTargetEmitter(dstProto, writer, flush)
	if err != nil {
		return nil, err
	}

	// Track whether onStart has been called.
	emitterStarted := false

	// Normalize each upstream event and feed to the target emitter.
	normalize := func(ev *IRStreamEvent) error {
		// P0-2: only call onStart once we've seen a MEANINGFUL event.
		// Semantically empty events (no deltas, no usage, no finish reason,
		// no response metadata) must NOT commit HTTP 200. Error objects are
		// already rejected by the decoders, so by the time we get here the
		// event decoded successfully — but it might still be a no-op
		// (e.g. a content_block_stop with no payload, or a ping).
		if !emitterStarted {
			if !isMeaningfulEvent(ev) {
				return nil
			}
			id, model := extractIDModel(ev)
			acc.ID = id
			acc.Model = model
			if err := emitter.onStart(id, model); err != nil {
				return err
			}
			emitterStarted = true
		}

		// Extract and emit content deltas.
		if ev.Choice != nil && ev.Choice.Delta != nil {
			// Text and thinking content.
			for _, c := range ev.Choice.Delta.Content {
				if c.Text == "" {
					continue
				}
				if c.Type == "thinking" {
					if err := emitter.onThinkingDelta(c.Text); err != nil {
						return err
					}
				} else {
					if err := emitter.onTextDelta(c.Text); err != nil {
						return err
					}
				}
			}
			// Convenience text field (Chat protocol puts text directly on delta).
			if ev.Choice.Delta.Text != "" && len(ev.Choice.Delta.Content) == 0 {
				if err := emitter.onTextDelta(ev.Choice.Delta.Text); err != nil {
					return err
				}
			}
			// Tool calls.
			for _, tc := range ev.Choice.Delta.ToolCalls {
				// P1-2: prefer the tool call's own Index; fall back to the
				// Choice.Index when the upstream didn't populate it.
				toolIdx := tc.Index
				if toolIdx == 0 {
					toolIdx = ev.Choice.Index
				}
				if tc.Name != "" || tc.ID != "" {
					if err := emitter.onToolCallStart(toolIdx, tc.ID, tc.Name); err != nil {
						return err
					}
				}
				if tc.Arguments != "" {
					if err := emitter.onToolCallDelta(toolIdx, tc.Arguments); err != nil {
						return err
					}
				}
			}
		}

		// Tool call delta (Messages content_block_delta with tool).
		if ev.ToolCallDelta != nil {
			if err := emitter.onToolCallDelta(ev.ToolCallDelta.Index, ev.ToolCallDelta.Arguments); err != nil {
				return err
			}
		}

		// ContentDelta field (used by Messages/Responses decoders).
		if ev.ContentDelta != "" && (ev.Choice == nil || ev.Choice.Delta == nil || len(ev.Choice.Delta.Content) == 0) {
			if err := emitter.onTextDelta(ev.ContentDelta); err != nil {
				return err
			}
		}

		// P1-4: process Usage BEFORE Finish so the emitter has the usage
		// available when onFinish fires (Messages message_delta carries both
		// stop_reason and output_tokens in the same event).
		if ev.Response != nil && ev.Response.Usage != nil {
			mergeUsage(acc, ev.Response.Usage)
			if err := emitter.onUsage(ev.Response.Usage); err != nil {
				return err
			}
		}

		// Finish reason.
		if ev.Choice != nil && ev.Choice.FinishReason != "" {
			// P0-2 (round-7): a "failed" finish reason means the upstream
			// Responses API reported response.failed. Intercept BEFORE
			// calling emitter.onFinish — onFinish would write a terminal
			// chunk (e.g. Chat finish_reason) to the output, which would
			// trigger onFirstEvent and commit HTTP 200 prematurely,
			// blocking failover. Return the sentinel immediately so the
			// caller can failover (if uncommitted) or mark the key as
			// failed (if already committed) without emitting any content.
			if ev.Choice.FinishReason == "failed" {
				acc.setFinishReason("failed")
				return fmt.Errorf("%w: response.failed terminal in normalize", ErrUpstreamResponseFailed)
			}
			acc.setFinishReason(ev.Choice.FinishReason)
			if err := emitter.onFinish(ev.Choice.FinishReason); err != nil {
				return err
			}
		}

		return nil
	}

	// Select the upstream decoder.
	var decErr error
	switch upProto {
	case "chat":
		decErr = ChatStreamDecoder(combined, normalize)
	case "messages":
		decErr = MessagesStreamDecoder(combined, normalize)
	case "responses":
		decErr = ResponsesStreamDecoder(combined, normalize)
	default:
		return nil, fmt.Errorf("stream: unsupported upstream protocol %q", upProto)
	}
	if decErr != nil {
		// P0-1/P0-3: a decoder error (including io.ErrUnexpectedEOF when
		// upstream closed without a terminal event) must NOT emit success
		// terminal events. If no valid target event has been emitted yet
		// (onFirstEvent not triggered), do NOT call onError either — any
		// bytes written by onError (e.g. [DONE]) would flow through
		// firstEventWriter and trigger onFirstEvent, committing HTTP 200
		// to the client for a stream that never produced a valid event.
		// Only call onError if the response is already committed.
		if fw, ok := writer.(*firstEventWriter); ok && !fw.triggered {
			return acc, decErr
		}
		_ = emitter.onError(decErr)
		return acc, decErr
	}
	// Emit terminal events for a successful stream (P0-3: onComplete).
	reason := ""
	if len(acc.Choices) > 0 {
		reason = acc.Choices[0].FinishReason
	}
	// P0-1/P0-2 (round-6): a "failed" finish reason means the upstream
	// Responses API reported a response.failed event. ALWAYS return
	// ErrUpstreamResponseFailed so the caller can mark the key/upstream
	// as failed — even when HTTP 200 was already committed, the upstream
	// reported a logical failure and must NOT be counted as a success.
	// When the response is not yet committed, the caller can failover.
	if reason == "failed" {
		if fw, ok := writer.(*firstEventWriter); ok && !fw.triggered {
			// Stream never committed; surface the error for failover.
			return acc, fmt.Errorf("%w: response.failed terminal", ErrUpstreamResponseFailed)
		}
		// Already committed: truncate the client stream via onError, but
		// still return the sentinel so the caller marks the key as failed.
		_ = emitter.onError(ErrUpstreamResponseFailed)
		return acc, fmt.Errorf("%w: response.failed terminal after commit", ErrUpstreamResponseFailed)
	}
	if err := emitter.onComplete(reason); err != nil {
		return acc, err
	}

	return acc, nil
}

// firstEventWriter wraps an io.Writer and invokes onFirst exactly once after
// the first non-empty successful write. If onFirst returns an error, the
// Write returns that error (after the bytes have already been written to
// dst), which propagates up and aborts the conversion. This implements the
// P0-2 first-event validation hook: the caller's onFirst commits HTTP 200
// and swaps any staging buffer for the real destination.
type firstEventWriter struct {
	dst       io.Writer
	onFirst   func() error
	triggered bool
}

func (w *firstEventWriter) Write(p []byte) (int, error) {
	n, err := w.dst.Write(p)
	if err != nil {
		return n, err
	}
	if !w.triggered && n > 0 && w.onFirst != nil {
		w.triggered = true
		if cbErr := w.onFirst(); cbErr != nil {
			return n, cbErr
		}
	}
	return n, nil
}

// extractIDModel pulls the response ID and model from an event's Response
// field, falling back to empty strings.
func extractIDModel(ev *IRStreamEvent) (id, model string) {
	if ev.Response != nil {
		return ev.Response.ID, ev.Response.Model
	}
	return "", ""
}

// isMeaningfulEvent reports whether ev carries enough semantic content to
// justify calling onStart (and thus committing HTTP 200 to the client).
//
// P0-2: a decoded-but-empty event (e.g. content_block_stop, ping, or a
// Messages message_delta with no stop_reason/usage) must NOT trigger onStart.
// The decoders already reject semantically *invalid* events (unknown types,
// error payloads, empty chunks), so by the time we get here the event is
// structurally valid — but it may still be a no-op lifecycle event. We only
// start the emitter when we have actual content to deliver: a text/thinking
// delta, a tool-call delta, usage, a finish reason, or response metadata
// (id/model from message_start/response.created).
func isMeaningfulEvent(ev *IRStreamEvent) bool {
	if ev == nil {
		return false
	}
	// Response metadata (message_start / response.created) — always
	// meaningful; it carries id/model and may carry initial usage.
	if ev.Response != nil && (ev.Response.ID != "" || ev.Response.Model != "" || ev.Response.Usage != nil) {
		return true
	}
	// Content delta (text or thinking).
	if ev.ContentDelta != "" {
		return true
	}
	// Choice with delta content (text/thinking) or tool calls.
	if ev.Choice != nil {
		if ev.Choice.Delta != nil {
			if ev.Choice.Delta.Text != "" {
				return true
			}
			for _, c := range ev.Choice.Delta.Content {
				if c.Text != "" {
					return true
				}
			}
			if len(ev.Choice.Delta.ToolCalls) > 0 {
				for _, tc := range ev.Choice.Delta.ToolCalls {
					if tc.Name != "" || tc.ID != "" || tc.Arguments != "" {
						return true
					}
				}
			}
		}
		// Finish reason on its own is meaningful (it ends the stream).
		if ev.Choice.FinishReason != "" {
			return true
		}
	}
	// Tool call delta (Messages input_json_delta).
	if ev.ToolCallDelta != nil && ev.ToolCallDelta.Arguments != "" {
		return true
	}
	return false
}

// mergeUsage merges usage info into the accumulated response.
func mergeUsage(resp *IRResponse, u *IRUsage) {
	if resp.Usage == nil {
		resp.Usage = &IRUsage{}
	}
	if u.PromptTokens > 0 {
		resp.Usage.PromptTokens = u.PromptTokens
	}
	if u.CompletionTokens > 0 {
		resp.Usage.CompletionTokens = u.CompletionTokens
	}
	if u.TotalTokens > 0 {
		resp.Usage.TotalTokens = u.TotalTokens
	}
	if u.ReasoningTokens > 0 {
		resp.Usage.ReasoningTokens = u.ReasoningTokens
	}
	if u.CacheReadTokens > 0 {
		resp.Usage.CacheReadTokens = u.CacheReadTokens
	}
	if u.CacheCreationTokens > 0 {
		resp.Usage.CacheCreationTokens = u.CacheCreationTokens
	}
	// P1-2: cross-protocol streams often deliver input_tokens (PromptTokens)
	// and output_tokens (CompletionTokens) in SEPARATE events, neither of
	// which carries total_tokens. Recalculate total_tokens from the parts
	// when it was not explicitly provided so the final usage report is not
	// missing the total.
	if resp.Usage.TotalTokens == 0 && resp.Usage.PromptTokens > 0 && resp.Usage.CompletionTokens > 0 {
		resp.Usage.TotalTokens = resp.Usage.PromptTokens + resp.Usage.CompletionTokens
	}
}

// mergeUsageInto merges src into dst (in-place), copying only non-zero fields.
func mergeUsageInto(dst, src *IRUsage) {
	if dst == nil || src == nil {
		return
	}
	if src.PromptTokens > 0 {
		dst.PromptTokens = src.PromptTokens
	}
	if src.CompletionTokens > 0 {
		dst.CompletionTokens = src.CompletionTokens
	}
	if src.TotalTokens > 0 {
		dst.TotalTokens = src.TotalTokens
	}
	if src.ReasoningTokens > 0 {
		dst.ReasoningTokens = src.ReasoningTokens
	}
	if src.CacheReadTokens > 0 {
		dst.CacheReadTokens = src.CacheReadTokens
	}
	if src.CacheCreationTokens > 0 {
		dst.CacheCreationTokens = src.CacheCreationTokens
	}
	// P1-2: recalculate total_tokens when it was not explicitly provided
	// but both prompt and completion counts are now available (they may
	// arrive in separate events in cross-protocol streams).
	if dst.TotalTokens == 0 && dst.PromptTokens > 0 && dst.CompletionTokens > 0 {
		dst.TotalTokens = dst.PromptTokens + dst.CompletionTokens
	}
}

func (r *IRResponse) setFinishReason(reason string) {
	if len(r.Choices) == 0 {
		r.Choices = []IRChoice{{}}
	}
	r.Choices[0].FinishReason = reason
}

// ---------------------------------------------------------------------------
// targetEmitter — protocol-specific state machine for incremental emission.
//
// P0-3: onEnd is split into onComplete (normal EOF, emits success terminal
// events) and onError (decoder error, emits only the stream terminator).
// P1-3: onUsage forwards usage information to the client.
// ---------------------------------------------------------------------------

type targetEmitter interface {
	onStart(id, model string) error
	onTextDelta(text string) error
	onThinkingDelta(text string) error
	onToolCallStart(idx int, id, name string) error
	onToolCallDelta(idx int, args string) error
	onFinish(reason string) error
	onUsage(u *IRUsage) error
	// onComplete emits success terminal events for a normal EOF. reason is
	// the accumulated finish reason (may be empty, in which case a protocol
	// default is used).
	onComplete(reason string) error
	// onError is called when the upstream decoder fails. It must NOT emit
	// success terminal events; it only terminates the stream so the client
	// does not mistake a failed generation for a successful one.
	onError(err error) error
}

func newTargetEmitter(proto string, w io.Writer, flush func()) (targetEmitter, error) {
	switch proto {
	case "chat":
		return &chatEmitter{enc: NewChatStreamEncoder(w), flush: flush}, nil
	case "messages":
		return &messagesEmitter{enc: NewMessagesStreamEncoder(w), flush: flush}, nil
	case "responses":
		return &responsesEmitter{enc: NewResponsesStreamEncoder(w), flush: flush}, nil
	default:
		return nil, fmt.Errorf("stream: unsupported target protocol %q", proto)
	}
}

// flushWriter calls flush after each write.
type flushWriter struct {
	w     io.Writer
	flush func()
}

func (fw *flushWriter) Write(p []byte) (int, error) {
	n, err := fw.w.Write(p)
	if fw.flush != nil {
		fw.flush()
	}
	return n, err
}

// ---------------------------------------------------------------------------
// Chat target emitter
// ---------------------------------------------------------------------------

type chatEmitter struct {
	enc      *ChatStreamEncoder
	flush    func()
	started  bool
	id       string
	model    string
	roleSent bool
	finished bool
	usage    *IRUsage // accumulated; emitted as a final usage-only chunk (P1-3)
}

func (e *chatEmitter) onStart(id, model string) error {
	e.started = true
	e.id = id
	e.model = model
	return nil
}

func (e *chatEmitter) emit(ev *IRStreamEvent) error {
	if e.id != "" || e.model != "" {
		if ev.Response == nil {
			ev.Response = &IRResponse{}
		}
		if e.id != "" {
			ev.Response.ID = e.id
		}
		if e.model != "" {
			ev.Response.Model = e.model
		}
	}
	if err := e.enc.WriteEvent(ev); err != nil {
		return err
	}
	if e.flush != nil {
		e.flush()
	}
	return nil
}

func (e *chatEmitter) onTextDelta(text string) error {
	if !e.roleSent {
		// Initial chunk with role.
		if err := e.emit(&IRStreamEvent{
			Type: "completion",
			Choice: &IRChoice{
				Index: 0,
				Delta: &IRMessage{Role: "assistant"},
			},
		}); err != nil {
			return err
		}
		e.roleSent = true
	}
	return e.emit(&IRStreamEvent{
		Type: "completion",
		Choice: &IRChoice{
			Index: 0,
			Delta: &IRMessage{Content: []IRContent{{Type: "text", Text: text}}},
		},
	})
}

func (e *chatEmitter) onThinkingDelta(text string) error {
	if !e.roleSent {
		if err := e.emit(&IRStreamEvent{
			Type: "completion",
			Choice: &IRChoice{
				Index: 0,
				Delta: &IRMessage{Role: "assistant"},
			},
		}); err != nil {
			return err
		}
		e.roleSent = true
	}
	return e.emit(&IRStreamEvent{
		Type: "completion",
		Choice: &IRChoice{
			Index: 0,
			Delta: &IRMessage{Content: []IRContent{{Type: "thinking", Text: text}}},
		},
	})
}

func (e *chatEmitter) onToolCallStart(idx int, id, name string) error {
	if !e.roleSent {
		if err := e.emit(&IRStreamEvent{
			Type: "completion",
			Choice: &IRChoice{
				Index: 0,
				Delta: &IRMessage{Role: "assistant"},
			},
		}); err != nil {
			return err
		}
		e.roleSent = true
	}
	return e.emit(&IRStreamEvent{
		Type: "completion",
		Choice: &IRChoice{
			Index: 0,
			Delta: &IRMessage{
				ToolCalls: []IRToolCall{{Index: idx, ID: id, Name: name}},
			},
		},
	})
}

func (e *chatEmitter) onToolCallDelta(idx int, args string) error {
	return e.emit(&IRStreamEvent{
		Type: "completion",
		Choice: &IRChoice{
			Index: 0,
			Delta: &IRMessage{
				ToolCalls: []IRToolCall{{Index: idx, Arguments: args}},
			},
		},
	})
}

func (e *chatEmitter) onFinish(reason string) error {
	if e.finished {
		return nil
	}
	e.finished = true
	// P1-5: map the incoming finish reason to the Chat vocabulary.
	mapped := mapFinishReasonToChat(reason)
	return e.emit(&IRStreamEvent{
		Type: "completion",
		Choice: &IRChoice{
			Index:        0,
			FinishReason: mapped,
		},
	})
}

func (e *chatEmitter) onUsage(u *IRUsage) error {
	if u == nil {
		return nil
	}
	if e.usage == nil {
		e.usage = &IRUsage{}
	}
	mergeUsageInto(e.usage, u)
	return nil
}

func (e *chatEmitter) onComplete(reason string) error {
	// P1-5: map the incoming reason to the Chat vocabulary.
	mapped := mapFinishReasonToChat(reason)
	if mapped == "" {
		mapped = "stop"
	}
	if !e.finished {
		// Emit a finish event with a default reason if none was received.
		if err := e.emit(&IRStreamEvent{
			Type: "completion",
			Choice: &IRChoice{
				Index:        0,
				FinishReason: mapped,
			},
		}); err != nil {
			return err
		}
		e.finished = true
	}
	// P1-3: emit a final usage-only chunk so the client receives usage.
	if e.usage != nil {
		if err := e.emit(&IRStreamEvent{
			Type:     "completion",
			Response: &IRResponse{Usage: e.usage},
		}); err != nil {
			return err
		}
	}
	if err := e.enc.WriteDone(); err != nil {
		return err
	}
	if e.flush != nil {
		e.flush()
	}
	return nil
}

func (e *chatEmitter) onError(err error) error {
	// P0-3: do NOT emit a finish event, [DONE], or any success terminal
	// event. The stream is simply truncated — the client observes an
	// incomplete 200 SSE response and can detect the failure by the
	// absence of [DONE]. Emitting [DONE] would make a failed generation
	// look successful.
	if e.flush != nil {
		e.flush()
	}
	return nil
}

// ---------------------------------------------------------------------------
// Messages target emitter
// ---------------------------------------------------------------------------

// toolBlockState tracks the Messages content block allocated for one upstream
// tool call (P1-1). Each upstream tool index maps to its own content block so
// multiple tool calls do not collide on a single currentType/blockIdx. Blocks
// stay open until the response completes — interleaved deltas (start0,
// start1, delta0, delta1) route to the correct block via the map.
type toolBlockState struct {
	SourceIndex int    // upstream tool index
	BlockIndex  int    // Messages content_block index
	ID          string // tool call id
	Name        string // tool call name
	Started     bool   // content_block_start emitted
	Closed      bool   // content_block_stop emitted
}

type messagesEmitter struct {
	enc            *MessagesStreamEncoder
	flush          func()
	started        bool
	id             string
	model          string
	currentType    string // "text" | "thinking" | "" (only non-tool blocks)
	blockIdx       int    // index of the currently-open NON-tool block
	toolBlocks     map[int]*toolBlockState
	nextBlockIdx   int // monotonically increasing content block index
	finished       bool
	usage          *IRUsage
}

func (e *messagesEmitter) onStart(id, model string) error {
	e.started = true
	e.id = id
	e.model = model
	e.toolBlocks = make(map[int]*toolBlockState)
	e.nextBlockIdx = 0
	// Emit message_start immediately so the client sees the stream has begun.
	return e.emit(&IRStreamEvent{
		Type:     "message_start",
		Response: &IRResponse{ID: id, Model: model},
	})
}

func (e *messagesEmitter) emit(ev *IRStreamEvent) error {
	if err := e.enc.WriteEvent(ev); err != nil {
		return err
	}
	if e.flush != nil {
		e.flush()
	}
	return nil
}

// closeCurrentBlock closes the currently-open NON-tool block (text/thinking),
// if any. It does NOT touch tool blocks (P1-1: tool blocks stay open until
// the response completes so interleaved deltas can route to them).
func (e *messagesEmitter) closeCurrentBlock() error {
	if e.currentType == "" {
		return nil
	}
	stopEv := &IRStreamEvent{
		Type:   "content_block_stop",
		Choice: &IRChoice{Index: e.blockIdx},
	}
	if err := e.emit(stopEv); err != nil {
		return err
	}
	e.currentType = ""
	e.nextBlockIdx++
	return nil
}

// openBlock allocates and emits a content_block_start for a non-tool block.
func (e *messagesEmitter) openBlock(blockType string, tcID, tcName string) error {
	delta := &IRMessage{}
	if blockType == "tool" {
		delta.ToolCalls = []IRToolCall{{ID: tcID, Name: tcName}}
	} else {
		delta.Content = []IRContent{{Type: blockType}}
	}
	idx := e.nextBlockIdx
	startEv := &IRStreamEvent{
		Type:   "content_block_start",
		Choice: &IRChoice{Index: idx, Delta: delta},
	}
	if err := e.emit(startEv); err != nil {
		return err
	}
	return nil
}

func (e *messagesEmitter) onTextDelta(text string) error {
	if e.currentType != "" && e.currentType != "text" {
		if err := e.closeCurrentBlock(); err != nil {
			return err
		}
	}
	if e.currentType == "" {
		e.blockIdx = e.nextBlockIdx
		if err := e.openBlock("text", "", ""); err != nil {
			return err
		}
		e.currentType = "text"
	}
	return e.emit(&IRStreamEvent{
		Type:         "content_block_delta",
		ContentDelta: text,
		Choice: &IRChoice{
			Index: e.blockIdx,
			Delta: &IRMessage{Content: []IRContent{{Type: "text", Text: text}}},
		},
	})
}

func (e *messagesEmitter) onThinkingDelta(text string) error {
	if e.currentType != "" && e.currentType != "thinking" {
		if err := e.closeCurrentBlock(); err != nil {
			return err
		}
	}
	if e.currentType == "" {
		e.blockIdx = e.nextBlockIdx
		if err := e.openBlock("thinking", "", ""); err != nil {
			return err
		}
		e.currentType = "thinking"
	}
	return e.emit(&IRStreamEvent{
		Type:         "content_block_delta",
		ContentDelta: text,
		Choice: &IRChoice{
			Index: e.blockIdx,
			Delta: &IRMessage{Content: []IRContent{{Type: "thinking", Text: text}}},
		},
	})
}

func (e *messagesEmitter) onToolCallStart(idx int, id, name string) error {
	// P1-1: each upstream tool index gets its own Messages content block, and
	// blocks stay OPEN so interleaved deltas (start0, start1, delta0, delta1)
	// can route to the correct block. Only close a non-tool block when
	// switching to a tool; do NOT close a previous tool block.
	if st, ok := e.toolBlocks[idx]; ok && st.Started && !st.Closed {
		// Already started — update id/name if provided (defensive).
		if id != "" {
			st.ID = id
		}
		if name != "" {
			st.Name = name
		}
		return nil
	}
	// Close any open non-tool block (text/thinking) before opening a tool
	// block. We do NOT close other tool blocks (P1-1).
	if e.currentType != "" {
		if err := e.closeCurrentBlock(); err != nil {
			return err
		}
	}
	blockIdx := e.nextBlockIdx
	// Emit content_block_start for the tool directly (openBlock writes into
	// nextBlockIdx, but we want explicit control here so we mirror it).
	delta := &IRMessage{ToolCalls: []IRToolCall{{ID: id, Name: name}}}
	startEv := &IRStreamEvent{
		Type:   "content_block_start",
		Choice: &IRChoice{Index: blockIdx, Delta: delta},
	}
	if err := e.emit(startEv); err != nil {
		return err
	}
	e.nextBlockIdx++
	e.toolBlocks[idx] = &toolBlockState{
		SourceIndex: idx,
		BlockIndex:  blockIdx,
		ID:          id,
		Name:        name,
		Started:     true,
		Closed:      false,
	}
	return nil
}

func (e *messagesEmitter) onToolCallDelta(idx int, args string) error {
	// P1-1: route the delta to the correct content block for this tool index.
	st, ok := e.toolBlocks[idx]
	if !ok || !st.Started || st.Closed {
		// No open block for this tool; open one opportunistically so the
		// delta is not lost.
		if err := e.onToolCallStart(idx, "", ""); err != nil {
			return err
		}
		st = e.toolBlocks[idx]
	}
	blockIdx := st.BlockIndex
	return e.emit(&IRStreamEvent{
		Type:          "content_block_delta",
		ToolCallDelta: &IRToolCallDelta{Index: blockIdx, Arguments: args},
		Choice:        &IRChoice{Index: blockIdx},
	})
}

func (e *messagesEmitter) onFinish(reason string) error {
	if e.finished {
		return nil
	}
	// P1-5: map the incoming finish reason to the Messages vocabulary.
	finReason := mapFinishReasonToMessages(reason)
	if finReason == "" {
		finReason = "end_turn"
	}
	// Close the open non-tool block (if any) first.
	if err := e.closeCurrentBlock(); err != nil {
		return err
	}
	// P1-1: close ALL still-open tool blocks in ascending BlockIndex order
	// so the client sees stops in the same order as starts.
	type openBlock struct{ blockIdx int }
	var openBlocks []openBlock
	for _, st := range e.toolBlocks {
		if st.Started && !st.Closed {
			openBlocks = append(openBlocks, openBlock{blockIdx: st.BlockIndex})
		}
	}
	// Simple stable sort by blockIdx (small N).
	for i := 0; i < len(openBlocks); i++ {
		for j := i + 1; j < len(openBlocks); j++ {
			if openBlocks[j].blockIdx < openBlocks[i].blockIdx {
				openBlocks[i], openBlocks[j] = openBlocks[j], openBlocks[i]
			}
		}
	}
	for _, ob := range openBlocks {
		stopEv := &IRStreamEvent{
			Type:   "content_block_stop",
			Choice: &IRChoice{Index: ob.blockIdx},
		}
		if err := e.emit(stopEv); err != nil {
			return err
		}
	}
	for _, st := range e.toolBlocks {
		if st.Started && !st.Closed {
			st.Closed = true
		}
	}
	e.finished = true
	deltaMsg := &IRStreamEvent{
		Type: "message_delta",
		Choice: &IRChoice{
			Index:        0,
			FinishReason: finReason,
		},
	}
	if e.usage != nil {
		deltaMsg.Response = &IRResponse{Usage: &IRUsage{CompletionTokens: e.usage.CompletionTokens}}
	}
	return e.emit(deltaMsg)
}

func (e *messagesEmitter) onUsage(u *IRUsage) error {
	if u == nil {
		return nil
	}
	// P1-3: store usage so message_delta can carry the completion token count.
	if e.usage == nil {
		e.usage = &IRUsage{}
	}
	mergeUsageInto(e.usage, u)
	return nil
}

func (e *messagesEmitter) onComplete(reason string) error {
	// P1-5: onFinish maps the incoming reason to the Messages vocabulary, so
	// pass the raw reason here (do not double-map). A default of "end_turn" is
	// used only when no reason was ever observed.
	if !e.finished {
		if err := e.onFinish(reason); err != nil {
			return err
		}
	}
	if err := e.enc.WriteDone(); err != nil {
		return err
	}
	if e.flush != nil {
		e.flush()
	}
	return nil
}

func (e *messagesEmitter) onError(err error) error {
	// P0-3: do NOT emit message_delta/end_turn/message_stop (success terminal
	// events). The stream is truncated — the client detects the failure by
	// the absence of message_stop.
	if e.flush != nil {
		e.flush()
	}
	return nil
}

// ---------------------------------------------------------------------------
// Responses target emitter
// ---------------------------------------------------------------------------

// respToolItem tracks one function_call output item for the Responses stream
// (P1-2). Each upstream tool index maps to its own output item with a distinct
// call_id, name, and output_index.
type respToolItem struct {
	OutputIndex int    // Responses output_index for this item
	CallID      string // function call_id
	Name        string // function name
	Arguments   string // accumulated arguments
	Started     bool   // output_item.added emitted
	Done        bool   // output_item.done emitted
}

type responsesEmitter struct {
	enc                *ResponsesStreamEncoder
	flush              func()
	started            bool
	id                 string
	model              string
	itemAdded          bool // message output item added
	partAdded          bool // content part added (text)
	currentType        string
	messageOutputIndex int // output_index for the message item
	nextOutputIndex    int // next available output_index
	toolItems          map[int]*respToolItem
	toolOrder          []int // source indices in start order
	finished           bool
	usage              *IRUsage
}

func (e *responsesEmitter) onStart(id, model string) error {
	e.started = true
	e.id = id
	e.model = model
	return e.emit(&IRStreamEvent{
		Type:     "response.created",
		Response: &IRResponse{ID: id, Model: model},
	})
}

func (e *responsesEmitter) emit(ev *IRStreamEvent) error {
	if err := e.enc.WriteEvent(ev); err != nil {
		return err
	}
	if e.flush != nil {
		e.flush()
	}
	return nil
}

// ensureMessageItem emits response.output_item.added for the assistant message
// item on first text/thinking delta and records its output_index.
func (e *responsesEmitter) ensureMessageItem() error {
	if e.itemAdded {
		return nil
	}
	e.itemAdded = true
	e.messageOutputIndex = e.nextOutputIndex
	e.nextOutputIndex++
	return e.emit(&IRStreamEvent{
		Type: "response.output_item.added",
		Choice: &IRChoice{
			Index: e.messageOutputIndex,
			Delta: &IRMessage{Role: "assistant"},
		},
	})
}

func (e *responsesEmitter) onTextDelta(text string) error {
	if err := e.ensureMessageItem(); err != nil {
		return err
	}
	if !e.partAdded {
		if err := e.emit(&IRStreamEvent{
			Type: "response.content_part.added",
			Choice: &IRChoice{
				Index: e.messageOutputIndex,
				Delta: &IRMessage{Content: []IRContent{{Type: "text"}}},
			},
		}); err != nil {
			return err
		}
		e.partAdded = true
		e.currentType = "text"
	}
	return e.emit(&IRStreamEvent{
		Type:         "response.output_text.delta",
		ContentDelta: text,
		Choice:       &IRChoice{Index: e.messageOutputIndex},
	})
}

func (e *responsesEmitter) onThinkingDelta(text string) error {
	if err := e.ensureMessageItem(); err != nil {
		return err
	}
	return e.emit(&IRStreamEvent{
		Type:         "response.reasoning_content.delta",
		ContentDelta: text,
		Choice: &IRChoice{
			Index: e.messageOutputIndex,
			Delta: &IRMessage{Content: []IRContent{{Type: "thinking", Text: text}}},
		},
	})
}

func (e *responsesEmitter) onToolCallStart(idx int, id, name string) error {
	// P1-2: save id/name per idx and emit response.output_item.added with a
	// function_call item (including call_id and function name), each tool call
	// getting its own output item with a distinct call_id.
	if e.toolItems == nil {
		e.toolItems = make(map[int]*respToolItem)
	}
	if ti, ok := e.toolItems[idx]; ok {
		// Already started — update id/name if provided (defensive).
		if id != "" {
			ti.CallID = id
		}
		if name != "" {
			ti.Name = name
		}
		return nil
	}
	outIdx := e.nextOutputIndex
	e.nextOutputIndex++
	ti := &respToolItem{
		OutputIndex: outIdx,
		CallID:      id,
		Name:        name,
		Started:     true,
	}
	e.toolItems[idx] = ti
	e.toolOrder = append(e.toolOrder, idx)
	return e.emit(&IRStreamEvent{
		Type: "response.output_item.added",
		Choice: &IRChoice{
			Index: outIdx,
			Delta: &IRMessage{
				ToolCalls: []IRToolCall{{ID: id, Name: name}},
			},
		},
	})
}

func (e *responsesEmitter) onToolCallDelta(idx int, args string) error {
	// P1-2: route deltas to the correct output_index.
	ti, ok := e.toolItems[idx]
	if !ok {
		// No start emitted; emit one with empty id/name so the delta is not lost.
		if err := e.onToolCallStart(idx, "", ""); err != nil {
			return err
		}
		ti = e.toolItems[idx]
	}
	ti.Arguments += args
	return e.emit(&IRStreamEvent{
		Type:         "response.function_call_arguments.delta",
		ContentDelta: args,
		ToolCallDelta: &IRToolCallDelta{
			Index:     ti.OutputIndex,
			Arguments: args,
		},
		Choice: &IRChoice{Index: ti.OutputIndex},
	})
}

func (e *responsesEmitter) onFinish(reason string) error {
	if e.finished {
		return nil
	}
	e.finished = true
	// Close the text content part / message item if open.
	if e.partAdded && e.currentType == "text" {
		if err := e.emit(&IRStreamEvent{
			Type:         "response.output_text.done",
			ContentDelta: "",
			Choice:       &IRChoice{Index: e.messageOutputIndex},
		}); err != nil {
			return err
		}
		if err := e.emit(&IRStreamEvent{
			Type:         "response.content_part.done",
			ContentDelta: "",
			Choice:       &IRChoice{Index: e.messageOutputIndex},
		}); err != nil {
			return err
		}
	}
	if e.itemAdded {
		if err := e.emit(&IRStreamEvent{
			Type:   "response.output_item.done",
			Choice: &IRChoice{Index: e.messageOutputIndex, Message: &IRMessage{Role: "assistant"}},
		}); err != nil {
			return err
		}
	}
	// Close each tool item (P1-2): emit function_call_arguments.done and
	// output_item.done for each tool, in start order.
	for _, srcIdx := range e.toolOrder {
		ti := e.toolItems[srcIdx]
		if ti == nil || ti.Done {
			continue
		}
		if err := e.emit(&IRStreamEvent{
			Type: "response.function_call_arguments.done",
			Choice: &IRChoice{
				Index: ti.OutputIndex,
				Message: &IRMessage{
					ToolCalls: []IRToolCall{{
						ID:        ti.CallID,
						Name:      ti.Name,
						Arguments: ti.Arguments,
					}},
				},
			},
		}); err != nil {
			return err
		}
		if err := e.emit(&IRStreamEvent{
			Type: "response.output_item.done",
			Choice: &IRChoice{
				Index: ti.OutputIndex,
				Message: &IRMessage{
					ToolCalls: []IRToolCall{{
						ID:        ti.CallID,
						Name:      ti.Name,
						Arguments: ti.Arguments,
					}},
				},
			},
		}); err != nil {
			return err
		}
		ti.Done = true
	}
	return nil
}

func (e *responsesEmitter) onUsage(u *IRUsage) error {
	if u == nil {
		return nil
	}
	// P1-3: store usage so response.completed carries it.
	if e.usage == nil {
		e.usage = &IRUsage{}
	}
	mergeUsageInto(e.usage, u)
	return nil
}

func (e *responsesEmitter) onComplete(reason string) error {
	// P1-5: onFinish normalizes the reason internally; pass it raw.
	if !e.finished {
		if err := e.onFinish(reason); err != nil {
			return err
		}
	}
	mapped := mapFinishReasonToResponses(reason)
	resp := &IRResponse{ID: e.id, Model: e.model}
	if e.usage != nil {
		resp.Usage = e.usage
	}
	// P0-1: emit the correct terminal event based on the mapped reason.
	// - "incomplete" (from upstream "length" / response.incomplete) must
	//   emit response.incomplete, NOT response.completed, and must NOT
	//   emit [DONE] (the stream is terminated by the incomplete event
	//   itself).
	// - "failed" should not reach here (it is routed to onError by
	//   StreamConvertIncremental), but handle it defensively.
	// - everything else (including "completed") emits response.completed
	//   followed by [DONE] (current behavior).
	eventType := "response.completed"
	switch mapped {
	case "incomplete":
		eventType = "response.incomplete"
	case "failed":
		eventType = "response.failed"
	}
	if err := e.emit(&IRStreamEvent{
		Type:     eventType,
		Response: resp,
	}); err != nil {
		return err
	}
	// Only emit [DONE] for a fully successful completion. Incomplete and
	// failed terminal events end the stream on their own; emitting [DONE]
	// would make them look like a successful completion to some clients.
	if eventType == "response.completed" {
		if err := e.enc.WriteDone(); err != nil {
			return err
		}
	}
	if e.flush != nil {
		e.flush()
	}
	return nil
}

func (e *responsesEmitter) onError(err error) error {
	// P0-3: do NOT emit response.completed/[DONE] (success terminal events).
	// The stream is truncated — the client detects the failure by the
	// absence of response.completed.
	if e.flush != nil {
		e.flush()
	}
	return nil
}

// --------------------------------------------------------------------------
// Finish reason normalization for streaming (P1-5).
//
// Stream emitters previously output the raw upstream finish reason without
// mapping it to the target protocol's vocabulary. These helpers normalize the
// reason so that, e.g., a Messages "end_turn" arriving at a Chat client
// becomes "stop" instead of the Anthropic-specific "end_turn".
// --------------------------------------------------------------------------

// mapFinishReasonToChat maps any upstream finish reason to the OpenAI Chat
// Completions vocabulary.
func mapFinishReasonToChat(reason string) string {
	switch reason {
	case "end_turn", "stop_sequence":
		return "stop"
	case "tool_use":
		return "tool_calls"
	case "max_tokens":
		return "length"
	default:
		return reason
	}
}

// mapFinishReasonToMessages maps any upstream finish reason to the Anthropic
// Messages vocabulary.
func mapFinishReasonToMessages(reason string) string {
	switch reason {
	case "stop":
		return "end_turn"
	case "tool_calls":
		return "tool_use"
	case "length":
		return "max_tokens"
	default:
		return reason
	}
}

// mapFinishReasonToResponses maps any upstream finish reason to the OpenAI
// Responses vocabulary.
func mapFinishReasonToResponses(reason string) string {
	switch reason {
	case "stop":
		return "completed"
	case "length":
		return "incomplete"
	case "tool_calls":
		return "tool_calls"
	default:
		return reason
	}
}
