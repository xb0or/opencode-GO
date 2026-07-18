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
// The caller pre-reads the first complete SSE event (for validation / failover)
// and passes it as firstEvent. The rest of the upstream body is passed as
// rest. StreamConvertIncremental feeds firstEvent ++ rest to the upstream
// decoder, translates each decoded IRStreamEvent to the target protocol, and
// writes it to dst, calling flush after each batch.
//
// Failover is only allowed before StreamConvertIncremental is called — once
// the caller commits HTTP 200 + text/event-stream headers and invokes this
// function, the response is committed and subsequent upstream errors cannot
// trigger failover.
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
func StreamConvertIncremental(
	upProto, dstProto string,
	firstEvent []byte,
	rest io.Reader,
	dst io.Writer,
	flush func(),
) (*IRResponse, error) {
	// Combine pre-read first event with the rest of the body.
	combined := io.MultiReader(bytes.NewReader(firstEvent), rest)

	// Accumulated response for usage tracking.
	acc := &IRResponse{}

	// Create the target emitter.
	emitter, err := newTargetEmitter(dstProto, dst, flush)
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

		// Usage (from Response field).
		if ev.Response != nil && ev.Response.Usage != nil {
			mergeUsage(acc, ev.Response.Usage)
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
		_ = emitter.onEnd()
		return acc, decErr
	}

	// Emit terminal events.
	if err := emitter.onEnd(); err != nil {
		return acc, err
	}

	return acc, nil
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

func (r *IRResponse) setFinishReason(reason string) {
	if len(r.Choices) == 0 {
		r.Choices = []IRChoice{{}}
	}
	r.Choices[0].FinishReason = reason
}

// ---------------------------------------------------------------------------
// targetEmitter — protocol-specific state machine for incremental emission.
// ---------------------------------------------------------------------------

type targetEmitter interface {
	onStart(id, model string) error
	onTextDelta(text string) error
	onThinkingDelta(text string) error
	onToolCallStart(idx int, id, name string) error
	onToolCallDelta(idx int, args string) error
	onFinish(reason string) error
	onEnd() error
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

func (e *chatEmitter) onEnd() error {
	if !e.finished {
		// Emit a finish event with a default reason if none was received.
		if err := e.emit(&IRStreamEvent{
			Type: "completion",
			Choice: &IRChoice{
				Index:        0,
				FinishReason: "stop",
			},
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

// ---------------------------------------------------------------------------
// Messages target emitter
// ---------------------------------------------------------------------------

type messagesEmitter struct {
	enc         *MessagesStreamEncoder
	flush       func()
	started     bool
	id          string
	model       string
	currentType string // "text" | "thinking" | "tool" | ""
	blockIdx    int
	toolCalls   map[int]string // idx -> accumulated tool call id for block tracking
	finished    bool
	usage       *IRUsage
}

func (e *messagesEmitter) onStart(id, model string) error {
	e.started = true
	e.id = id
	e.model = model
	e.toolCalls = make(map[int]string)
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
	if e.currentType != "" && e.currentType != "tool" {
		if err := e.closeCurrentBlock(); err != nil {
			return err
		}
	}
	if e.currentType == "" {
		if err := e.openBlock("tool", id, name); err != nil {
			return err
		}
		e.currentType = "tool"
		e.toolCalls[idx] = id
	}
	return nil
}

func (e *messagesEmitter) onToolCallDelta(idx int, args string) error {
	return e.emit(&IRStreamEvent{
		Type:          "content_block_delta",
		ToolCallDelta: &IRToolCallDelta{Index: e.blockIdx, Arguments: args},
		Choice:        &IRChoice{Index: e.blockIdx},
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

func (e *messagesEmitter) onEnd() error {
	if !e.finished {
		if err := e.onFinish("end_turn"); err != nil {
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

// ---------------------------------------------------------------------------
// Responses target emitter
// ---------------------------------------------------------------------------

type responsesEmitter struct {
	enc         *ResponsesStreamEncoder
	flush       func()
	started     bool
	id          string
	model       string
	itemAdded   bool
	partAdded   bool
	finished    bool
	currentType string
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

func (e *responsesEmitter) onTextDelta(text string) error {
	if !e.itemAdded {
		if err := e.emit(&IRStreamEvent{
			Type: "response.output_item.added",
			Choice: &IRChoice{
				Index: 0,
				Delta: &IRMessage{Role: "assistant"},
			},
		}); err != nil {
			return err
		}
		e.itemAdded = true
	}
	if !e.partAdded {
		if err := e.emit(&IRStreamEvent{
			Type: "response.content_part.added",
			Choice: &IRChoice{
				Index: 0,
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
		Choice:       &IRChoice{Index: 0},
	})
}

func (e *responsesEmitter) onThinkingDelta(text string) error {
	if !e.itemAdded {
		if err := e.emit(&IRStreamEvent{
			Type: "response.output_item.added",
			Choice: &IRChoice{
				Index: 0,
				Delta: &IRMessage{Role: "assistant"},
			},
		}); err != nil {
			return err
		}
		e.itemAdded = true
	}
	return e.emit(&IRStreamEvent{
		Type:         "response.reasoning_content.delta",
		ContentDelta: text,
		Choice: &IRChoice{
			Index: 0,
			Delta: &IRMessage{Content: []IRContent{{Type: "thinking", Text: text}}},
		},
	})
}

func (e *responsesEmitter) onToolCallStart(idx int, id, name string) error {
	if !e.itemAdded {
		if err := e.emit(&IRStreamEvent{
			Type: "response.output_item.added",
			Choice: &IRChoice{
				Index: 0,
				Delta: &IRMessage{Role: "assistant"},
			},
		}); err != nil {
			return err
		}
		e.itemAdded = true
	}
	return nil
}

func (e *responsesEmitter) onToolCallDelta(idx int, args string) error {
	return e.emit(&IRStreamEvent{
		Type:         "response.function_call_arguments.delta",
		ContentDelta: args,
		ToolCallDelta: &IRToolCallDelta{
			Index:     0,
			Arguments: args,
		},
		Choice: &IRChoice{Index: 0},
	})
}

func (e *responsesEmitter) onFinish(reason string) error {
	if e.finished {
		return nil
	}
	e.finished = true
	if e.partAdded && e.currentType == "text" {
		if err := e.emit(&IRStreamEvent{
			Type:         "response.output_text.done",
			ContentDelta: "",
			Choice:       &IRChoice{Index: 0},
		}); err != nil {
			return err
		}
		if err := e.emit(&IRStreamEvent{
			Type:         "response.content_part.done",
			ContentDelta: "",
			Choice:       &IRChoice{Index: 0},
		}); err != nil {
			return err
		}
	}
	if e.itemAdded {
		if err := e.emit(&IRStreamEvent{
			Type:   "response.output_item.done",
			Choice: &IRChoice{Index: 0, Message: &IRMessage{Role: "assistant"}},
		}); err != nil {
			return err
		}
	}
	return nil
}

func (e *responsesEmitter) onEnd() error {
	if !e.finished {
		if err := e.onFinish("completed"); err != nil {
			return err
		}
	}
	resp := &IRResponse{ID: e.id, Model: e.model}
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