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
		// Extract metadata on first event.
		if !emitterStarted {
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
				if tc.Name != "" || tc.ID != "" {
					if err := emitter.onToolCallStart(tc.Index, tc.ID, tc.Name); err != nil {
						return err
					}
				}
				if tc.Arguments != "" {
					if err := emitter.onToolCallDelta(tc.Index, tc.Arguments); err != nil {
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

		// Finish reason.
		if ev.Choice != nil && ev.Choice.FinishReason != "" {
			acc.setFinishReason(ev.Choice.FinishReason)
			if err := emitter.onFinish(ev.Choice.FinishReason); err != nil {
				return err
			}
		}

		// Usage (from Response field). Merge into acc and forward to emitter
		// so the client receives usage information (P1-3).
		if ev.Response != nil && ev.Response.Usage != nil {
			mergeUsage(acc, ev.Response.Usage)
			if err := emitter.onUsage(ev.Response.Usage); err != nil {
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
		// P0-3: a decoder error must NOT emit success terminal events.
		// If no valid target event has been emitted yet (onFirstEvent not
		// triggered), do NOT call onError either — any bytes written by
		// onError (e.g. [DONE]) would flow through firstEventWriter and
		// trigger onFirstEvent, committing HTTP 200 to the client for a
		// stream that never produced a valid event. Only call onError if
		// the response is already committed.
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
	return e.emit(&IRStreamEvent{
		Type: "completion",
		Choice: &IRChoice{
			Index:        0,
			FinishReason: reason,
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
	if reason == "" {
		reason = "stop"
	}
	if !e.finished {
		// Emit a finish event with a default reason if none was received.
		if err := e.emit(&IRStreamEvent{
			Type: "completion",
			Choice: &IRChoice{
				Index:        0,
				FinishReason: reason,
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
// multiple tool calls do not collide on a single currentType/blockIdx.
type toolBlockState struct {
	SourceIndex int    // upstream tool index
	BlockIndex  int    // Messages content_block index
	ID          string // tool call id
	Name        string // tool call name
	Started     bool   // content_block_start emitted
}

type messagesEmitter struct {
	enc            *MessagesStreamEncoder
	flush          func()
	started        bool
	id             string
	model          string
	currentType    string // "text" | "thinking" | "tool" | ""
	currentToolIdx int    // source index of the currently-open tool block (-1 if none)
	blockIdx       int    // index of the currently-open block
	toolBlocks     map[int]*toolBlockState
	finished       bool
	usage          *IRUsage
}

func (e *messagesEmitter) onStart(id, model string) error {
	e.started = true
	e.id = id
	e.model = model
	e.toolBlocks = make(map[int]*toolBlockState)
	e.currentToolIdx = -1
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
	e.currentToolIdx = -1
	e.blockIdx++
	return nil
}

func (e *messagesEmitter) openBlock(blockType string, tcID, tcName string) error {
	delta := &IRMessage{}
	if blockType == "tool" {
		delta.ToolCalls = []IRToolCall{{ID: tcID, Name: tcName}}
	} else {
		delta.Content = []IRContent{{Type: blockType}}
	}
	startEv := &IRStreamEvent{
		Type:   "content_block_start",
		Choice: &IRChoice{Index: e.blockIdx, Delta: delta},
	}
	return e.emit(startEv)
}

func (e *messagesEmitter) onTextDelta(text string) error {
	if e.currentType != "" && e.currentType != "text" {
		if err := e.closeCurrentBlock(); err != nil {
			return err
		}
	}
	if e.currentType == "" {
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
	// P1-1: each upstream tool index gets its own Messages content block.
	// If this tool already has an open block that is currently active, nothing
	// to do (id/name may already have been emitted on the initial start).
	if st, ok := e.toolBlocks[idx]; ok && e.currentType == "tool" && e.currentToolIdx == idx {
		// Update id/name if newly provided (defensive).
		if id != "" {
			st.ID = id
		}
		if name != "" {
			st.Name = name
		}
		return nil
	}
	// Close any open block (text, thinking, or a different tool) before opening
	// a new tool block.
	if e.currentType != "" {
		if err := e.closeCurrentBlock(); err != nil {
			return err
		}
	}
	if err := e.openBlock("tool", id, name); err != nil {
		return err
	}
	e.currentType = "tool"
	e.currentToolIdx = idx
	e.toolBlocks[idx] = &toolBlockState{
		SourceIndex: idx,
		BlockIndex:  e.blockIdx,
		ID:          id,
		Name:        name,
		Started:     true,
	}
	return nil
}

func (e *messagesEmitter) onToolCallDelta(idx int, args string) error {
	// P1-1: route the delta to the correct content block for this tool index.
	st, ok := e.toolBlocks[idx]
	if !ok {
		// No start was emitted for this tool; open one opportunistically so the
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
	// Close any open content block first.
	if err := e.closeCurrentBlock(); err != nil {
		return err
	}
	e.finished = true
	finReason := reason
	if finReason == "" {
		finReason = "end_turn"
	}
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
	if reason == "" {
		reason = "end_turn"
	}
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
	_ = reason // responses uses "completed" status implicitly
	if !e.finished {
		if err := e.onFinish("completed"); err != nil {
			return err
		}
	}
	resp := &IRResponse{ID: e.id, Model: e.model}
	if e.usage != nil {
		resp.Usage = e.usage
	}
	if err := e.emit(&IRStreamEvent{
		Type:     "response.completed",
		Response: resp,
	}); err != nil {
		return err
	}
	if err := e.enc.WriteDone(); err != nil {
		return err
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
