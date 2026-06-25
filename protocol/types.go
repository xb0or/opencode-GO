package protocol

import (
	"encoding/json"
	"strings"
)

// Protocol identifies the wire format.
type Protocol string

const (
	Chat      Protocol = "chat"      // OpenAI Chat Completions
	Messages  Protocol = "messages"  // Anthropic Messages
	Responses Protocol = "responses" // OpenAI Responses API
)

// ──────────────────────────── IR Request ────────────────────────────

// IRRequest is the unified intermediate representation of a completion request.
// Every incoming protocol decodes into this; every outgoing protocol encodes
// from this.
type IRRequest struct {
	Model       string          `json:"model"`
	System      string          `json:"system,omitempty"` // single system prompt (Anthropic)
	Messages    []IRMessage     `json:"messages"`
	Temperature *float64        `json:"temperature,omitempty"`
	MaxTokens   int             `json:"max_tokens,omitempty"`
	Stream      bool            `json:"stream"`
	Tools       []IRTool        `json:"tools,omitempty"`
	ToolChoice  json.RawMessage `json:"tool_choice,omitempty"` // pass-through
	TopP        *float64        `json:"top_p,omitempty"`
	Stop        []string        `json:"stop,omitempty"`
	// Extra holds protocol-specific fields we don't fully understand.
	Extra map[string]any `json:"extra,omitempty"`
}

// IRMessage is one turn in the conversation.
type IRMessage struct {
	Role       string         `json:"role"`                   // system | user | assistant | tool
	Content    []IRContent    `json:"content,omitempty"`      // multi-part content
	Text       string         `json:"text,omitempty"`         // convenience: single-text shorthand
	ToolCalls  []IRToolCall   `json:"tool_calls,omitempty"`   // assistant→tool invocation
	ToolCallID string         `json:"tool_call_id,omitempty"` // tool response references this id
	Name       string         `json:"name,omitempty"`         // tool function name (for tool role)
	Extra      map[string]any `json:"extra,omitempty"`
}

// IRContent is a typed content block inside a message.
type IRContent struct {
	Type    string          `json:"type"`                  // text | image | tool_use | tool_result | thinking
	Text    string          `json:"text,omitempty"`        // for type=text or thinking
	Source  *IRImageSource  `json:"source,omitempty"`      // for type=image
	ID      string          `json:"id,omitempty"`          // tool_use id
	Name    string          `json:"name,omitempty"`        // tool_use function name
	Input   json.RawMessage `json:"input,omitempty"`       // tool_use input args
	ToolID  string          `json:"tool_use_id,omitempty"` // tool_result references tool_use
	IsError bool            `json:"is_error,omitempty"`    // tool_result error flag
	Extra   map[string]any  `json:"extra,omitempty"`
}

// IRImageSource describes an inline image.
type IRImageSource struct {
	Type      string `json:"type"`                 // base64
	MediaType string `json:"media_type,omitempty"` // image/png
	Data      string `json:"data,omitempty"`
	URL       string `json:"url,omitempty"`
}

// IRTool declares a function tool the model may call.
type IRTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"` // JSON Schema
	Strict      bool            `json:"strict,omitempty"`
}

// IRToolCall is a tool invocation made by the assistant.
type IRToolCall struct {
	Index     int    `json:"index,omitempty"`
	ID        string `json:"id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"` // JSON string
}

// ──────────────────────────── IR Response ───────────────────────────

// IRResponse is the unified intermediate representation of a completion response.
type IRResponse struct {
	ID      string         `json:"id"`
	Model   string         `json:"model"`
	Choices []IRChoice     `json:"choices"`
	Usage   *IRUsage       `json:"usage,omitempty"`
	Extra   map[string]any `json:"extra,omitempty"`
}

// IRChoice is one completion choice.
type IRChoice struct {
	Index        int            `json:"index"`
	Message      *IRMessage     `json:"message,omitempty"`       // non-streaming
	Delta        *IRMessage     `json:"delta,omitempty"`         // streaming
	FinishReason string         `json:"finish_reason,omitempty"` // stop | tool_calls | length | content_filter
	Extra        map[string]any `json:"extra,omitempty"`
}

// IRUsage carries token usage information.
type IRUsage struct {
	PromptTokens          int `json:"prompt_tokens"`
	CompletionTokens      int `json:"completion_tokens"`
	TotalTokens           int `json:"total_tokens,omitempty"`
	CacheReadTokens       int `json:"cache_read_tokens,omitempty"`
	CacheCreationTokens   int `json:"cache_creation_tokens,omitempty"`
	ReasoningTokens       int `json:"reasoning_tokens,omitempty"`
}

// ──────────────────────────── IR Stream Event ───────────────────────

// IRStreamEvent is one event in a streaming response.
type IRStreamEvent struct {
	Type     string      `json:"type"` // message_start | content_block_start | content_block_delta | content_block_stop | message_delta | message_stop | completion
	Response *IRResponse `json:"response,omitempty"`
	Choice   *IRChoice   `json:"choice,omitempty"`
	// ContentDelta carries partial text for content_block_delta events.
	ContentDelta string `json:"content_delta,omitempty"`
	// ToolCallDelta carries partial arguments for tool call streaming.
	ToolCallDelta *IRToolCallDelta `json:"tool_call_delta,omitempty"`
	Extra         map[string]any   `json:"extra,omitempty"`
}

// IRToolCallDelta is a partial tool call update during streaming.
type IRToolCallDelta struct {
	Index     int    `json:"index"`
	ID        string `json:"id,omitempty"`
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"` // partial chunk
}

// thinkingText returns the concatenated reasoning/thinking text blocks from a
// message. It intentionally ignores normal text so Chat-compatible providers
// that require a separate reasoning_content field do not receive CoT as user
// visible content.
func thinkingText(m IRMessage) string {
	text, _ := thinkingTextAndPresence(m)
	return text
}

// thinkingTextAndPresence returns the concatenated thinking text and whether
// at least one thinking block was present. Presence is distinct from non-empty
// text because some Chat-compatible providers emit an empty reasoning_content
// marker before later deltas.
func thinkingTextAndPresence(m IRMessage) (string, bool) {
	text := ""
	found := false
	for _, c := range m.Content {
		if c.Type == "thinking" {
			text += c.Text
			found = true
		}
	}
	return text, found
}

// appendThinkingContent appends reasoning text to the existing thinking block
// when possible. This keeps streamed reasoning deltas compact while preserving
// ordering for messages that already contain separate visible text blocks.
func appendThinkingContent(content []IRContent, text string) []IRContent {
	if text == "" {
		return content
	}
	return appendThinkingContentBlock(content, text)
}

// appendThinkingContentBlock is like appendThinkingContent but preserves an
// explicit empty thinking block.
func appendThinkingContentBlock(content []IRContent, text string) []IRContent {
	for i := range content {
		if content[i].Type == "thinking" {
			content[i].Text += text
			return content
		}
	}
	return append(content, IRContent{Type: "thinking", Text: text})
}

// appendTextContent appends visible text to the existing text block when
// possible without touching reasoning/thinking blocks.
func appendTextContent(content []IRContent, text string) []IRContent {
	if text == "" {
		return content
	}
	for i := range content {
		if content[i].Type == "text" {
			content[i].Text += text
			return content
		}
	}
	return append(content, IRContent{Type: "text", Text: text})
}

func stringPtr(s string) *string {
	return &s
}

func hasContentType(content []IRContent, typ string) bool {
	for _, c := range content {
		if c.Type == typ {
			return true
		}
	}
	return false
}

func visibleText(m IRMessage) string {
	if m.Text != "" {
		return m.Text
	}
	text := ""
	for _, c := range m.Content {
		if c.Type == "text" {
			text += c.Text
		}
	}
	return text
}

func messageTextForSystem(m IRMessage) string {
	text := visibleText(m)
	if text != "" {
		return text
	}
	for _, c := range m.Content {
		if c.Type == "thinking" {
			continue
		}
		if c.Text != "" {
			text += c.Text
		}
	}
	return text
}

func isEmptyMessage(m IRMessage) bool {
	return strings.TrimSpace(m.Role) == "" && visibleText(m) == "" && thinkingText(m) == "" && len(m.ToolCalls) == 0 && m.ToolCallID == ""
}

func hasAssistantPayload(m IRMessage) bool {
	return visibleText(m) != "" || thinkingText(m) != "" || len(cleanIRToolCalls(m.ToolCalls)) > 0
}

func withoutToolCalls(m IRMessage) IRMessage {
	m.ToolCalls = nil
	return m
}

func normalizeChatHistory(messages []IRMessage) []IRMessage {
	usedToolOutputs := make(map[int]bool)
	out := make([]IRMessage, 0, len(messages))
	for i, m := range messages {
		if usedToolOutputs[i] || isSystemLikeRole(m.Role) || isEmptyMessage(m) {
			continue
		}
		if m.Role == "tool" || m.ToolCallID != "" {
			// Tool messages are only valid immediately after the assistant tool_call
			// they answer. Orphans are dropped rather than forwarded as invalid Chat
			// history.
			continue
		}
		calls := cleanIRToolCalls(m.ToolCalls)
		if len(calls) == 0 {
			out = append(out, withoutToolCalls(m))
			continue
		}

		pairedCalls := make([]IRToolCall, 0, len(calls))
		pairedOutputs := make([]IRMessage, 0, len(calls))
		for _, call := range calls {
			if idx := findToolOutput(messages, usedToolOutputs, i+1, call.ID); idx >= 0 {
				pairedCalls = append(pairedCalls, call)
				toolMsg := messages[idx]
				toolMsg.Role = "tool"
				toolMsg.ToolCallID = call.ID
				toolMsg.ToolCalls = nil
				pairedOutputs = append(pairedOutputs, toolMsg)
				usedToolOutputs[idx] = true
			}
		}

		if len(pairedCalls) > 0 {
			pairedAssistant := m
			pairedAssistant.Role = "assistant"
			pairedAssistant.ToolCalls = pairedCalls
			out = append(out, pairedAssistant)
			out = append(out, pairedOutputs...)
			continue
		}

		fallback := withoutToolCalls(m)
		if hasAssistantPayload(fallback) {
			out = append(out, fallback)
		}
	}
	return out
}

func findToolOutput(messages []IRMessage, used map[int]bool, start int, callID string) int {
	if strings.TrimSpace(callID) == "" {
		return -1
	}
	for i := start; i < len(messages); i++ {
		if used[i] {
			continue
		}
		m := messages[i]
		if (m.Role == "tool" || m.ToolCallID != "") && m.ToolCallID == callID {
			return i
		}
	}
	return -1
}

func chatModelRequiresReasoningContent(model string) bool {
	normalized := strings.ToLower(strings.TrimSpace(model))
	if normalized == "" {
		return false
	}
	reasoningModels := []string{
		"deepseek-v4",
		"deepseek-reasoner",
		"deepseek-r1",
		"mimo-v2.5",
		"mimo-v.2.5",
	}
	for _, marker := range reasoningModels {
		if strings.Contains(normalized, marker) {
			return true
		}
	}
	return false
}
