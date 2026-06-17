package protocol

import (
	"bufio"
	"bytes"
	"io"
)

// ResponsesStreamDecoder reads an OpenAI Responses API SSE stream and emits
// IRStreamEvents via the callback.  Responses SSE lines use the same
// data: <json>\n\n format as other OpenAI protocols.
func ResponsesStreamDecoder(r io.Reader, onEvent func(*IRStreamEvent) error) error {
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
		if len(payload) == 0 {
			continue
		}
		ev, err := DecodeResponsesStreamEvent(payload)
		if err != nil {
			continue // skip malformed chunks
		}
		if err := onEvent(ev); err != nil {
			return err
		}
	}
	return scanner.Err()
}

// ResponsesStreamEncoder writes IRStreamEvents as Responses SSE lines.
type ResponsesStreamEncoder struct {
	w io.Writer
}

func NewResponsesStreamEncoder(w io.Writer) *ResponsesStreamEncoder {
	return &ResponsesStreamEncoder{w: w}
}

func (e *ResponsesStreamEncoder) WriteEvent(ev *IRStreamEvent) error {
	data, err := EncodeResponsesStreamEvent(ev)
	if err != nil {
		return err
	}
	_, err = e.w.Write(append(append([]byte("data: "), data...), '\n', '\n'))
	return err
}

func (e *ResponsesStreamEncoder) WriteDone() error {
	_, err := e.w.Write([]byte("data: [DONE]\n\n"))
	return err
}

// DecodeResponsesSSE reads a Responses API SSE stream into a full IRResponse.
func DecodeResponsesSSE(r io.Reader) (*IRResponse, error) {
	resp := &IRResponse{}
	var msg IRMessage
	var finishReason string
	var toolCalls []IRToolCall

	err := ResponsesStreamDecoder(r, func(ev *IRStreamEvent) error {
		switch ev.Type {
		case "response.created":
			if ev.Response != nil {
				resp.ID = ev.Response.ID
				resp.Model = ev.Response.Model
			}
		case "response.output_text.delta":
			msg.Text += ev.ContentDelta
			msg.Content = appendTextContent(msg.Content, ev.ContentDelta)
		case "response.reasoning_text.delta", "response.reasoning.delta", "response.reasoning_content.delta":
			msg.Content = appendThinkingContentBlock(msg.Content, ev.ContentDelta)
		case "response.function_call_arguments.delta":
			if len(toolCalls) == 0 {
				toolCalls = append(toolCalls, IRToolCall{})
			}
			idx := 0
			if ev.ToolCallDelta != nil {
				idx = ev.ToolCallDelta.Index
			}
			for len(toolCalls) <= idx {
				toolCalls = append(toolCalls, IRToolCall{})
			}
			toolCalls[idx].Arguments += ev.ContentDelta
		case "response.output_item.done":
			if ev.Choice != nil && ev.Choice.Message != nil {
				if text, ok := thinkingTextAndPresence(*ev.Choice.Message); ok {
					msg.Content = appendThinkingContentBlock(msg.Content, text)
				}
				if len(ev.Choice.Message.ToolCalls) > 0 {
					tc := ev.Choice.Message.ToolCalls[0]
					if len(toolCalls) == 0 {
						toolCalls = append(toolCalls, tc)
					} else {
						toolCalls[0].ID = tc.ID
						toolCalls[0].Name = tc.Name
						if toolCalls[0].Arguments == "" {
							toolCalls[0].Arguments = tc.Arguments
						}
					}
				}
			}
			if ev.Choice != nil && ev.Choice.FinishReason != "" {
				finishReason = ev.Choice.FinishReason
			}
		case "response.completed":
			if ev.Response != nil {
				if ev.Response.Usage != nil {
					resp.Usage = ev.Response.Usage
				}
			}
		case "response.incomplete":
			finishReason = "length"
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	msg.Role = "assistant"
	msg.ToolCalls = toolCalls
	resp.Choices = []IRChoice{{Index: 0, Message: &msg, FinishReason: finishReason}}
	return resp, nil
}
