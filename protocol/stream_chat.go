package protocol

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
)

// ChatStreamDecoder reads an OpenAI Chat Completions SSE stream and emits
// IRStreamEvents via the callback.
//
// P0-1: tracks whether a terminal event was seen ([DONE] or a chunk with a
// non-empty finish_reason). If EOF is reached without a terminal event, the
// decoder returns io.ErrUnexpectedEOF so the caller does NOT call onComplete
// (which would synthesize a fake-success finish_reason=stop + [DONE]).
func ChatStreamDecoder(r io.Reader, onEvent func(*IRStreamEvent) error) error {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 256*1024), 256*1024)
	terminalSeen := false
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		if !bytes.HasPrefix(line, []byte("data: ")) {
			continue
		}
		payload := bytes.TrimSpace(line[6:])
		if bytes.Equal(payload, []byte("[DONE]")) {
			terminalSeen = true
			return nil
		}
		ev, err := DecodeChatStreamChunk(payload)
		if err != nil {
			// P0-2/P0-3: a malformed data: payload is a decoder error, not a
			// silently-skippable line. Returning the error lets the caller
			// (StreamConvertIncremental) invoke onError and avoid emitting a
			// success terminal event, so the client does not mistake a broken
			// upstream for a completed one.
			return fmt.Errorf("chat stream: malformed data payload: %w", err)
		}
		if err := onEvent(ev); err != nil {
			return err
		}
		if ev.Choice != nil && ev.Choice.FinishReason != "" {
			terminalSeen = true
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	// P0-1: a clean EOF without a terminal event means upstream closed the
	// connection mid-stream. Surface it as an unexpected EOF so the caller
	// routes to onError (no fake-success terminal events).
	if !terminalSeen {
		return io.ErrUnexpectedEOF
	}
	return nil
}

// ChatStreamEncoder writes IRStreamEvents as OpenAI Chat Completions SSE lines.
type ChatStreamEncoder struct {
	w io.Writer
}

func NewChatStreamEncoder(w io.Writer) *ChatStreamEncoder {
	return &ChatStreamEncoder{w: w}
}

func (e *ChatStreamEncoder) WriteEvent(ev *IRStreamEvent) error {
	data, err := EncodeChatStreamChunk(ev)
	if err != nil {
		return err
	}
	_, err = e.w.Write(append(append([]byte("data: "), data...), '\n', '\n'))
	return err
}

func (e *ChatStreamEncoder) WriteDone() error {
	_, err := e.w.Write([]byte("data: [DONE]\n\n"))
	return err
}

// DecodeChatSSE reads an OpenAI Chat SSE stream into a full IRResponse (buffered).
func DecodeChatSSE(r io.Reader) (*IRResponse, error) {
	resp := &IRResponse{}
	var msg IRMessage
	var finishReason string

	err := ChatStreamDecoder(r, func(ev *IRStreamEvent) error {
		if ev.Response != nil && ev.Response.Usage != nil {
			resp.Usage = ev.Response.Usage
		}
		if ev.Choice == nil {
			return nil
		}
		ch := ev.Choice
		if ch.Delta != nil {
			if ch.Delta.Text != "" {
				msg.Text += ch.Delta.Text
				msg.Content = appendTextContent(msg.Content, ch.Delta.Text)
			}
			for _, c := range ch.Delta.Content {
				switch c.Type {
				case "thinking":
					msg.Content = appendThinkingContent(msg.Content, c.Text)
				case "text":
					if c.Text != "" && ch.Delta.Text == "" {
						msg.Text += c.Text
						msg.Content = appendTextContent(msg.Content, c.Text)
					}
				}
			}
			if len(ch.Delta.ToolCalls) > 0 {
				tc := ch.Delta.ToolCalls[0]
				idx := tc.Index
				for len(msg.ToolCalls) <= idx {
					msg.ToolCalls = append(msg.ToolCalls, IRToolCall{})
				}
				if tc.ID != "" {
					msg.ToolCalls[idx].ID = tc.ID
				}
				if tc.Name != "" {
					msg.ToolCalls[idx].Name = tc.Name
				}
				if tc.Arguments != "" {
					msg.ToolCalls[idx].Arguments += tc.Arguments
				}
			}
		}
		if ch.FinishReason != "" {
			finishReason = ch.FinishReason
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	msg.Role = "assistant"
	msg.ToolCalls = cleanIRToolCalls(msg.ToolCalls)
	resp.Choices = []IRChoice{{Index: 0, Message: &msg, FinishReason: finishReason}}
	return resp, nil
}
