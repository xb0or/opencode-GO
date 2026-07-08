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
	Role             string         `json:"role"`
	Content          any            `json:"content,omitempty"` // string | []ChatContent
	ReasoningContent *string        `json:"reasoning_content,omitempty"`
	Name             string         `json:"name,omitempty"`
	ToolCalls        []ChatToolCall `json:"tool_calls,omitempty"`
	ToolCallID       string         `json:"tool_call_id,omitempty"`
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
	Index    int              `json:"index,omitempty"`
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
	PromptTokens            int                 `json:"prompt_tokens"`
	CompletionTokens        int                 `json:"completion_tokens"`
	TotalTokens             int                 `json:"total_tokens"`
	PromptTokensDetails     *ChatTokensDetails  `json:"prompt_tokens_details,omitempty"`
	CompletionTokensDetails *ChatTokensDetails `json:"completion_tokens_details,omitempty"`
}

// ChatTokensDetails carries nested per-token-class counts (cache, reasoning).
type ChatTokensDetails struct {
	CachedTokens    int `json:"cached_tokens,omitempty"`
	ReasoningTokens int `json:"reasoning_tokens,omitempty"`
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
		if !isFunctionToolType(t.Type) {
			continue
		}
		ir.Tools = appendIRToolIfValid(ir.Tools, IRTool{
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
	tools := cleanIRTools(ir.Tools)
	includeEmptyReasoning := chatModelRequiresReasoningContent(ir.Model)
	req := ChatRequest{
		Model:       ir.Model,
		Temperature: ir.Temperature,
		MaxTokens:   ir.MaxTokens,
		Stream:      ir.Stream,
		ToolChoice:  normalizeToolChoiceForChat(ir.ToolChoice, tools),
		TopP:        ir.TopP,
		Stop:        ir.Stop,
	}
	// Chat-compatible APIs require system/developer instructions to be outside
	// assistant tool_call -> tool result runs. Fold system-like messages from any
	// position into the leading system prompt before normalizing tool pairs.
	instructions := ir.System
	var chatHistory []IRMessage
	for _, m := range ir.Messages {
		if isSystemLikeRole(m.Role) {
			if text := messageTextForSystem(m); text != "" {
				if instructions != "" {
					instructions += "\n"
				}
				instructions += text
			}
			continue
		}
		chatHistory = append(chatHistory, m)
	}
	if instructions != "" {
		req.Messages = append(req.Messages, ChatMessage{Role: "system", Content: instructions})
	}
	for _, m := range normalizeChatHistory(chatHistory) {
		req.Messages = append(req.Messages, irMsgToChatWithReasoning(m, includeEmptyReasoning))
	}
	for _, t := range tools {
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
			CacheReadTokens:  chatCacheReadTokens(resp.Usage),
			ReasoningTokens:  chatReasoningTokens(resp.Usage),
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
			PromptTokensDetails: chatUsageDetailsFromIR(ir.Usage, true),
			CompletionTokensDetails: chatUsageDetailsFromIR(ir.Usage, false),
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

// chatCacheReadTokens extracts cached/hit tokens from chat usage details.
func chatCacheReadTokens(u *ChatUsage) int {
	if u == nil {
		return 0
	}
	if u.PromptTokensDetails != nil && u.PromptTokensDetails.CachedTokens > 0 {
		return u.PromptTokensDetails.CachedTokens
	}
	return 0
}

// chatReasoningTokens extracts reasoning tokens from chat usage details.
func chatReasoningTokens(u *ChatUsage) int {
	if u == nil {
		return 0
	}
	if u.CompletionTokensDetails != nil && u.CompletionTokensDetails.ReasoningTokens > 0 {
		return u.CompletionTokensDetails.ReasoningTokens
	}
	return 0
}

// chatUsageDetailsFromIR rebuilds a nested ChatTokensDetails from IR usage
// accounting. Only fields with non-zero counts are emitted.
func chatUsageDetailsFromIR(u *IRUsage, promptSide bool) *ChatTokensDetails {
	if u == nil {
		return nil
	}
	if promptSide {
		if u.CacheReadTokens == 0 {
			return nil
		}
		return &ChatTokensDetails{CachedTokens: u.CacheReadTokens}
	}
	if u.ReasoningTokens == 0 {
		return nil
	}
	return &ChatTokensDetails{ReasoningTokens: u.ReasoningTokens}
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
			CacheReadTokens:  chatCacheReadTokens(chunk.Usage),
			ReasoningTokens:  chatReasoningTokens(chunk.Usage),
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
				PromptTokensDetails: chatUsageDetailsFromIR(ev.Response.Usage, true),
				CompletionTokensDetails: chatUsageDetailsFromIR(ev.Response.Usage, false),
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

// StripToolChoiceForReasoning removes the tool_choice field from a Chat
// Completions request body when the target model is a reasoning/thinking
// model. Providers like DeepSeek reject non-auto tool_choice values while
// thinking mode is active ("Thinking mode does not support this tool_choice"),
// so we normalize to the provider default (auto) to avoid HTTP 400 errors.
// It returns the (possibly rewritten) body and true when the field was stripped.
func StripToolChoiceForReasoning(body []byte, model string) ([]byte, bool) {
	if !chatModelRequiresReasoningContent(model) {
		return body, false
	}
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		return body, false
	}
	if _, ok := m["tool_choice"]; !ok {
		return body, false
	}
	delete(m, "tool_choice")
	out, err := json.Marshal(m)
	if err != nil {
		return body, false
	}
	return out, true
}

func chatMsgToIR(m ChatMessage) IRMessage {
	ir := IRMessage{Role: m.Role, Name: m.Name, ToolCallID: m.ToolCallID}
	if m.ReasoningContent != nil {
		ir.Content = appendThinkingContentBlock(ir.Content, *m.ReasoningContent)
	}
	switch v := m.Content.(type) {
	case string:
		ir.Text = v
		if len(ir.Content) > 0 {
			ir.Content = appendTextContent(ir.Content, v)
		}
	case []any:
		for _, item := range v {
			b, _ := json.Marshal(item)
			var bc ChatContent
			if err := json.Unmarshal(b, &bc); err == nil {
				contentType := bc.Type
				if contentType == "input_text" || contentType == "output_text" {
					contentType = "text"
				}
				var content IRContent
				if bc.ImageURL != nil {
					content = imageContentFromDataURI(bc.ImageURL.URL)
				} else {
					content = IRContent{Type: contentType, Text: bc.Text}
				}
				ir.Content = append(ir.Content, content)
			}
		}
	}
	for _, tc := range m.ToolCalls {
		ir.ToolCalls = append(ir.ToolCalls, IRToolCall{
			Index:     tc.Index,
			ID:        tc.ID,
			Name:      tc.Function.Name,
			Arguments: tc.Function.Arguments,
		})
	}
	return ir
}

func irMsgToChat(m IRMessage) ChatMessage {
	return irMsgToChatWithReasoning(m, false)
}

func irMsgToChatWithReasoning(m IRMessage, includeEmptyReasoning bool) ChatMessage {
	cm := ChatMessage{Role: normalizeChatRole(m.Role), Name: m.Name, ToolCallID: m.ToolCallID}
	if text, ok := thinkingTextAndPresence(m); ok {
		cm.ReasoningContent = stringPtr(text)
	} else if includeEmptyReasoning && cm.Role == "assistant" {
		cm.ReasoningContent = stringPtr("")
	}
	if m.Text != "" && len(m.Content) == 0 {
		cm.Content = m.Text
	} else if len(m.Content) > 0 {
		var parts []ChatContent
		for _, c := range m.Content {
			switch c.Type {
			case "text":
				parts = append(parts, ChatContent{Type: "text", Text: c.Text})
			case "image":
				if c.Source != nil {
					url := c.Source.URL
					if url == "" && c.Source.Type == "base64" {
						url = imageSourceToDataURI(c.Source)
					}
					parts = append(parts, ChatContent{Type: "image_url", ImageURL: &ChatImageURL{URL: url}})
				}
			}
		}
		if len(parts) == 0 {
			cm.Content = ""
		} else if len(parts) == 1 && parts[0].Type == "text" {
			cm.Content = parts[0].Text
		} else {
			cm.Content = parts
		}
	} else {
		cm.Content = ""
	}
	for idx, tc := range cleanIRToolCalls(m.ToolCalls) {
		cm.ToolCalls = append(cm.ToolCalls, ChatToolCall{
			Index: idx,
			ID:    tc.ID,
			Type:  "function",
			Function: ChatToolCallFunc{
				Name:      tc.Name,
				Arguments: tc.Arguments,
			},
		})
	}
	return cm
}
