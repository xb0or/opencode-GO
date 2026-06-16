package protocol

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
)

// MessagesStreamDecoder reads an Anthropic Messages SSE stream and emits
// IRStreamEvents via the callback.  Anthropic SSE lines are of the form:
//
//	event: <type>\n
//	data: <json>\n\n
//
// We ignore the event: line and parse the JSON from data: instead (the JSON
// already contains the "type" field).
func MessagesStreamDecoder(r io.Reader, onEvent func(*IRStreamEvent) error) error {
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
		ev, err := DecodeMessagesStreamEvent(payload)
		if err != nil {
			continue // skip malformed chunks
		}
		if err := onEvent(ev); err != nil {
			return err
		}
	}
	return scanner.Err()
}

// MessagesStreamEncoder writes IRStreamEvents as Anthropic Messages SSE lines.
type MessagesStreamEncoder struct {
	w io.Writer
}

func NewMessagesStreamEncoder(w io.Writer) *MessagesStreamEncoder {
	return &MessagesStreamEncoder{w: w}
}

// WriteEvent serialises an IRStreamEvent into Anthropic SSE wire format.
// The Anthropic protocol expects both an "event:" line and a "data:" line per
// frame; we emit both.
func (e *MessagesStreamEncoder) WriteEvent(ev *IRStreamEvent) error {
	data, err := EncodeMessagesStreamEvent(ev)
	if err != nil {
		return err
	}
	// event: <type>\ndata: <json>\n\n
	var buf bytes.Buffer
	buf.WriteString("event: ")
	buf.WriteString(ev.Type)
	buf.WriteByte('\n')
	buf.WriteString("data: ")
	buf.Write(data)
	buf.WriteString("\n\n")
	_, err = e.w.Write(buf.Bytes())
	return err
}

// WriteDone sends the final message_stop event (Anthropic doesn't have a
// separate [DONE] marker; the message_stop event signals completion).
func (e *MessagesStreamEncoder) WriteDone() error {
	data, _ := json.Marshal(MsgStreamEvent{Type: "message_stop"})
	var buf bytes.Buffer
	buf.WriteString("event: message_stop\ndata: ")
	buf.Write(data)
	buf.WriteString("\n\n")
	_, err := e.w.Write(buf.Bytes())
	return err
}

// DecodeMessagesSSE reads an Anthropic Messages SSE stream into a full
// IRResponse (buffered).  Useful when the gateway needs to collect the whole
// response before converting protocols.
func DecodeMessagesSSE(r io.Reader) (*IRResponse, error) {
	resp := &IRResponse{}
	var msg IRMessage
	var finishReason string

	err := MessagesStreamDecoder(r, func(ev *IRStreamEvent) error {
		switch ev.Type {
		case "message_start":
			if ev.Response != nil {
				resp.ID = ev.Response.ID
				resp.Model = ev.Response.Model
				if ev.Response.Usage != nil {
					resp.Usage = ev.Response.Usage
				}
			}
		case "content_block_start":
			if ev.Choice != nil && ev.Choice.Delta != nil {
				if len(ev.Choice.Delta.ToolCalls) > 0 {
					tc := ev.Choice.Delta.ToolCalls[0]
					msg.ToolCalls = append(msg.ToolCalls, IRToolCall{ID: tc.ID, Name: tc.Name})
				}
			}
		case "content_block_delta":
			if ev.Choice != nil && ev.Choice.Delta != nil {
				if len(ev.Choice.Delta.Content) > 0 {
					c := ev.Choice.Delta.Content[0]
					if c.Type == "text" {
						msg.Text += c.Text
						if len(msg.Content) == 0 {
							msg.Content = []IRContent{{Type: "text"}}
						}
						msg.Content[0].Text += c.Text
					} else if c.Type == "thinking" {
						// Preserve thinking blocks.
						found := false
						for i := range msg.Content {
							if msg.Content[i].Type == "thinking" {
								msg.Content[i].Text += c.Text
								found = true
								break
							}
						}
						if !found {
							msg.Content = append(msg.Content, IRContent{Type: "thinking", Text: c.Text})
						}
					}
				}
				if ev.ToolCallDelta != nil {
					if len(msg.ToolCalls) == 0 {
						msg.ToolCalls = append(msg.ToolCalls, IRToolCall{})
					}
					msg.ToolCalls[0].Arguments += ev.ToolCallDelta.Arguments
				}
			}
		case "message_delta":
			if ev.Choice != nil && ev.Choice.FinishReason != "" {
				finishReason = ev.Choice.FinishReason
			}
			if ev.Response != nil && ev.Response.Usage != nil {
				if resp.Usage == nil {
					resp.Usage = &IRUsage{}
				}
				resp.Usage.CompletionTokens = ev.Response.Usage.CompletionTokens
			}
		case "message_stop":
			if ev.Response != nil && ev.Response.Usage != nil {
				resp.Usage = ev.Response.Usage
			}
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
