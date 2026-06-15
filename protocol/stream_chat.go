package protocol

import (
	"bufio"
	"bytes"
	"io"
)

// ChatStreamDecoder reads an OpenAI Chat Completions SSE stream and emits
// IRStreamEvents via the callback.
func ChatStreamDecoder(r io.Reader, onEvent func(*IRStreamEvent) error) error {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 256*1024), 256*1024)
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
			return nil
		}
		ev, err := DecodeChatStreamChunk(payload)
		if err != nil {
			continue // skip malformed chunks
		}
		if err := onEvent(ev); err != nil {
			return err
		}
	}
	return scanner.Err()
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
		if ev.Choice == nil {
			return nil
		}
		ch := ev.Choice
		if ch.Delta != nil {
			if ch.Delta.Text != "" {
				msg.Text += ch.Delta.Text
				if len(msg.Content) == 0 {
					msg.Content = []IRContent{{Type: "text"}}
				}
				msg.Content[0].Text += ch.Delta.Text
			}
			if len(ch.Delta.ToolCalls) > 0 {
				tc := ch.Delta.ToolCalls[0]
				if len(msg.ToolCalls) == 0 {
					msg.ToolCalls = append(msg.ToolCalls, IRToolCall{ID: tc.ID, Name: tc.Name, Arguments: tc.Arguments})
				} else {
					msg.ToolCalls[0].Arguments += tc.Arguments
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
	resp.Choices = []IRChoice{{Index: 0, Message: &msg, FinishReason: finishReason}}
	return resp, nil
}
