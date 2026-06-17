package protocol

import (
	"encoding/json"
	"fmt"
	"strings"
)

// ──────────────────────── Anthropic Messages ─────────────────────────

// MsgRequest is the wire format for POST /v1/messages.
type MsgRequest struct {
	Model         string          `json:"model"`
	MaxTokens     int             `json:"max_tokens"`
	System        any             `json:"system,omitempty"` // string | []MsgSystemBlock
	Messages      []MsgMessage    `json:"messages"`
	Temperature   *float64        `json:"temperature,omitempty"`
	Stream        bool            `json:"stream,omitempty"`
	Tools         []MsgTool       `json:"tools,omitempty"`
	ToolChoice    json.RawMessage `json:"tool_choice,omitempty"`
	TopP          *float64        `json:"top_p,omitempty"`
	StopSequences []string        `json:"stop_sequences,omitempty"`
}

type MsgSystemBlock struct {
	Type string `json:"type"` // text
	Text string `json:"text"`
}

type MsgMessage struct {
	Role    string          `json:"role"` // user | assistant
	Content json.RawMessage `json:"content"`
}

type MsgContent struct {
	Type      string          `json:"type"` // text | image | tool_use | tool_result
	Text      string          `json:"text,omitempty"`
	Source    *MsgImageSource `json:"source,omitempty"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   any             `json:"content,omitempty"` // string | []MsgContent for tool_result
	IsError   bool            `json:"is_error,omitempty"`
	Thinking  string          `json:"thinking,omitempty"`
}

type MsgImageSource struct {
	Type      string `json:"type"` // base64
	MediaType string `json:"media_type,omitempty"`
	Data      string `json:"data,omitempty"`
	URL       string `json:"url,omitempty"`
}

type MsgTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema,omitempty"`
}

// MsgResponse is the wire format for a non-streaming response.
type MsgResponse struct {
	ID           string       `json:"id"`
	Type         string       `json:"type"` // message
	Role         string       `json:"role"`
	Content      []MsgContent `json:"content"`
	Model        string       `json:"model"`
	StopReason   string       `json:"stop_reason,omitempty"`
	StopSequence string       `json:"stop_sequence,omitempty"`
	Usage        *MsgUsage    `json:"usage,omitempty"`
}

type MsgUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

// MsgStreamEvent is one SSE event for streaming Anthropic Messages.
type MsgStreamEvent struct {
	Type         string       `json:"type"`
	Index        int          `json:"index,omitempty"`
	Delta        *MsgDelta    `json:"delta,omitempty"`
	ContentBlock *MsgContent  `json:"content_block,omitempty"`
	Message      *MsgResponse `json:"message,omitempty"`
	Usage        *MsgUsage    `json:"usage,omitempty"`
}

type MsgDelta struct {
	Type        string `json:"type,omitempty"` // text_delta | input_json_delta | thinking_delta
	Text        string `json:"text,omitempty"`
	PartialJSON string `json:"partial_json,omitempty"`
	Thinking    string `json:"thinking,omitempty"`
	StopReason  string `json:"stop_reason,omitempty"`
}

// ──────────────────────── Decoders ──────────────────────────────────

// DecodeMessagesRequest parses an Anthropic Messages request body into IR.
func DecodeMessagesRequest(data []byte) (*IRRequest, error) {
	var req MsgRequest
	if err := json.Unmarshal(data, &req); err != nil {
		return nil, fmt.Errorf("messages: decode request: %w", err)
	}
	ir := &IRRequest{
		Model:       req.Model,
		Temperature: req.Temperature,
		MaxTokens:   req.MaxTokens,
		Stream:      req.Stream,
		ToolChoice:  req.ToolChoice,
		TopP:        req.TopP,
		Stop:        req.StopSequences,
	}
	// Parse system field.
	switch v := req.System.(type) {
	case string:
		ir.System = v
	case []any:
		for _, item := range v {
			b, _ := json.Marshal(item)
			var sb MsgSystemBlock
			if json.Unmarshal(b, &sb) == nil && sb.Text != "" {
				if ir.System != "" {
					ir.System += "\n"
				}
				ir.System += sb.Text
			}
		}
	}
	for _, m := range req.Messages {
		ir.Messages = append(ir.Messages, msgMsgToIR(m))
	}
	for _, t := range req.Tools {
		ir.Tools = append(ir.Tools, IRTool{
			Name:        t.Name,
			Description: t.Description,
			Parameters:  t.InputSchema,
		})
	}
	return ir, nil
}

// EncodeMessagesRequest serializes an IR request into Anthropic Messages wire format.
func EncodeMessagesRequest(ir *IRRequest) ([]byte, error) {
	req := MsgRequest{
		Model:         ir.Model,
		MaxTokens:     ir.MaxTokens,
		Temperature:   ir.Temperature,
		Stream:        ir.Stream,
		ToolChoice:    normalizeToolChoiceForMessages(ir.ToolChoice),
		TopP:          ir.TopP,
		StopSequences: ir.Stop,
	}
	if ir.System != "" {
		req.System = ir.System
	}
	// Anthropic only allows user/assistant in messages; system is separate.
	for _, m := range ir.Messages {
		if m.Role == "system" {
			// Fold system messages into the system field.
			if req.System == nil {
				req.System = m.Text
			} else {
				existing, _ := req.System.(string)
				if existing != "" {
					existing += "\n"
				}
				existing += m.Text
				req.System = existing
			}
			continue
		}
		req.Messages = append(req.Messages, irMsgToMsg(m))
	}
	for _, t := range ir.Tools {
		req.Tools = append(req.Tools, MsgTool{
			Name:        t.Name,
			Description: t.Description,
			InputSchema: t.Parameters,
		})
	}
	return json.Marshal(req)
}

// DecodeMessagesResponse parses an Anthropic Messages response into IR.
func DecodeMessagesResponse(data []byte) (*IRResponse, error) {
	var resp MsgResponse
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, fmt.Errorf("messages: decode response: %w", err)
	}
	ir := &IRResponse{
		ID:    resp.ID,
		Model: resp.Model,
	}
	if resp.Usage != nil {
		ir.Usage = &IRUsage{
			PromptTokens:     resp.Usage.InputTokens,
			CompletionTokens: resp.Usage.OutputTokens,
			TotalTokens:      resp.Usage.InputTokens + resp.Usage.OutputTokens,
		}
	}
	msg := msgContentToIRMessage(resp.Content)
	msg.Role = resp.Role
	ir.Choices = append(ir.Choices, IRChoice{
		Index:        0,
		Message:      &msg,
		FinishReason: resp.StopReason,
	})
	return ir, nil
}

// EncodeMessagesResponse serializes an IR response into Anthropic Messages wire format.
func EncodeMessagesResponse(ir *IRResponse) ([]byte, error) {
	resp := MsgResponse{
		ID:    ir.ID,
		Type:  "message",
		Role:  "assistant",
		Model: ir.Model,
	}
	if ir.Usage != nil {
		resp.Usage = &MsgUsage{
			InputTokens:  ir.Usage.PromptTokens,
			OutputTokens: ir.Usage.CompletionTokens,
		}
	}
	if len(ir.Choices) > 0 {
		ch := ir.Choices[0]
		resp.StopReason = normalizeMessagesStopReason(ch.FinishReason)
		if ch.Message != nil {
			resp.Role = ch.Message.Role
			resp.Content = irMsgToMsgContent(*ch.Message)
		}
	}
	return json.Marshal(resp)
}

// normalizeMessagesStopReason maps IR finish reasons to Anthropic stop reasons.
func normalizeMessagesStopReason(reason string) string {
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

// DecodeMessagesStreamEvent parses an Anthropic SSE event into an IRStreamEvent.
func DecodeMessagesStreamEvent(data []byte) (*IRStreamEvent, error) {
	var ev MsgStreamEvent
	if err := json.Unmarshal(data, &ev); err != nil {
		return nil, fmt.Errorf("messages: decode stream event: %w", err)
	}
	ir := &IRStreamEvent{Type: ev.Type}
	switch ev.Type {
	case "message_start":
		if ev.Message != nil {
			ir.Response = &IRResponse{ID: ev.Message.ID, Model: ev.Message.Model}
			if ev.Message.Usage != nil {
				ir.Response.Usage = &IRUsage{
					PromptTokens:     ev.Message.Usage.InputTokens,
					CompletionTokens: ev.Message.Usage.OutputTokens,
					TotalTokens:      ev.Message.Usage.InputTokens + ev.Message.Usage.OutputTokens,
				}
			}
		}
	case "content_block_start":
		if ev.ContentBlock != nil {
			ir.Choice = &IRChoice{Index: ev.Index}
			ir.Choice.Delta = &IRMessage{Role: "assistant"}
			if ev.ContentBlock.Type == "text" {
				ir.Choice.Delta.Content = []IRContent{{Type: "text"}}
			} else if ev.ContentBlock.Type == "thinking" {
				ir.Choice.Delta.Content = []IRContent{{Type: "thinking"}}
			} else if ev.ContentBlock.Type == "tool_use" {
				ir.Choice.Delta.ToolCalls = []IRToolCall{{ID: ev.ContentBlock.ID, Name: ev.ContentBlock.Name}}
			}
		}
	case "content_block_delta":
		ir.Choice = &IRChoice{Index: ev.Index, Delta: &IRMessage{Role: "assistant"}}
		if ev.Delta != nil {
			switch ev.Delta.Type {
			case "text_delta":
				ir.ContentDelta = ev.Delta.Text
				ir.Choice.Delta.Content = []IRContent{{Type: "text", Text: ev.Delta.Text}}
			case "thinking_delta":
				ir.ContentDelta = ev.Delta.Thinking
				ir.Choice.Delta.Content = []IRContent{{Type: "thinking", Text: ev.Delta.Thinking}}
			case "input_json_delta":
				ir.ToolCallDelta = &IRToolCallDelta{Index: ev.Index, Arguments: ev.Delta.PartialJSON}
			}
		}
	case "content_block_stop":
		// No-op; boundaries handled by encoder.
	case "message_delta":
		ir.Choice = &IRChoice{Index: 0, Delta: &IRMessage{Role: "assistant"}}
		if ev.Delta != nil && ev.Delta.StopReason != "" {
			ir.Choice.FinishReason = ev.Delta.StopReason
		}
		if ev.Usage != nil {
			ir.Response = &IRResponse{Usage: &IRUsage{
				PromptTokens:     ev.Usage.InputTokens,
				CompletionTokens: ev.Usage.OutputTokens,
				TotalTokens:      ev.Usage.InputTokens + ev.Usage.OutputTokens,
			}}
		}
	case "message_stop":
		ir.Choice = &IRChoice{Index: 0, FinishReason: "stop"}
		if ev.Usage != nil {
			ir.Response = &IRResponse{Usage: &IRUsage{
				PromptTokens:     ev.Usage.InputTokens,
				CompletionTokens: ev.Usage.OutputTokens,
				TotalTokens:      ev.Usage.InputTokens + ev.Usage.OutputTokens,
			}}
		}
	}
	return ir, nil
}

// EncodeMessagesStreamEvent serializes an IRStreamEvent into Anthropic SSE events.
// It may return multiple JSON objects (one per line) because the Anthropic protocol
// uses multiple event types for a single conceptual update.
func EncodeMessagesStreamEvent(ev *IRStreamEvent) ([]byte, error) {
	switch ev.Type {
	case "message_start":
		var msg *MsgResponse
		if ev.Response != nil {
			msg = &MsgResponse{ID: ev.Response.ID, Type: "message", Role: "assistant", Model: ev.Response.Model}
		}
		return json.Marshal(MsgStreamEvent{Type: "message_start", Message: msg})

	case "content_block_start":
		cb := &MsgContent{Type: "text"}
		if ev.Choice != nil && ev.Choice.Delta != nil && len(ev.Choice.Delta.ToolCalls) > 0 {
			tc := ev.Choice.Delta.ToolCalls[0]
			cb = &MsgContent{Type: "tool_use", ID: tc.ID, Name: tc.Name}
		}
		idx := 0
		if ev.Choice != nil {
			idx = ev.Choice.Index
		}
		return json.Marshal(MsgStreamEvent{Type: "content_block_start", Index: idx, ContentBlock: cb})

	case "content_block_delta":
		idx := 0
		if ev.Choice != nil {
			idx = ev.Choice.Index
		}
		delta := &MsgDelta{Type: "text_delta", Text: ev.ContentDelta}
		if ev.ToolCallDelta != nil {
			delta = &MsgDelta{Type: "input_json_delta", PartialJSON: ev.ToolCallDelta.Arguments}
		}
		if ev.Choice != nil && ev.Choice.Delta != nil && len(ev.Choice.Delta.Content) > 0 {
			if ev.Choice.Delta.Content[0].Type == "thinking" {
				delta = &MsgDelta{Type: "thinking_delta", Thinking: ev.ContentDelta}
			}
		}
		return json.Marshal(MsgStreamEvent{Type: "content_block_delta", Index: idx, Delta: delta})

	case "content_block_stop":
		idx := 0
		if ev.Choice != nil {
			idx = ev.Choice.Index
		}
		return json.Marshal(MsgStreamEvent{Type: "content_block_stop", Index: idx})

	case "message_delta":
		delta := &MsgDelta{Type: "text_delta"}
		if ev.Choice != nil {
			delta.StopReason = ev.Choice.FinishReason
		}
		var usage *MsgUsage
		if ev.Response != nil && ev.Response.Usage != nil {
			usage = &MsgUsage{OutputTokens: ev.Response.Usage.CompletionTokens}
		}
		return json.Marshal(MsgStreamEvent{Type: "message_delta", Delta: delta, Usage: usage})

	case "message_stop":
		return json.Marshal(MsgStreamEvent{Type: "message_stop"})
	}
	return json.Marshal(MsgStreamEvent{Type: ev.Type})
}

// ──────────────────────── helpers ───────────────────────────────────

// parseMsgMessageContent parses a MsgMessage content field which may be a
// string shorthand or an array of content blocks.
func parseMsgMessageContent(raw json.RawMessage) ([]MsgContent, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return []MsgContent{{Type: "text", Text: s}}, nil
	}
	var blocks []MsgContent
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return nil, err
	}
	return blocks, nil
}

func msgMsgToIR(m MsgMessage) IRMessage {
	content, _ := parseMsgMessageContent(m.Content)
	ir := IRMessage{Role: m.Role}
	for _, c := range content {
		switch c.Type {
		case "text":
			ir.Content = append(ir.Content, IRContent{Type: "text", Text: c.Text})
		case "thinking":
			ir.Content = append(ir.Content, IRContent{Type: "thinking", Text: c.Thinking})
		case "image":
			if c.Source == nil {
				continue
			}
			ir.Content = append(ir.Content, IRContent{
				Type: "image",
				Source: &IRImageSource{
					Type:      c.Source.Type,
					MediaType: c.Source.MediaType,
					Data:      c.Source.Data,
					URL:       c.Source.URL,
				},
			})
		case "tool_use":
			ir.Content = append(ir.Content, IRContent{
				Type:  "tool_use",
				ID:    c.ID,
				Name:  c.Name,
				Input: c.Input,
			})
			ir.ToolCalls = append(ir.ToolCalls, IRToolCall{
				ID:        c.ID,
				Name:      c.Name,
				Arguments: string(c.Input),
			})
		case "tool_result":
			irContent := IRContent{
				Type:    "tool_result",
				ToolID:  c.ToolUseID,
				IsError: c.IsError,
			}
			if c.Content != nil {
				if s, ok := c.Content.(string); ok {
					irContent.Text = s
				} else {
					b, _ := json.Marshal(c.Content)
					var blocks []MsgContent
					if json.Unmarshal(b, &blocks) == nil {
						var parts []string
						for _, block := range blocks {
							if block.Type == "text" {
								parts = append(parts, block.Text)
							}
						}
						irContent.Text = strings.Join(parts, "")
					}
				}
			}
			ir.Content = append(ir.Content, irContent)
			ir.ToolCallID = c.ToolUseID
		}
	}
	// shorthand: if only text content and no tool calls, set Text
	if len(ir.Content) == 1 && ir.Content[0].Type == "text" && len(ir.ToolCalls) == 0 {
		ir.Text = ir.Content[0].Text
	}
	return ir
}

func irMsgToMsg(m IRMessage) MsgMessage {
	mm := MsgMessage{Role: m.Role}
	// system messages should have been stripped by caller, but guard anyway
	if m.Role == "system" {
		mm.Role = "user"
	}
	// Text shorthand
	if m.Text != "" && len(m.Content) == 0 && len(m.ToolCalls) == 0 {
		mm.Content, _ = json.Marshal(m.Text)
		return mm
	}
	var blocks []MsgContent
	for _, c := range m.Content {
		switch c.Type {
		case "text":
			blocks = append(blocks, MsgContent{Type: "text", Text: c.Text})
		case "thinking":
			blocks = append(blocks, MsgContent{Type: "thinking", Thinking: c.Text})
		case "image":
			if c.Source != nil {
				blocks = append(blocks, MsgContent{
					Type:   "image",
					Source: &MsgImageSource{Type: c.Source.Type, MediaType: c.Source.MediaType, Data: c.Source.Data, URL: c.Source.URL},
				})
			}
		case "tool_use":
			blocks = append(blocks, MsgContent{
				Type:  "tool_use",
				ID:    c.ID,
				Name:  c.Name,
				Input: c.Input,
			})
		case "tool_result":
			var content any
			if c.Text != "" {
				content = c.Text
			}
			blocks = append(blocks, MsgContent{
				Type:      "tool_result",
				ToolUseID: c.ToolID,
				IsError:   c.IsError,
				Content:   content,
			})
		}
	}
	mm.Content, _ = json.Marshal(blocks)
	return mm
}

func msgContentToIRMessage(content []MsgContent) IRMessage {
	var ir IRMessage
	for _, c := range content {
		switch c.Type {
		case "text":
			ir.Content = append(ir.Content, IRContent{Type: "text", Text: c.Text})
		case "thinking":
			ir.Content = append(ir.Content, IRContent{Type: "thinking", Text: c.Thinking})
		case "tool_use":
			ir.Content = append(ir.Content, IRContent{Type: "tool_use", ID: c.ID, Name: c.Name, Input: c.Input})
			ir.ToolCalls = append(ir.ToolCalls, IRToolCall{ID: c.ID, Name: c.Name, Arguments: string(c.Input)})
		}
	}
	if len(ir.Content) == 1 && ir.Content[0].Type == "text" {
		ir.Text = ir.Content[0].Text
	}
	return ir
}

func irMsgToMsgContent(m IRMessage) []MsgContent {
	if m.Text != "" && len(m.Content) == 0 {
		return []MsgContent{{Type: "text", Text: m.Text}}
	}
	var out []MsgContent
	for _, c := range m.Content {
		switch c.Type {
		case "text":
			out = append(out, MsgContent{Type: "text", Text: c.Text})
		case "tool_use":
			out = append(out, MsgContent{Type: "tool_use", ID: c.ID, Name: c.Name, Input: c.Input})
		case "tool_result":
			out = append(out, MsgContent{Type: "tool_result", ToolUseID: c.ToolID, IsError: c.IsError})
		}
	}
	return out
}
