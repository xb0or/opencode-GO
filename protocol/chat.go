package protocol

import (
	"encoding/json"
	"fmt"
)

// ──────────────────────── OpenAI Chat Completions ────────────────────

// ChatRequest is the wire format for POST /v1/chat/completions.
type ChatRequest struct {
	Model       string          `json:"model"`
	Messages    []ChatMessage   `json:"messages"`
	Temperature *float64        `json:"temperature,omitempty"`
	MaxTokens   int             `json:"max_tokens,omitempty"`
	Stream      bool            `json:"stream,omitempty"`
	Tools       []ChatTool      `json:"tools,omitempty"`
	ToolChoice  json.RawMessage `json:"tool_choice,omitempty"`
	TopP        *float64        `json:"top_p,omitempty"`
	Stop        []string        `json:"stop,omitempty"`
}

type ChatMessage struct {
	Role       string         `json:"role"`
	Content    any            `json:"content,omitempty"` // string | []ChatContent
	Name       string         `json:"name,omitempty"`
	ToolCalls  []ChatToolCall `json:"tool_calls,omitempty"`
	ToolCallID string         `json:"tool_call_id,omitempty"`
}

type ChatContent struct {
	Type     string        `json:"type"`
	Text     string        `json:"text,omitempty"`
	ImageURL *ChatImageURL `json:"image_url,omitempty"`
}

type ChatImageURL struct {
	URL string `json:"url"`
}

type ChatTool struct {
	Type     string           `json:"type"` // function
	Function ChatToolFunction `json:"function"`
}

type ChatToolFunction struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
	Strict      bool            `json:"strict,omitempty"`
}

type ChatToolCall struct {
	ID       string           `json:"id"`
	Type     string           `json:"type"` // function
	Function ChatToolCallFunc `json:"function"`
}

type ChatToolCallFunc struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// ChatResponse is the wire format for a non-streaming completion.
type ChatResponse struct {
	ID      string       `json:"id"`
	Object  string       `json:"object"`
	Created int64        `json:"created"`
	Model   string       `json:"model"`
	Choices []ChatChoice `json:"choices"`
	Usage   *ChatUsage   `json:"usage,omitempty"`
}

type ChatChoice struct {
	Index        int         `json:"index"`
	Message      ChatMessage `json:"message"`
	FinishReason string      `json:"finish_reason,omitempty"`
}

type ChatUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// ChatStreamChunk is one SSE delta for a streaming chat completion.
type ChatStreamChunk struct {
	ID      string             `json:"id"`
	Object  string             `json:"object"`
	Created int64              `json:"created"`
	Model   string             `json:"model"`
	Choices []ChatStreamChoice `json:"choices"`
	Usage   *ChatUsage         `json:"usage,omitempty"`
}

type ChatStreamChoice struct {
	Index        int         `json:"index"`
	Delta        ChatMessage `json:"delta"`
	FinishReason *string     `json:"finish_reason,omitempty"`
}

// ──────────────────────── Decoders ──────────────────────────────────

// DecodeChatRequest parses a Chat Completions request body into IR.
func DecodeChatRequest(data []byte) (*IRRequest, error) {
	var req ChatRequest
	if err := json.Unmarshal(data, &req); err != nil {
		return nil, fmt.Errorf("chat: decode request: %w", err)
	}
	ir := &IRRequest{
		Model:       req.Model,
		Temperature: req.Temperature,
		MaxTokens:   req.MaxTokens,
		Stream:      req.Stream,
		ToolChoice:  req.ToolChoice,
		TopP:        req.TopP,
		Stop:        req.Stop,
	}
	for _, m := range req.Messages {
		ir.Messages = append(ir.Messages, chatMsgToIR(m))
	}
	for _, t := range req.Tools {
		ir.Tools = append(ir.Tools, IRTool{
			Name:        t.Function.Name,
			Description: t.Function.Description,
			Parameters:  t.Function.Parameters,
			Strict:      t.Function.Strict,
		})
	}
	return ir, nil
}

// EncodeChatRequest serializes an IR request into Chat Completions wire format.
func EncodeChatRequest(ir *IRRequest) ([]byte, error) {
	req := ChatRequest{
		Model:       ir.Model,
		Temperature: ir.Temperature,
		MaxTokens:   ir.MaxTokens,
		Stream:      ir.Stream,
		ToolChoice:  normalizeToolChoiceForChat(ir.ToolChoice),
		TopP:        ir.TopP,
		Stop:        ir.Stop,
	}
	// If there's a system prompt (from Anthropic's system field), prepend it as
	// a system message.
	if ir.System != "" {
		req.Messages = append(req.Messages, ChatMessage{Role: "system", Content: ir.System})
	}
	for _, m := range ir.Messages {
		req.Messages = append(req.Messages, irMsgToChat(m))
	}
	for _, t := range ir.Tools {
		req.Tools = append(req.Tools, ChatTool{
			Type: "function",
			Function: ChatToolFunction{
				Name:        t.Name,
				Description: t.Description,
				Parameters:  t.Parameters,
				Strict:      t.Strict,
			},
		})
	}
	return json.Marshal(req)
}

// DecodeChatResponse parses a Chat Completions response into IR.
func DecodeChatResponse(data []byte) (*IRResponse, error) {
	var resp ChatResponse
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, fmt.Errorf("chat: decode response: %w", err)
	}
	ir := &IRResponse{
		ID:    resp.ID,
		Model: resp.Model,
	}
	if resp.Usage != nil {
		ir.Usage = &IRUsage{
			PromptTokens:     resp.Usage.PromptTokens,
			CompletionTokens: resp.Usage.CompletionTokens,
			TotalTokens:      resp.Usage.TotalTokens,
		}
	}
	for _, ch := range resp.Choices {
		msg := chatMsgToIR(ch.Message)
		ir.Choices = append(ir.Choices, IRChoice{
			Index:        ch.Index,
			Message:      &msg,
			FinishReason: ch.FinishReason,
		})
	}
	return ir, nil
}

// EncodeChatResponse serializes an IR response into Chat Completions wire format.
func EncodeChatResponse(ir *IRResponse) ([]byte, error) {
	resp := ChatResponse{
		ID:      ir.ID,
		Object:  "chat.completion",
		Model:   ir.Model,
		Created: 0,
	}
	if ir.Usage != nil {
		resp.Usage = &ChatUsage{
			PromptTokens:     ir.Usage.PromptTokens,
			CompletionTokens: ir.Usage.CompletionTokens,
			TotalTokens:      ir.Usage.TotalTokens,
		}
	}
	for _, ch := range ir.Choices {
		cc := ChatChoice{Index: ch.Index, FinishReason: normalizeChatFinishReason(ch.FinishReason)}
		if ch.Message != nil {
			cc.Message = irMsgToChat(*ch.Message)
		}
		resp.Choices = append(resp.Choices, cc)
	}
	return json.Marshal(resp)
}

// normalizeChatFinishReason maps protocol-specific finish reasons to OpenAI Chat values.
func normalizeChatFinishReason(reason string) string {
	switch reason {
	case "end_turn", "stop_sequence":
		return "stop"
	case "tool_use":
		return "tool_calls"
	default:
		return reason
	}
}

// DecodeChatStreamChunk parses a streaming SSE chunk into an IRStreamEvent.
func DecodeChatStreamChunk(data []byte) (*IRStreamEvent, error) {
	var chunk ChatStreamChunk
	if err := json.Unmarshal(data, &chunk); err != nil {
		return nil, fmt.Errorf("chat: decode stream chunk: %w", err)
	}
	ev := &IRStreamEvent{Type: "completion"}
	if chunk.Usage != nil {
		ev.Response = &IRResponse{Model: chunk.Model, Usage: &IRUsage{
			PromptTokens:     chunk.Usage.PromptTokens,
			CompletionTokens: chunk.Usage.CompletionTokens,
			TotalTokens:      chunk.Usage.TotalTokens,
		}}
	}
	for _, ch := range chunk.Choices {
		msg := chatMsgToIR(ch.Delta)
		fin := ""
		if ch.FinishReason != nil {
			fin = *ch.FinishReason
		}
		ev.Choice = &IRChoice{
			Index:        ch.Index,
			Delta:        &msg,
			FinishReason: fin,
		}
		break // chat completions always have 1 choice in streaming
	}
	return ev, nil
}

// EncodeChatStreamChunk serializes an IR stream event into a Chat SSE chunk.
func EncodeChatStreamChunk(ev *IRStreamEvent) ([]byte, error) {
	chunk := ChatStreamChunk{
		ID:      "",
		Object:  "chat.completion.chunk",
		Created: 0,
	}
	if ev.Response != nil {
		chunk.ID = ev.Response.ID
		chunk.Model = ev.Response.Model
		if ev.Response.Usage != nil {
			chunk.Usage = &ChatUsage{
				PromptTokens:     ev.Response.Usage.PromptTokens,
				CompletionTokens: ev.Response.Usage.CompletionTokens,
				TotalTokens:      ev.Response.Usage.TotalTokens,
			}
		}
	}
	if ev.Choice != nil {
		delta := ChatMessage{}
		if ev.Choice.Delta != nil {
			delta = irMsgToChat(*ev.Choice.Delta)
		}
		sc := ChatStreamChoice{Index: ev.Choice.Index, Delta: delta}
		if ev.Choice.FinishReason != "" {
			fin := ev.Choice.FinishReason
			sc.FinishReason = &fin
		}
		chunk.Choices = append(chunk.Choices, sc)
	}
	return json.Marshal(chunk)
}

// ──────────────────────── helpers ───────────────────────────────────

func chatMsgToIR(m ChatMessage) IRMessage {
	ir := IRMessage{Role: m.Role, Name: m.Name, ToolCallID: m.ToolCallID}
	switch v := m.Content.(type) {
	case string:
		ir.Text = v
	case []any:
		for _, item := range v {
			b, _ := json.Marshal(item)
			var bc ChatContent
			if err := json.Unmarshal(b, &bc); err == nil {
				ir.Content = append(ir.Content, IRContent{
					Type: bc.Type,
					Text: bc.Text,
					Source: func() *IRImageSource {
						if bc.ImageURL != nil {
							return &IRImageSource{URL: bc.ImageURL.URL, Type: "url"}
						}
						return nil
					}(),
				})
			}
		}
	}
	for _, tc := range m.ToolCalls {
		ir.ToolCalls = append(ir.ToolCalls, IRToolCall{
			ID:        tc.ID,
			Name:      tc.Function.Name,
			Arguments: tc.Function.Arguments,
		})
	}
	return ir
}

func irMsgToChat(m IRMessage) ChatMessage {
	cm := ChatMessage{Role: m.Role, Name: m.Name, ToolCallID: m.ToolCallID}
	if m.Text != "" && len(m.Content) == 0 {
		cm.Content = m.Text
	} else if len(m.Content) > 0 {
		var parts []ChatContent
		for _, c := range m.Content {
			switch c.Type {
			case "text", "thinking":
				parts = append(parts, ChatContent{Type: "text", Text: c.Text})
			case "image":
				if c.Source != nil {
					parts = append(parts, ChatContent{Type: "image_url", ImageURL: &ChatImageURL{URL: c.Source.URL}})
				}
			}
		}
		if len(parts) == 1 && parts[0].Type == "text" {
			cm.Content = parts[0].Text
		} else {
			cm.Content = parts
		}
	} else {
		cm.Content = ""
	}
	for _, tc := range m.ToolCalls {
		cm.ToolCalls = append(cm.ToolCalls, ChatToolCall{
			ID:   tc.ID,
			Type: "function",
			Function: ChatToolCallFunc{
				Name:      tc.Name,
				Arguments: tc.Arguments,
			},
		})
	}
	return cm
}
