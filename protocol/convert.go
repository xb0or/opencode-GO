package protocol

import (
	"fmt"
	"io"

	"github.com/opencode-sw/gateway/config"
)

// ──────────────────────── Request conversion ────────────────────────

// ConvertRequest decodes a request body in the source protocol format and
// re-encodes it into the target protocol format.  It returns the converted
// body ready to send upstream.
func ConvertRequest(from, to config.Protocol, data []byte) ([]byte, error) {
	if from == to {
		return data, nil
	}
	ir, err := decodeRequest(from, data)
	if err != nil {
		return nil, fmt.Errorf("convert request: %w", err)
	}
	out, err := encodeRequest(to, ir)
	if err != nil {
		return nil, fmt.Errorf("convert request: %w", err)
	}
	return out, nil
}

func decodeRequest(proto config.Protocol, data []byte) (*IRRequest, error) {
	switch proto {
	case config.ProtocolChat:
		return DecodeChatRequest(data)
	case config.ProtocolMessages:
		return DecodeMessagesRequest(data)
	case config.ProtocolResponses:
		return DecodeResponsesRequest(data)
	default:
		return nil, fmt.Errorf("unsupported protocol: %s", proto)
	}
}

func encodeRequest(proto config.Protocol, ir *IRRequest) ([]byte, error) {
	switch proto {
	case config.ProtocolChat:
		return EncodeChatRequest(ir)
	case config.ProtocolMessages:
		return EncodeMessagesRequest(ir)
	case config.ProtocolResponses:
		return EncodeResponsesRequest(ir)
	default:
		return nil, fmt.Errorf("unsupported protocol: %s", proto)
	}
}

// ──────────────────────── Response conversion ───────────────────────

// ConvertResponse decodes a response body in the source protocol format and
// re-encodes it into the target protocol format.
func ConvertResponse(from, to config.Protocol, data []byte) ([]byte, error) {
	if from == to {
		return data, nil
	}
	ir, err := decodeResponse(from, data)
	if err != nil {
		return nil, fmt.Errorf("convert response: %w", err)
	}
	out, err := encodeResponse(to, ir)
	if err != nil {
		return nil, fmt.Errorf("convert response: %w", err)
	}
	return out, nil
}

func decodeResponse(proto config.Protocol, data []byte) (*IRResponse, error) {
	switch proto {
	case config.ProtocolChat:
		return DecodeChatResponse(data)
	case config.ProtocolMessages:
		return DecodeMessagesResponse(data)
	case config.ProtocolResponses:
		return DecodeResponsesResponse(data)
	default:
		return nil, fmt.Errorf("unsupported protocol: %s", proto)
	}
}

func encodeResponse(proto config.Protocol, ir *IRResponse) ([]byte, error) {
	switch proto {
	case config.ProtocolChat:
		return EncodeChatResponse(ir)
	case config.ProtocolMessages:
		return EncodeMessagesResponse(ir)
	case config.ProtocolResponses:
		return EncodeResponsesResponse(ir)
	default:
		return nil, fmt.Errorf("unsupported protocol: %s", proto)
	}
}

// ──────────────────────── Streaming conversion ─────────────────────

// StreamConverter reads an SSE stream in the source protocol from src,
// converts each event to the target protocol, and writes it to dst.
// It returns once the source stream ends.
func StreamConverter(dst io.Writer, src io.Reader, from, to config.Protocol) error {
	if from == to {
		// Transparent byte copy for same-protocol passthrough.
		_, err := io.Copy(dst, src)
		return err
	}

	// For cross-protocol streaming we use the IR as intermediate:
	//   src → decodeStreamEvent(from) → IRStreamEvent → encodeStreamEvent(to) → dst
	//
	// However, the three protocols have *very* different streaming semantics:
	//   - Chat:      simple data: {chunk} lines, [DONE] terminator
	//   - Messages:  multi-event sequences (message_start, content_block_*, message_*)
	//   - Responses: multi-event sequences (response.*, response.output_text.delta, etc.)
	//
	// A simple 1:1 event translation would produce invalid output for the target
	// protocol.  Instead, we buffer the full upstream response and re-emit it in
	// the target protocol's native streaming format.

	// Strategy: buffer the full upstream SSE into an IRResponse, then stream
	// it back out in the target protocol format.
	fullResp, err := bufferStream(from, src)
	if err != nil {
		return fmt.Errorf("stream convert: buffer upstream: %w", err)
	}

	// Emit the buffered response as a streaming response in the target protocol.
	return emitStreamResponse(dst, to, fullResp)
}

// bufferStream reads the entire upstream SSE stream into an IRResponse.
func bufferStream(proto config.Protocol, r io.Reader) (*IRResponse, error) {
	switch proto {
	case config.ProtocolChat:
		return DecodeChatSSE(r)
	case config.ProtocolMessages:
		return DecodeMessagesSSE(r)
	case config.ProtocolResponses:
		return DecodeResponsesSSE(r)
	default:
		return nil, fmt.Errorf("unsupported protocol: %s", proto)
	}
}

// emitStreamResponse writes a buffered IRResponse as an SSE stream in the
// target protocol format.
func emitStreamResponse(dst io.Writer, proto config.Protocol, resp *IRResponse) error {
	switch proto {
	case config.ProtocolChat:
		return emitChatStream(dst, resp)
	case config.ProtocolMessages:
		return emitMessagesStream(dst, resp)
	case config.ProtocolResponses:
		return emitResponsesStream(dst, resp)
	default:
		return fmt.Errorf("unsupported protocol: %s", proto)
	}
}

// emitChatStream writes a buffered IRResponse as an OpenAI Chat SSE stream.
func emitChatStream(dst io.Writer, resp *IRResponse) error {
	enc := NewChatStreamEncoder(dst)
	if len(resp.Choices) == 0 {
		return enc.WriteDone()
	}
	ch := resp.Choices[0]
	msg := ch.Message
	if msg == nil {
		return enc.WriteDone()
	}

	// Emit initial chunk with role.
	first := &IRStreamEvent{
		Type:  "chat.completion.chunk",
		Choice: &IRChoice{
			Index: 0,
			Delta: &IRMessage{Role: "assistant"},
		},
	}
	if err := enc.WriteEvent(first); err != nil {
		return err
	}

	// Emit text content.
	if msg.Text != "" {
		ev := &IRStreamEvent{
			Type:        "chat.completion.chunk",
			ContentDelta: msg.Text,
			Choice: &IRChoice{
				Index: 0,
				Delta: &IRMessage{Content: []IRContent{{Type: "text", Text: msg.Text}}},
			},
		}
		if err := enc.WriteEvent(ev); err != nil {
			return err
		}
	}

	// Emit tool calls.
	for _, tc := range msg.ToolCalls {
		// First chunk: tool call ID and name.
		idEv := &IRStreamEvent{
			Type: "chat.completion.chunk",
			Choice: &IRChoice{
				Index: 0,
				Delta: &IRMessage{
					ToolCalls: []IRToolCall{{ID: tc.ID, Name: tc.Name}},
				},
			},
		}
		if err := enc.WriteEvent(idEv); err != nil {
			return err
		}
		// Arguments chunk.
		if tc.Arguments != "" {
			argEv := &IRStreamEvent{
				Type: "chat.completion.chunk",
				Choice: &IRChoice{
					Index: 0,
					Delta: &IRMessage{
						ToolCalls: []IRToolCall{{Arguments: tc.Arguments}},
					},
				},
			}
			if err := enc.WriteEvent(argEv); err != nil {
				return err
			}
		}
	}

	// Final chunk with finish_reason.
	finReason := ch.FinishReason
	if finReason == "" {
		finReason = "stop"
	}
	finEv := &IRStreamEvent{
		Type: "chat.completion.chunk",
		Choice: &IRChoice{
			Index:        0,
			FinishReason: finReason,
		},
	}
	if err := enc.WriteEvent(finEv); err != nil {
		return err
	}
	return enc.WriteDone()
}

// emitMessagesStream writes a buffered IRResponse as an Anthropic Messages SSE stream.
func emitMessagesStream(dst io.Writer, resp *IRResponse) error {
	enc := NewMessagesStreamEncoder(dst)
	msg := resp.Choices[0].Message

	// message_start
	startEv := &IRStreamEvent{
		Type:     "message_start",
		Response: &IRResponse{ID: resp.ID, Model: resp.Model, Usage: resp.Usage},
	}
	if err := enc.WriteEvent(startEv); err != nil {
		return err
	}

	// Determine content blocks.
	hasText := msg.Text != ""
	hasThinking := false
	for _, c := range msg.Content {
		if c.Type == "thinking" {
			hasThinking = true
		}
	}

	idx := 0
	// Thinking block.
	if hasThinking {
		thinkingText := ""
		for _, c := range msg.Content {
			if c.Type == "thinking" {
				thinkingText += c.Text
			}
		}
		startCb := &IRStreamEvent{
			Type:  "content_block_start",
			Choice: &IRChoice{Index: idx, Delta: &IRMessage{Content: []IRContent{{Type: "thinking"}}}},
		}
		if err := enc.WriteEvent(startCb); err != nil {
			return err
		}
		deltaEv := &IRStreamEvent{
			Type:         "content_block_delta",
			ContentDelta: thinkingText,
			Choice: &IRChoice{
				Index: idx,
				Delta: &IRMessage{Content: []IRContent{{Type: "thinking", Text: thinkingText}}},
			},
		}
		if err := enc.WriteEvent(deltaEv); err != nil {
			return err
		}
		stopCb := &IRStreamEvent{Type: "content_block_stop", Choice: &IRChoice{Index: idx}}
		if err := enc.WriteEvent(stopCb); err != nil {
			return err
		}
		idx++
	}

	// Text block.
	if hasText {
		startCb := &IRStreamEvent{
			Type:  "content_block_start",
			Choice: &IRChoice{Index: idx, Delta: &IRMessage{Content: []IRContent{{Type: "text"}}}},
		}
		if err := enc.WriteEvent(startCb); err != nil {
			return err
		}
		deltaEv := &IRStreamEvent{
			Type:         "content_block_delta",
			ContentDelta: msg.Text,
			Choice: &IRChoice{
				Index: idx,
				Delta: &IRMessage{Content: []IRContent{{Type: "text", Text: msg.Text}}},
			},
		}
		if err := enc.WriteEvent(deltaEv); err != nil {
			return err
		}
		stopCb := &IRStreamEvent{Type: "content_block_stop", Choice: &IRChoice{Index: idx}}
		if err := enc.WriteEvent(stopCb); err != nil {
			return err
		}
		idx++
	}

	// Tool use blocks.
	for _, tc := range msg.ToolCalls {
		startCb := &IRStreamEvent{
			Type:   "content_block_start",
			Choice: &IRChoice{Index: idx, Delta: &IRMessage{ToolCalls: []IRToolCall{{ID: tc.ID, Name: tc.Name}}}},
		}
		if err := enc.WriteEvent(startCb); err != nil {
			return err
		}
		if tc.Arguments != "" {
			deltaEv := &IRStreamEvent{
				Type: "content_block_delta",
				ToolCallDelta: &IRToolCallDelta{Index: idx, Arguments: tc.Arguments},
				Choice: &IRChoice{Index: idx},
			}
			if err := enc.WriteEvent(deltaEv); err != nil {
				return err
			}
		}
		stopCb := &IRStreamEvent{Type: "content_block_stop", Choice: &IRChoice{Index: idx}}
		if err := enc.WriteEvent(stopCb); err != nil {
			return err
		}
		idx++
	}

	// message_delta (stop_reason + usage)
	finReason := "end_turn"
	if len(resp.Choices) > 0 && resp.Choices[0].FinishReason != "" {
		finReason = resp.Choices[0].FinishReason
	}
	deltaMsg := &IRStreamEvent{
		Type: "message_delta",
		Choice: &IRChoice{
			Index:        0,
			FinishReason: finReason,
		},
	}
	if resp.Usage != nil {
		deltaMsg.Response = &IRResponse{Usage: &IRUsage{CompletionTokens: resp.Usage.CompletionTokens}}
	}
	if err := enc.WriteEvent(deltaMsg); err != nil {
		return err
	}

	return enc.WriteDone()
}

// emitResponsesStream writes a buffered IRResponse as a Responses API SSE stream.
func emitResponsesStream(dst io.Writer, resp *IRResponse) error {
	enc := NewResponsesStreamEncoder(dst)
	msg := resp.Choices[0].Message

	// response.created
	created := &IRStreamEvent{
		Type:     "response.created",
		Response: &IRResponse{ID: resp.ID, Model: resp.Model},
	}
	if err := enc.WriteEvent(created); err != nil {
		return err
	}

	// response.output_item.added
	itemAdded := &IRStreamEvent{
		Type:   "response.output_item.added",
		Choice: &IRChoice{Index: 0, Delta: &IRMessage{Role: "assistant"}},
	}
	if err := enc.WriteEvent(itemAdded); err != nil {
		return err
	}

	// Text delta.
	if msg.Text != "" {
		deltaEv := &IRStreamEvent{
			Type:         "response.output_text.delta",
			ContentDelta: msg.Text,
			Choice:       &IRChoice{Index: 0},
		}
		if err := enc.WriteEvent(deltaEv); err != nil {
			return err
		}
		doneEv := &IRStreamEvent{
			Type:   "response.output_text.done",
			Choice: &IRChoice{Index: 0},
		}
		if err := enc.WriteEvent(doneEv); err != nil {
			return err
		}
	}

	// Tool calls.
	for _, tc := range msg.ToolCalls {
		// function_call_arguments.delta
		if tc.Arguments != "" {
			deltaEv := &IRStreamEvent{
				Type:         "response.function_call_arguments.delta",
				ContentDelta: tc.Arguments,
				Choice:       &IRChoice{Index: 0},
			}
			if err := enc.WriteEvent(deltaEv); err != nil {
				return err
			}
		}
		doneEv := &IRStreamEvent{
			Type: "response.function_call_arguments.done",
			Choice: &IRChoice{
				Index:   0,
				Message: &IRMessage{ToolCalls: []IRToolCall{{ID: tc.ID, Name: tc.Name, Arguments: tc.Arguments}}},
			},
		}
		if err := enc.WriteEvent(doneEv); err != nil {
			return err
		}
	}

	// response.output_item.done
	finReason := "stop"
	if len(resp.Choices) > 0 && resp.Choices[0].FinishReason != "" {
		finReason = resp.Choices[0].FinishReason
	}
	itemDone := &IRStreamEvent{
		Type:   "response.output_item.done",
		Choice: &IRChoice{Index: 0, Message: msg, FinishReason: finReason},
	}
	if err := enc.WriteEvent(itemDone); err != nil {
		return err
	}

	// response.completed
	completed := &IRStreamEvent{
		Type:     "response.completed",
		Response: resp,
	}
	if err := enc.WriteEvent(completed); err != nil {
		return err
	}

	return enc.WriteDone()
}
