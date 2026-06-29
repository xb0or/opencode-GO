package protocol

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/xb0or/opencode-GO/config"
)

// ──────────────────────── Request conversion tests ──────────────────

func TestConvertRequest_SameProtocol(t *testing.T) {
	body := []byte(`{"model":"test","messages":[{"role":"user","content":"hi"}]}`)
	out, err := ConvertRequest(config.ProtocolChat, config.ProtocolChat, body)
	if err != nil {
		t.Fatalf("same-protocol passthrough should not error: %v", err)
	}
	if string(out) != string(body) {
		t.Errorf("same-protocol should return identical body")
	}
}

func TestConvertRequest_ChatToMessages(t *testing.T) {
	body := []byte(`{
		"model": "claude-sonnet-4.5",
		"messages": [
			{"role": "system", "content": "You are helpful."},
			{"role": "user", "content": "Hello"}
		],
		"temperature": 0.7,
		"max_tokens": 1024
	}`)
	out, err := ConvertRequest(config.ProtocolChat, config.ProtocolMessages, body)
	if err != nil {
		t.Fatalf("chat→messages: %v", err)
	}

	var req MsgRequest
	if err := json.Unmarshal(out, &req); err != nil {
		t.Fatalf("chat→messages: output not valid Messages JSON: %v", err)
	}
	if req.Model != "claude-sonnet-4.5" {
		t.Errorf("model: got %q, want %q", req.Model, "claude-sonnet-4.5")
	}
	if req.MaxTokens != 1024 {
		t.Errorf("max_tokens: got %d, want 1024", req.MaxTokens)
	}
	// System messages should be folded into the system field.
	sys, ok := req.System.(string)
	if !ok || sys != "You are helpful." {
		t.Errorf("system: got %v, want %q", req.System, "You are helpful.")
	}
	// Only user/assistant messages remain.
	for _, m := range req.Messages {
		if m.Role == "system" {
			t.Errorf("system message should have been folded into system field")
		}
	}
}

func TestConvertRequest_MessagesToChat(t *testing.T) {
	body := []byte(`{
		"model": "gpt-4",
		"system": "Be concise.",
		"messages": [
			{"role": "user", "content": [{"type": "text", "text": "Hi"}]}
		],
		"max_tokens": 512
	}`)
	out, err := ConvertRequest(config.ProtocolMessages, config.ProtocolChat, body)
	if err != nil {
		t.Fatalf("messages→chat: %v", err)
	}

	var req ChatRequest
	if err := json.Unmarshal(out, &req); err != nil {
		t.Fatalf("messages→chat: output not valid Chat JSON: %v", err)
	}
	if req.Model != "gpt-4" {
		t.Errorf("model: got %q, want %q", req.Model, "gpt-4")
	}
	// System should become a system message.
	found := false
	for _, m := range req.Messages {
		if m.Role == "system" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected a system message in chat output")
	}
}

func TestConvertRequest_ChatToResponses(t *testing.T) {
	body := []byte(`{
		"model": "gpt-5",
		"messages": [
			{"role": "system", "content": "You are a coder."},
			{"role": "user", "content": "Write hello world"}
		]
	}`)
	out, err := ConvertRequest(config.ProtocolChat, config.ProtocolResponses, body)
	if err != nil {
		t.Fatalf("chat→responses: %v", err)
	}

	var req RespRequest
	if err := json.Unmarshal(out, &req); err != nil {
		t.Fatalf("chat→responses: output not valid Responses JSON: %v", err)
	}
	if req.Model != "gpt-5" {
		t.Errorf("model: got %q, want %q", req.Model, "gpt-5")
	}
	if req.Instructions != "You are a coder." {
		t.Errorf("instructions: got %q, want %q", req.Instructions, "You are a coder.")
	}
}

func TestConvertRequest_OmitsDefaultAutoToolChoice(t *testing.T) {
	chatBody := []byte(`{
		"model": "claude",
		"messages": [{"role": "user", "content": "hi"}],
		"tools": [{"type":"function","function":{"name":"lookup","parameters":{"type":"object"}}}],
		"tool_choice": "auto"
	}`)
	msgOut, err := ConvertRequest(config.ProtocolChat, config.ProtocolMessages, chatBody)
	if err != nil {
		t.Fatalf("chat→messages: %v", err)
	}
	var msgReq map[string]any
	if err := json.Unmarshal(msgOut, &msgReq); err != nil {
		t.Fatalf("messages output JSON: %v", err)
	}
	if _, ok := msgReq["tool_choice"]; ok {
		t.Fatalf("default auto tool_choice should be omitted for messages: %s", string(msgOut))
	}

	responsesBody := []byte(`{
		"model": "gpt",
		"input": "hi",
		"tools": [{"type":"function","name":"lookup","parameters":{"type":"object"}}],
		"tool_choice": "auto"
	}`)
	chatOut, err := ConvertRequest(config.ProtocolResponses, config.ProtocolChat, responsesBody)
	if err != nil {
		t.Fatalf("responses→chat: %v", err)
	}
	var chatReq map[string]any
	if err := json.Unmarshal(chatOut, &chatReq); err != nil {
		t.Fatalf("chat output JSON: %v", err)
	}
	if _, ok := chatReq["tool_choice"]; ok {
		t.Fatalf("default auto tool_choice should be omitted for chat: %s", string(chatOut))
	}
}

func TestConvertRequest_NormalizesNamedToolChoice(t *testing.T) {
	chatBody := []byte(`{
		"model": "claude",
		"messages": [{"role": "user", "content": "hi"}],
		"tools": [{"type":"function","function":{"name":"lookup","parameters":{"type":"object"}}}],
		"tool_choice": {"type":"function","function":{"name":"lookup"}}
	}`)
	msgOut, err := ConvertRequest(config.ProtocolChat, config.ProtocolMessages, chatBody)
	if err != nil {
		t.Fatalf("chat→messages: %v", err)
	}
	var msgReq MsgRequest
	if err := json.Unmarshal(msgOut, &msgReq); err != nil {
		t.Fatalf("messages output JSON: %v", err)
	}
	var msgChoice map[string]any
	if err := json.Unmarshal(msgReq.ToolChoice, &msgChoice); err != nil {
		t.Fatalf("messages tool_choice JSON: %v", err)
	}
	if msgChoice["type"] != "tool" || msgChoice["name"] != "lookup" {
		t.Fatalf("messages tool_choice = %#v, want tool lookup", msgChoice)
	}

	messagesBody := []byte(`{
		"model": "gpt",
		"max_tokens": 16,
		"messages": [{"role": "user", "content": "hi"}],
		"tools": [{"name":"lookup","input_schema":{"type":"object"}}],
		"tool_choice": {"type":"tool","name":"lookup"}
	}`)
	respOut, err := ConvertRequest(config.ProtocolMessages, config.ProtocolResponses, messagesBody)
	if err != nil {
		t.Fatalf("messages→responses: %v", err)
	}
	var respReq RespRequest
	if err := json.Unmarshal(respOut, &respReq); err != nil {
		t.Fatalf("responses output JSON: %v", err)
	}
	var respChoice map[string]any
	if err := json.Unmarshal(respReq.ToolChoice, &respChoice); err != nil {
		t.Fatalf("responses tool_choice JSON: %v", err)
	}
	function, _ := respChoice["function"].(map[string]any)
	if respChoice["type"] != "function" || function["name"] != "lookup" {
		t.Fatalf("responses tool_choice = %#v, want function lookup", respChoice)
	}
}

func TestConvertRequest_ResponsesDeveloperRoleToChatSystem(t *testing.T) {
	body := []byte(`{
		"model": "deepseek-v4-flash",
		"input": [
			{"type": "message", "role": "user", "content": "Hello"},
			{"type": "message", "role": "developer", "content": "Follow internal rules."}
		]
	}`)
	out, err := ConvertRequest(config.ProtocolResponses, config.ProtocolChat, body)
	if err != nil {
		t.Fatalf("responses→chat: %v", err)
	}
	var req ChatRequest
	if err := json.Unmarshal(out, &req); err != nil {
		t.Fatalf("chat output JSON: %v", err)
	}
	for _, m := range req.Messages {
		if m.Role == "developer" {
			t.Fatalf("developer role should not be sent to chat upstream: %s", string(out))
		}
	}
	foundSystem := false
	for _, m := range req.Messages {
		if m.Role == "system" && m.Content == "Follow internal rules." {
			foundSystem = true
		}
	}
	if !foundSystem {
		t.Fatalf("developer role was not converted to system: %#v", req.Messages)
	}
}

func TestConvertRequest_ChatDeveloperRoleToMessagesSystem(t *testing.T) {
	body := []byte(`{
		"model": "claude",
		"messages": [
			{"role": "developer", "content": "Follow internal rules."},
			{"role": "user", "content": "Hello"}
		],
		"max_tokens": 16
	}`)
	out, err := ConvertRequest(config.ProtocolChat, config.ProtocolMessages, body)
	if err != nil {
		t.Fatalf("chat→messages: %v", err)
	}
	var req MsgRequest
	if err := json.Unmarshal(out, &req); err != nil {
		t.Fatalf("messages output JSON: %v", err)
	}
	if req.System != "Follow internal rules." {
		t.Fatalf("developer role should fold into system, got %#v", req.System)
	}
	for _, m := range req.Messages {
		if m.Role == "developer" || m.Role == "system" {
			t.Fatalf("system-like role should not remain in messages: %#v", req.Messages)
		}
	}
}

func TestConvertRequest_ResponsesToChatFiltersUnsupportedAndInvalidTools(t *testing.T) {
	body := []byte(`{
		"model": "deepseek-v4-flash",
		"input": "hello",
		"tools": [
			{"type": "function", "name": "valid_tool", "parameters": {"type": "object"}},
			{"type": "web_search_preview"},
			{"type": "function", "name": "", "parameters": {"type": "object"}},
			{"type": "function", "name": "bad name", "parameters": {"type": "object"}},
			{"type": "function", "name": "valid_tool", "parameters": {"type": "object"}}
		],
		"tool_choice": {"type":"function","function":{"name":"missing_tool"}}
	}`)
	out, err := ConvertRequest(config.ProtocolResponses, config.ProtocolChat, body)
	if err != nil {
		t.Fatalf("responses→chat: %v", err)
	}
	var req ChatRequest
	if err := json.Unmarshal(out, &req); err != nil {
		t.Fatalf("chat output JSON: %v", err)
	}
	if len(req.Tools) != 1 {
		t.Fatalf("tools = %#v, want exactly one valid function tool", req.Tools)
	}
	if req.Tools[0].Function.Name != "valid_tool" {
		t.Fatalf("tool name = %q, want valid_tool", req.Tools[0].Function.Name)
	}
	if len(req.ToolChoice) != 0 {
		t.Fatalf("tool_choice referencing a filtered/missing tool should be omitted: %s", string(req.ToolChoice))
	}
}

func TestConvertRequest_FiltersInvalidToolsForAllTargets(t *testing.T) {
	ir := &IRRequest{
		Model:      "m",
		Messages:   []IRMessage{{Role: "user", Text: "hello"}},
		Tools:      []IRTool{{Name: ""}, {Name: "bad name"}, {Name: "ok_tool"}, {Name: "ok_tool"}},
		ToolChoice: json.RawMessage(`{"type":"function","function":{"name":"ok_tool"}}`),
	}

	chatOut, err := EncodeChatRequest(ir)
	if err != nil {
		t.Fatalf("encode chat: %v", err)
	}
	var chatReq ChatRequest
	if err := json.Unmarshal(chatOut, &chatReq); err != nil {
		t.Fatalf("chat output JSON: %v", err)
	}
	if len(chatReq.Tools) != 1 || chatReq.Tools[0].Function.Name != "ok_tool" {
		t.Fatalf("chat tools = %#v, want one ok_tool", chatReq.Tools)
	}
	if !strings.Contains(string(chatReq.ToolChoice), "ok_tool") {
		t.Fatalf("chat tool_choice should keep ok_tool, got %s", string(chatReq.ToolChoice))
	}

	msgOut, err := EncodeMessagesRequest(ir)
	if err != nil {
		t.Fatalf("encode messages: %v", err)
	}
	var msgReq MsgRequest
	if err := json.Unmarshal(msgOut, &msgReq); err != nil {
		t.Fatalf("messages output JSON: %v", err)
	}
	if len(msgReq.Tools) != 1 || msgReq.Tools[0].Name != "ok_tool" {
		t.Fatalf("messages tools = %#v, want one ok_tool", msgReq.Tools)
	}
	if !strings.Contains(string(msgReq.ToolChoice), "ok_tool") {
		t.Fatalf("messages tool_choice should keep ok_tool, got %s", string(msgReq.ToolChoice))
	}

	respOut, err := EncodeResponsesRequest(ir)
	if err != nil {
		t.Fatalf("encode responses: %v", err)
	}
	var respReq RespRequest
	if err := json.Unmarshal(respOut, &respReq); err != nil {
		t.Fatalf("responses output JSON: %v", err)
	}
	if len(respReq.Tools) != 1 || respReq.Tools[0].Name != "ok_tool" {
		t.Fatalf("responses tools = %#v, want one ok_tool", respReq.Tools)
	}
	if !strings.Contains(string(respReq.ToolChoice), "ok_tool") {
		t.Fatalf("responses tool_choice should keep ok_tool, got %s", string(respReq.ToolChoice))
	}
}

func TestConvertRequest_ChatReasoningContentPreserved(t *testing.T) {
	body := []byte(`{
		"model": "deepseek-v4-flash",
		"messages": [
			{"role": "assistant", "content": "I will call a tool.", "reasoning_content": "Need tool."},
			{"role": "user", "content": "continue"}
		]
	}`)
	out, err := ConvertRequest(config.ProtocolChat, config.ProtocolMessages, body)
	if err != nil {
		t.Fatalf("chat→messages: %v", err)
	}
	var req MsgRequest
	if err := json.Unmarshal(out, &req); err != nil {
		t.Fatalf("messages output JSON: %v", err)
	}
	var foundThinking bool
	for _, m := range req.Messages {
		var blocks []MsgContent
		if err := json.Unmarshal(m.Content, &blocks); err != nil {
			continue
		}
		for _, b := range blocks {
			if b.Type == "thinking" && b.Thinking == "Need tool." {
				foundThinking = true
			}
			if b.Type == "text" && b.Text == "Need tool." {
				t.Fatalf("reasoning_content leaked into visible text: %#v", blocks)
			}
		}
	}
	if !foundThinking {
		t.Fatalf("reasoning_content was not preserved as thinking: %s", string(out))
	}
}

func TestEncodeChatRequestReasoningModelsInjectEmptyAssistantReasoning(t *testing.T) {
	ir := &IRRequest{
		Model: "deepseek-v4-flash",
		Messages: []IRMessage{
			{Role: "assistant", Text: "old answer"},
			{Role: "user", Text: "follow up"},
		},
	}
	out, err := EncodeChatRequest(ir)
	if err != nil {
		t.Fatalf("encode chat: %v", err)
	}
	var req ChatRequest
	if err := json.Unmarshal(out, &req); err != nil {
		t.Fatalf("chat output JSON: %v", err)
	}
	if len(req.Messages) == 0 || req.Messages[0].ReasoningContent == nil {
		t.Fatalf("reasoning model assistant history must include empty reasoning_content: %s", string(out))
	}
	if *req.Messages[0].ReasoningContent != "" {
		t.Fatalf("empty fallback reasoning_content = %q, want empty string", *req.Messages[0].ReasoningContent)
	}

	ir.Model = "gpt-4"
	out, err = EncodeChatRequest(ir)
	if err != nil {
		t.Fatalf("encode ordinary chat: %v", err)
	}
	req = ChatRequest{}
	if err := json.Unmarshal(out, &req); err != nil {
		t.Fatalf("ordinary chat output JSON: %v", err)
	}
	if req.Messages[0].ReasoningContent != nil {
		t.Fatalf("ordinary models should not receive synthetic reasoning_content: %s", string(out))
	}
}

func TestConvertRequest_ResponsesReasoningContentToChat(t *testing.T) {
	body := []byte(`{
		"model": "deepseek-v4-flash",
		"input": [
			{"type": "message", "role": "assistant", "content": "visible", "reasoning_content": "hidden"},
			{"type": "function_call", "call_id": "call_1", "name": "read_file", "arguments": "{}", "reasoning_content": ""},
			{"type": "function_call_output", "call_id": "call_1", "output": "ok"}
		]
	}`)
	out, err := ConvertRequest(config.ProtocolResponses, config.ProtocolChat, body)
	if err != nil {
		t.Fatalf("responses→chat: %v", err)
	}
	var req ChatRequest
	if err := json.Unmarshal(out, &req); err != nil {
		t.Fatalf("chat output JSON: %v", err)
	}
	if len(req.Messages) != 3 {
		t.Fatalf("messages = %d, want 3: %s", len(req.Messages), string(out))
	}
	if req.Messages[0].ReasoningContent == nil || *req.Messages[0].ReasoningContent != "hidden" {
		t.Fatalf("message reasoning_content = %#v, want hidden", req.Messages[0].ReasoningContent)
	}
	if req.Messages[0].Content != "visible" {
		t.Fatalf("visible content = %#v, want visible", req.Messages[0].Content)
	}
	if req.Messages[1].ReasoningContent == nil || *req.Messages[1].ReasoningContent != "" {
		t.Fatalf("function_call reasoning_content = %#v, want explicit empty string", req.Messages[1].ReasoningContent)
	}
}

func TestConvertRequest_ResponsesToolCallsAreImmediatelyFollowedByToolOutputs(t *testing.T) {
	body := []byte(`{
		"model": "deepseek-v4-flash",
		"input": [
			{"type": "function_call", "call_id": "call_1", "name": "read_file", "arguments": "{\"path\":\"README.md\"}"},
			{"type": "message", "role": "developer", "content": "Keep responses concise."},
			{"type": "reasoning", "summary": [{"type": "summary_text", "text": "Need file contents."}]},
			{"type": "function_call_output", "call_id": "call_1", "output": "file contents"},
			{"type": "message", "role": "user", "content": "continue"}
		]
	}`)
	out, err := ConvertRequest(config.ProtocolResponses, config.ProtocolChat, body)
	if err != nil {
		t.Fatalf("responses→chat: %v", err)
	}
	var req ChatRequest
	if err := json.Unmarshal(out, &req); err != nil {
		t.Fatalf("chat output JSON: %v", err)
	}
	if len(req.Messages) < 4 {
		t.Fatalf("messages too short: %#v\n%s", req.Messages, string(out))
	}
	if req.Messages[0].Role != "system" || req.Messages[0].Content != "Keep responses concise." {
		t.Fatalf("developer message should be folded to leading system, got %#v", req.Messages[0])
	}
	assistantIdx := -1
	for i, m := range req.Messages {
		if len(m.ToolCalls) > 0 {
			assistantIdx = i
			break
		}
	}
	if assistantIdx < 0 {
		t.Fatalf("no assistant tool_calls found: %s", string(out))
	}
	assistant := req.Messages[assistantIdx]
	if len(assistant.ToolCalls) != 1 || assistant.ToolCalls[0].ID != "call_1" {
		t.Fatalf("assistant tool_calls = %#v, want call_1", assistant.ToolCalls)
	}
	if assistant.ReasoningContent == nil || *assistant.ReasoningContent != "Need file contents." {
		t.Fatalf("reasoning summary should attach to assistant tool_call, got %#v", assistant.ReasoningContent)
	}
	if assistantIdx+1 >= len(req.Messages) || req.Messages[assistantIdx+1].Role != "tool" || req.Messages[assistantIdx+1].ToolCallID != "call_1" {
		t.Fatalf("assistant tool_calls must be immediately followed by matching tool output: %#v", req.Messages)
	}
}

func TestConvertRequest_ResponsesDropsUnansweredToolCallsForChat(t *testing.T) {
	body := []byte(`{
		"model": "deepseek-v4-flash",
		"input": [
			{"type": "function_call", "call_id": "call_missing", "name": "read_file", "arguments": "{}"},
			{"type": "message", "role": "user", "content": "continue"}
		]
	}`)
	out, err := ConvertRequest(config.ProtocolResponses, config.ProtocolChat, body)
	if err != nil {
		t.Fatalf("responses→chat: %v", err)
	}
	var req ChatRequest
	if err := json.Unmarshal(out, &req); err != nil {
		t.Fatalf("chat output JSON: %v", err)
	}
	for _, m := range req.Messages {
		if len(m.ToolCalls) > 0 || m.Role == "tool" {
			t.Fatalf("unanswered tool call/output should not be forwarded to Chat upstream: %#v\n%s", req.Messages, string(out))
		}
	}
}

func TestConvertRequest_ResponsesCoalescesParallelToolCallsAndOrdersOutputs(t *testing.T) {
	body := []byte(`{
		"model": "deepseek-v4-flash",
		"input": [
			{"type": "reasoning", "summary": [{"type": "summary_text", "text": "Need two tools."}]},
			{"type": "function_call", "call_id": "call_a", "name": "first_tool", "arguments": "{}"},
			{"type": "function_call", "call_id": "call_b", "name": "second_tool", "arguments": "{}"},
			{"type": "function_call_output", "call_id": "call_b", "output": "B"},
			{"type": "function_call_output", "call_id": "call_a", "output": "A"},
			{"type": "message", "role": "user", "content": "continue"}
		]
	}`)
	out, err := ConvertRequest(config.ProtocolResponses, config.ProtocolChat, body)
	if err != nil {
		t.Fatalf("responses→chat: %v", err)
	}
	var req ChatRequest
	if err := json.Unmarshal(out, &req); err != nil {
		t.Fatalf("chat output JSON: %v", err)
	}
	assistantIdx := -1
	for i, m := range req.Messages {
		if len(m.ToolCalls) > 0 {
			assistantIdx = i
			break
		}
	}
	if assistantIdx < 0 || len(req.Messages[assistantIdx].ToolCalls) != 2 {
		t.Fatalf("parallel calls should be coalesced into one assistant message: %#v\n%s", req.Messages, string(out))
	}
	if req.Messages[assistantIdx].ToolCalls[0].ID != "call_a" || req.Messages[assistantIdx].ToolCalls[1].ID != "call_b" {
		t.Fatalf("tool call order = %#v", req.Messages[assistantIdx].ToolCalls)
	}
	if assistantIdx+2 >= len(req.Messages) || req.Messages[assistantIdx+1].ToolCallID != "call_a" || req.Messages[assistantIdx+2].ToolCallID != "call_b" {
		t.Fatalf("tool outputs should immediately follow in tool_call order: %#v", req.Messages)
	}
}

func TestEncodeChatRequest_FiltersInvalidToolCalls(t *testing.T) {
	ir := &IRRequest{
		Model: "m",
		Messages: []IRMessage{
			{
				Role: "assistant",
				ToolCalls: []IRToolCall{
					{ID: "bad", Name: "", Arguments: "{}"},
					{ID: "ok", Name: "valid_tool", Arguments: "{}"},
				},
			},
			{Role: "tool", ToolCallID: "ok", Text: "result"},
		},
	}
	out, err := EncodeChatRequest(ir)
	if err != nil {
		t.Fatalf("encode chat: %v", err)
	}
	var req ChatRequest
	if err := json.Unmarshal(out, &req); err != nil {
		t.Fatalf("chat output JSON: %v", err)
	}
	if len(req.Messages) != 2 || len(req.Messages[0].ToolCalls) != 1 || req.Messages[1].Role != "tool" || req.Messages[1].ToolCallID != "ok" {
		t.Fatalf("tool_calls should be followed by matching tool output: %#v", req.Messages)
	}
	if req.Messages[0].ToolCalls[0].Function.Name != "valid_tool" {
		t.Fatalf("tool call name = %q, want valid_tool", req.Messages[0].ToolCalls[0].Function.Name)
	}
}

// ──────────────────────── Response conversion tests ─────────────────

func TestConvertResponse_SameProtocol(t *testing.T) {
	body := []byte(`{"id":"r1","object":"chat.completion","model":"m","choices":[]}`)
	out, err := ConvertResponse(config.ProtocolChat, config.ProtocolChat, body)
	if err != nil {
		t.Fatalf("same-protocol: %v", err)
	}
	if string(out) != string(body) {
		t.Errorf("same-protocol should return identical body")
	}
}

func TestConvertResponse_ChatToMessages(t *testing.T) {
	body := []byte(`{
		"id": "chatcmpl-123",
		"object": "chat.completion",
		"model": "gpt-4",
		"choices": [{
			"index": 0,
			"message": {"role": "assistant", "content": "Hello!"},
			"finish_reason": "stop"
		}],
		"usage": {"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15}
	}`)
	out, err := ConvertResponse(config.ProtocolChat, config.ProtocolMessages, body)
	if err != nil {
		t.Fatalf("chat→messages: %v", err)
	}

	var resp MsgResponse
	if err := json.Unmarshal(out, &resp); err != nil {
		t.Fatalf("chat→messages: output not valid Messages JSON: %v", err)
	}
	if resp.Type != "message" {
		t.Errorf("type: got %q, want %q", resp.Type, "message")
	}
	if resp.Role != "assistant" {
		t.Errorf("role: got %q, want %q", resp.Role, "assistant")
	}
	if resp.StopReason != "end_turn" {
		t.Errorf("stop_reason: got %q, want %q", resp.StopReason, "end_turn")
	}
	if resp.Usage == nil || resp.Usage.InputTokens != 10 {
		t.Errorf("usage: unexpected %v", resp.Usage)
	}
}

func TestConvertResponse_ChatToResponsesIncludesVisibleText(t *testing.T) {
	body := []byte(`{
		"id": "chatcmpl-123",
		"object": "chat.completion",
		"model": "glm-5.2",
		"choices": [{
			"index": 0,
			"message": {"role": "assistant", "content": "Hello from responses"},
			"finish_reason": "stop"
		}],
		"usage": {"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15}
	}`)
	out, err := ConvertResponse(config.ProtocolChat, config.ProtocolResponses, body)
	if err != nil {
		t.Fatalf("chat→responses: %v", err)
	}

	var resp RespResponse
	if err := json.Unmarshal(out, &resp); err != nil {
		t.Fatalf("chat→responses output JSON: %v", err)
	}
	if resp.OutputText != "Hello from responses" {
		t.Fatalf("output_text = %q, want visible assistant text; body=%s", resp.OutputText, string(out))
	}
	if len(resp.Output) != 1 || len(resp.Output[0].Content) != 1 || resp.Output[0].Content[0].Text != "Hello from responses" {
		t.Fatalf("responses output missing visible content: %#v", resp.Output)
	}
}

func TestDecodeResponsesResponseUsesOutputTextFallback(t *testing.T) {
	body := []byte(`{
		"id": "resp_123",
		"object": "response",
		"model": "glm-5.2",
		"status": "completed",
		"output_text": "fallback text",
		"usage": {"input_tokens": 3, "output_tokens": 2, "total_tokens": 5}
	}`)
	ir, err := DecodeResponsesResponse(body)
	if err != nil {
		t.Fatalf("decode responses: %v", err)
	}
	if len(ir.Choices) != 1 || ir.Choices[0].Message == nil || visibleText(*ir.Choices[0].Message) != "fallback text" {
		t.Fatalf("output_text fallback not preserved: %#v", ir.Choices)
	}
}

func TestConvertResponse_MessagesToChat(t *testing.T) {
	body := []byte(`{
		"id": "msg-123",
		"type": "message",
		"role": "assistant",
		"content": [{"type": "text", "text": "Hi there!"}],
		"model": "claude-sonnet-4.5",
		"stop_reason": "end_turn",
		"usage": {"input_tokens": 8, "output_tokens": 3}
	}`)
	out, err := ConvertResponse(config.ProtocolMessages, config.ProtocolChat, body)
	if err != nil {
		t.Fatalf("messages→chat: %v", err)
	}

	var resp ChatResponse
	if err := json.Unmarshal(out, &resp); err != nil {
		t.Fatalf("messages→chat: output not valid Chat JSON: %v", err)
	}
	if resp.Object != "chat.completion" {
		t.Errorf("object: got %q, want %q", resp.Object, "chat.completion")
	}
	if len(resp.Choices) != 1 {
		t.Fatalf("choices: got %d, want 1", len(resp.Choices))
	}
	if resp.Choices[0].FinishReason != "stop" {
		t.Errorf("finish_reason: got %q, want %q", resp.Choices[0].FinishReason, "stop")
	}
}

// ──────────────────────── IR roundtrip tests ────────────────────────

func TestChatRequestRoundtrip(t *testing.T) {
	original := `{"model":"gpt-4","messages":[{"role":"user","content":"hi"}],"temperature":0.5}`
	ir, err := DecodeChatRequest([]byte(original))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if ir.Model != "gpt-4" {
		t.Errorf("model: got %q", ir.Model)
	}
	if ir.Temperature == nil || *ir.Temperature != 0.5 {
		t.Errorf("temperature: got %v", ir.Temperature)
	}
	out, err := EncodeChatRequest(ir)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	var roundtrip ChatRequest
	if err := json.Unmarshal(out, &roundtrip); err != nil {
		t.Fatalf("roundtrip JSON: %v", err)
	}
	if roundtrip.Model != "gpt-4" {
		t.Errorf("roundtrip model: got %q", roundtrip.Model)
	}
}

func TestMessagesRequestRoundtrip(t *testing.T) {
	original := `{"model":"claude-sonnet-4.5","max_tokens":1024,"messages":[{"role":"user","content":[{"type":"text","text":"hello"}]}]}`
	ir, err := DecodeMessagesRequest([]byte(original))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if ir.Model != "claude-sonnet-4.5" {
		t.Errorf("model: got %q", ir.Model)
	}
	if ir.MaxTokens != 1024 {
		t.Errorf("max_tokens: got %d", ir.MaxTokens)
	}
	out, err := EncodeMessagesRequest(ir)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	var roundtrip MsgRequest
	if err := json.Unmarshal(out, &roundtrip); err != nil {
		t.Fatalf("roundtrip JSON: %v", err)
	}
	if roundtrip.Model != "claude-sonnet-4.5" {
		t.Errorf("roundtrip model: got %q", roundtrip.Model)
	}
}

func TestResponsesRequestRoundtrip(t *testing.T) {
	original := `{"model":"gpt-5","input":"hello world","instructions":"Be brief."}`
	ir, err := DecodeResponsesRequest([]byte(original))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if ir.Model != "gpt-5" {
		t.Errorf("model: got %q", ir.Model)
	}
	if ir.System != "Be brief." {
		t.Errorf("system: got %q", ir.System)
	}
	out, err := EncodeResponsesRequest(ir)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	var roundtrip RespRequest
	if err := json.Unmarshal(out, &roundtrip); err != nil {
		t.Fatalf("roundtrip JSON: %v", err)
	}
	if roundtrip.Model != "gpt-5" {
		t.Errorf("roundtrip model: got %q", roundtrip.Model)
	}
	if roundtrip.Instructions != "Be brief." {
		t.Errorf("roundtrip instructions: got %q", roundtrip.Instructions)
	}
}

// ──────────────────────── Stream event tests ────────────────────────

func TestEncodeDecodeChatStreamChunk(t *testing.T) {
	ev := &IRStreamEvent{
		Type: "completion",
		Choice: &IRChoice{
			Index: 0,
			Delta: &IRMessage{Role: "assistant", Text: "Hello"},
		},
	}
	data, err := EncodeChatStreamChunk(ev)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	decoded, err := DecodeChatStreamChunk(data)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if decoded.Choice == nil || decoded.Choice.Delta == nil {
		t.Fatalf("decoded choice/delta is nil")
	}
	if decoded.Choice.Delta.Text != "Hello" {
		t.Errorf("text: got %q, want %q", decoded.Choice.Delta.Text, "Hello")
	}
}

func TestConvertResponse_ThinkingBlockPreserved(t *testing.T) {
	// Anthropic response with thinking block should be preserved when converting to Messages.
	body := []byte(`{
		"id": "msg-think",
		"type": "message",
		"role": "assistant",
		"content": [
			{"type": "thinking", "thinking": "Let me think about this..."},
			{"type": "text", "text": "The answer is 42."}
		],
		"model": "claude-sonnet-4.5",
		"stop_reason": "end_turn",
		"usage": {"input_tokens": 5, "output_tokens": 10}
	}`)

	// messages → IR → messages roundtrip should preserve thinking.
	ir, err := DecodeMessagesResponse(body)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(ir.Choices) == 0 || ir.Choices[0].Message == nil {
		t.Fatal("no choices in decoded response")
	}
	msg := ir.Choices[0].Message
	hasThinking := false
	for _, c := range msg.Content {
		if c.Type == "thinking" {
			hasThinking = true
			if c.Text != "Let me think about this..." {
				t.Errorf("thinking text: got %q", c.Text)
			}
		}
	}
	if !hasThinking {
		t.Error("thinking block not found in IR")
	}

	// messages → chat should preserve thinking as reasoning_content instead of
	// mixing it into visible content.
	chatOut, err := ConvertResponse(config.ProtocolMessages, config.ProtocolChat, body)
	if err != nil {
		t.Fatalf("messages→chat: %v", err)
	}
	var chatResp ChatResponse
	if err := json.Unmarshal(chatOut, &chatResp); err != nil {
		t.Fatalf("unmarshal chat: %v", err)
	}
	if len(chatResp.Choices) == 0 {
		t.Fatal("no choices in chat response")
	}
	chatMsg := chatResp.Choices[0].Message
	if chatMsg.ReasoningContent == nil || *chatMsg.ReasoningContent != "Let me think about this..." {
		t.Fatalf("chat reasoning_content = %#v, want thinking text", chatMsg.ReasoningContent)
	}
	if chatMsg.Content != "The answer is 42." {
		t.Fatalf("chat visible content = %#v, want final answer only", chatMsg.Content)
	}

	// messages → responses should also preserve thinking out-of-band.
	respOut, err := ConvertResponse(config.ProtocolMessages, config.ProtocolResponses, body)
	if err != nil {
		t.Fatalf("messages→responses: %v", err)
	}
	var respResp RespResponse
	if err := json.Unmarshal(respOut, &respResp); err != nil {
		t.Fatalf("unmarshal responses: %v", err)
	}
	if len(respResp.Output) == 0 {
		t.Fatal("no output in responses")
	}
	foundThinking := false
	for _, item := range respResp.Output {
		if item.ReasoningContent != nil && *item.ReasoningContent == "Let me think about this..." {
			foundThinking = true
		}
	}
	if !foundThinking {
		t.Error("thinking text not found in responses reasoning_content")
	}
}

func TestStreamConverterWithUsageReturnsBufferedUsage(t *testing.T) {
	src := strings.NewReader("data: {\"id\":\"chatcmpl-1\",\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"}}]}\n\n" +
		"data: {\"id\":\"chatcmpl-1\",\"model\":\"m\",\"choices\":[],\"usage\":{\"prompt_tokens\":4,\"completion_tokens\":6,\"total_tokens\":10}}\n\n" +
		"data: [DONE]\n\n")
	var dst bytes.Buffer
	resp, err := StreamConverterWithUsage(&dst, src, config.ProtocolChat, config.ProtocolMessages)
	if err != nil {
		t.Fatalf("StreamConverterWithUsage error: %v", err)
	}
	if resp == nil || resp.Usage == nil || resp.Usage.PromptTokens != 4 || resp.Usage.CompletionTokens != 6 || resp.Usage.TotalTokens != 10 {
		t.Fatalf("unexpected buffered usage: %#v", resp)
	}
	if !strings.Contains(dst.String(), `"type":"message_stop"`) {
		t.Fatalf("converted stream missing message_stop event: %s", dst.String())
	}
}

func TestStreamConverterChatToResponsesEmitsIndexedTextEvents(t *testing.T) {
	src := strings.NewReader("data: {\"id\":\"chatcmpl-1\",\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"}}]}\n\n" +
		"data: {\"id\":\"chatcmpl-1\",\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\" there\"},\"finish_reason\":\"stop\"}]}\n\n" +
		"data: [DONE]\n\n")
	var dst bytes.Buffer
	resp, err := StreamConverterWithUsage(&dst, src, config.ProtocolChat, config.ProtocolResponses)
	if err != nil {
		t.Fatalf("StreamConverterWithUsage error: %v", err)
	}
	if resp == nil || len(resp.Choices) != 1 || resp.Choices[0].Message == nil || visibleText(*resp.Choices[0].Message) != "hi there" {
		t.Fatalf("buffered response missing visible text: %#v", resp)
	}
	out := dst.String()
	for _, want := range []string{
		`"type":"response.content_part.added"`,
		`"type":"response.output_text.delta"`,
		`"output_index":0`,
		`"content_index":0`,
		`"item_id":"msg_0"`,
		`"delta":"hi there"`,
		`"type":"response.content_part.done"`,
		`"type":"response.completed"`,
		`"output_text":"hi there"`,
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("responses stream missing %s:\n%s", want, out)
		}
	}
}

func TestStreamConverterChatToolCallToResponsesFunctionCall(t *testing.T) {
	src := strings.NewReader(
		"data: {\"id\":\"chatcmpl-1\",\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\"}}]}\n\n" +
			"data: {\"id\":\"chatcmpl-1\",\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"type\":\"function\",\"function\":{\"name\":\"read_file\",\"arguments\":\"\"}}]}}]}\n\n" +
			"data: {\"id\":\"chatcmpl-1\",\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"{\\\"path\\\":\"}}]}}]}\n\n" +
			"data: {\"id\":\"chatcmpl-1\",\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"\\\"README.md\\\"}\"}}]},\"finish_reason\":\"tool_calls\"}]}\n\n" +
			"data: [DONE]\n\n")
	var dst bytes.Buffer
	resp, err := StreamConverterWithUsage(&dst, src, config.ProtocolChat, config.ProtocolResponses)
	if err != nil {
		t.Fatalf("StreamConverterWithUsage error: %v", err)
	}
	if resp == nil || len(resp.Choices) != 1 || resp.Choices[0].Message == nil || len(resp.Choices[0].Message.ToolCalls) != 1 {
		t.Fatalf("buffered response missing tool call: %#v", resp)
	}
	tc := resp.Choices[0].Message.ToolCalls[0]
	if tc.ID != "call_1" || tc.Name != "read_file" || tc.Arguments != `{"path":"README.md"}` {
		t.Fatalf("buffered tool call = %#v", tc)
	}
	out := dst.String()
	for _, want := range []string{
		`"type":"function_call"`,
		`"call_id":"call_1"`,
		`"name":"read_file"`,
		`"arguments":"{\"path\":\"README.md\"}"`,
		`"type":"response.function_call_arguments.delta"`,
		`"type":"response.function_call_arguments.done"`,
		`"type":"response.output_item.done"`,
		`"type":"response.completed"`,
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("responses stream missing %s:\n%s", want, out)
		}
	}
	if strings.Contains(out, `"type":"message","role":"assistant","content":null`) {
		t.Fatalf("responses stream should not emit an empty message item before tool call:\n%s", out)
	}
}

func TestStreamConverterChatReasoningToolCallToResponses(t *testing.T) {
	src := strings.NewReader(
		"data: {\"id\":\"chatcmpl-1\",\"model\":\"deepseek-v4-flash\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\"}}]}\n\n" +
			"data: {\"id\":\"chatcmpl-1\",\"model\":\"deepseek-v4-flash\",\"choices\":[{\"index\":0,\"delta\":{\"reasoning_content\":\"Need \"}}]}\n\n" +
			"data: {\"id\":\"chatcmpl-1\",\"model\":\"deepseek-v4-flash\",\"choices\":[{\"index\":0,\"delta\":{\"reasoning_content\":\"tool.\"}}]}\n\n" +
			"data: {\"id\":\"chatcmpl-1\",\"model\":\"deepseek-v4-flash\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"type\":\"function\",\"function\":{\"name\":\"read_file\",\"arguments\":\"{}\"}}]},\"finish_reason\":\"tool_calls\"}]}\n\n" +
			"data: [DONE]\n\n")
	var dst bytes.Buffer
	resp, err := StreamConverterWithUsage(&dst, src, config.ProtocolChat, config.ProtocolResponses)
	if err != nil {
		t.Fatalf("StreamConverterWithUsage error: %v", err)
	}
	if resp == nil || len(resp.Choices) != 1 || resp.Choices[0].Message == nil {
		t.Fatalf("buffered response missing message: %#v", resp)
	}
	if got := thinkingText(*resp.Choices[0].Message); got != "Need tool." {
		t.Fatalf("buffered reasoning = %q, want Need tool.", got)
	}
	out := dst.String()
	for _, want := range []string{
		`"type":"response.reasoning_content.delta"`,
		`"reasoning_content":"Need tool."`,
		`"type":"function_call"`,
		`"call_id":"call_1"`,
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("responses stream missing %s:\n%s", want, out)
		}
	}
	if strings.Contains(out, `"Need tool.","type":"output_text"`) {
		t.Fatalf("reasoning should not be emitted as output_text:\n%s", out)
	}
}

func TestEncodeDecodeMessagesStreamEvent(t *testing.T) {
	ev := &IRStreamEvent{
		Type:         "content_block_delta",
		ContentDelta: "Hi there",
		Choice: &IRChoice{
			Index: 0,
			Delta: &IRMessage{Content: []IRContent{{Type: "text", Text: "Hi there"}}},
		},
	}
	data, err := EncodeMessagesStreamEvent(ev)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	decoded, err := DecodeMessagesStreamEvent(data)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if decoded.Type != "content_block_delta" {
		t.Errorf("type: got %q", decoded.Type)
	}
	if decoded.ContentDelta != "Hi there" {
		t.Errorf("content_delta: got %q", decoded.ContentDelta)
	}
}

// Messages allows content to be a plain string shorthand. The decoder must
// accept that form; previously it failed with "cannot unmarshal string into
// []MsgContent" which broke all Anthropic Messages → Chat conversions for
// clients that use the string shorthand.
func TestDecodeMessagesRequestAcceptsStringContent(t *testing.T) {
	body := []byte(`{
		"model": "claude",
		"max_tokens": 16,
		"messages": [
			{"role": "user", "content": "Hello"},
			{"role": "assistant", "content": "Hi there"}
		]
	}`)
	ir, err := DecodeMessagesRequest(body)
	if err != nil {
		t.Fatalf("string content should decode: %v", err)
	}
	if len(ir.Messages) != 2 {
		t.Fatalf("messages: got %d, want 2", len(ir.Messages))
	}
	if ir.Messages[0].Text != "Hello" {
		t.Errorf("user text: got %q, want %q", ir.Messages[0].Text, "Hello")
	}
	if ir.Messages[1].Text != "Hi there" {
		t.Errorf("assistant text: got %q, want %q", ir.Messages[1].Text, "Hi there")
	}
}

// A tool_result block may carry its payload as content: "..." or content:
// [{type:"text",...}]. The previous decoder dropped that payload entirely,
// so converting such a Messages request to Chat lost the tool output.
func TestDecodeMessagesRequestPreservesToolResultContent(t *testing.T) {
	body := []byte(`{
		"model": "claude",
		"max_tokens": 16,
		"messages": [
			{"role": "user", "content": "use the tool"},
			{"role": "assistant", "content": [{"type": "tool_use", "id": "t1", "name": "get", "input": {"q": "x"}}]},
			{"role": "user", "content": [{"type": "tool_result", "tool_use_id": "t1", "content": "result text"}]}
		]
	}`)
	ir, err := DecodeMessagesRequest(body)
	if err != nil {
		t.Fatalf("tool_result request should decode: %v", err)
	}
	var found bool
	for _, m := range ir.Messages {
		for _, c := range m.Content {
			if c.Type == "tool_result" && c.ToolID == "t1" && c.Text == "result text" {
				found = true
			}
		}
	}
	if !found {
		t.Fatalf("tool_result content text was lost: %#v", ir.Messages)
	}

	// And the array form of content.
	bodyArray := []byte(`{
		"model": "claude",
		"max_tokens": 16,
		"messages": [
			{"role": "user", "content": [{"type": "tool_result", "tool_use_id": "t1", "content": [{"type":"text","text":"array result"}]}]}
		]
	}`)
	ir2, err := DecodeMessagesRequest(bodyArray)
	if err != nil {
		t.Fatalf("tool_result array content should decode: %v", err)
	}
	found = false
	for _, m := range ir2.Messages {
		for _, c := range m.Content {
			if c.Type == "tool_result" && c.ToolID == "t1" && c.Text == "array result" {
				found = true
			}
		}
	}
	if !found {
		t.Fatalf("tool_result array content text was lost: %#v", ir2.Messages)
	}
}

// Converting a Messages request with string content to Chat must succeed end
// to end and produce a usable Chat body.
func TestConvertRequest_MessagesToChatStringContent(t *testing.T) {
	body := []byte(`{
		"model": "gpt-4",
		"messages": [
			{"role": "user", "content": "Hello"}
		],
		"max_tokens": 512
	}`)
	out, err := ConvertRequest(config.ProtocolMessages, config.ProtocolChat, body)
	if err != nil {
		t.Fatalf("messages→chat with string content: %v", err)
	}
	var req ChatRequest
	if err := json.Unmarshal(out, &req); err != nil {
		t.Fatalf("output not valid Chat JSON: %v", err)
	}
	if len(req.Messages) != 1 {
		t.Fatalf("messages: got %d, want 1", len(req.Messages))
	}
	if req.Messages[0].Role != "user" {
		t.Errorf("role: got %q", req.Messages[0].Role)
	}
	if s, ok := req.Messages[0].Content.(string); !ok || s != "Hello" {
		t.Errorf("content: got %#v, want string \"Hello\"", req.Messages[0].Content)
	}
}

// DecodeStreamBuffer must reject an upstream payload that is not a valid
// stream for its protocol (e.g. an HTML gateway error page) so the proxy can
// surface a clean error instead of an opaque JSON parse failure.
func TestDecodeStreamBufferRejectsHTMLBody(t *testing.T) {
	html := []byte("<html><body>502 Bad gateway\x1b[0m</body></html>")
	if _, err := DecodeStreamBuffer(config.ProtocolChat, html); err == nil {
		t.Fatalf("expected error decoding HTML as chat stream")
	}
}

func TestConvertResponseRejectsCompressedBody(t *testing.T) {
	compressed := []byte{0x1f, 0x8b, 0x08, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0xff, 0x3c, 0x00}
	if _, err := ConvertResponse(config.ProtocolChat, config.ProtocolMessages, compressed); err == nil {
		t.Fatalf("expected error converting compressed body")
	}
}

func TestDecodeStreamBufferRejectsEmptyBody(t *testing.T) {
	if _, err := DecodeStreamBuffer(config.ProtocolChat, nil); err == nil {
		t.Fatalf("expected error decoding empty body as chat stream")
	}
	if _, err := DecodeStreamBuffer(config.ProtocolChat, []byte{}); err == nil {
		t.Fatalf("expected error decoding empty byte slice as chat stream")
	}
}

// A stream that only contains [DONE] is valid SSE (just an empty response);
// the proxy should parse it. This differs from HTML which has no data: lines.
func TestDecodeStreamBufferAcceptsDoneOnlyStream(t *testing.T) {
	emptyStream := []byte("data: [DONE]\n\n")
	if _, err := DecodeStreamBuffer(config.ProtocolChat, emptyStream); err != nil {
		t.Fatalf("unexpected error parsing valid done-only stream: %v", err)
	}
}

func TestDecodeStreamBufferAcceptsValidChatStream(t *testing.T) {
	validStream := []byte("data: {\"id\":\"chatcmpl-1\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	resp, err := DecodeStreamBuffer(config.ProtocolChat, validStream)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(resp.Choices) == 0 || resp.Choices[0].Message == nil || resp.Choices[0].Message.Text != "hi" {
		t.Fatalf("unexpected response: %+v", resp)
	}
}

func TestStripToolChoiceForReasoning(t *testing.T) {
	// Reasoning model with required tool_choice: should strip it to avoid
	// "Thinking mode does not support this tool_choice" from the upstream.
	body := []byte(`{"model":"deepseek-v4-flash","messages":[],"tools":[{"type":"function","function":{"name":"read"}}],"tool_choice":"required"}`)
	out, ok := StripToolChoiceForReasoning(body, "deepseek-v4-flash")
	if !ok {
		t.Fatalf("expected tool_choice to be stripped for reasoning model")
	}
	var req map[string]any
	if err := json.Unmarshal(out, &req); err != nil {
		t.Fatalf("rewritten body is not valid JSON: %v", err)
	}
	if _, exists := req["tool_choice"]; exists {
		t.Fatalf("tool_choice should have been removed: %s", string(out))
	}
	// Other fields must survive.
	if req["model"] != "deepseek-v4-flash" {
		t.Fatalf("model field was lost: %s", string(out))
	}
	if _, ok := req["tools"]; !ok {
		t.Fatalf("tools field was lost: %s", string(out))
	}

	// Non-reasoning model: tool_choice should be preserved.
	plainBody := []byte(`{"model":"glm-51","messages":[],"tool_choice":"required"}`)
	if _, ok := StripToolChoiceForReasoning(plainBody, "glm-51"); ok {
		t.Fatalf("tool_choice should NOT be stripped for non-reasoning models")
	}

	// Reasoning model without tool_choice: no-op (ok=false).
	noChoice := []byte(`{"model":"deepseek-v4-flash","messages":[]}`)
	if _, ok := StripToolChoiceForReasoning(noChoice, "deepseek-v4-flash"); ok {
		t.Fatalf("no-op expected when tool_choice is absent")
	}
}

// Cross-protocol response conversion must preserve cache-read, cache-creation
// and reasoning token counts carried in upstream usage, not just
// prompt/completion/total.
func TestConvertResponsePreservesCacheAndReasoningTokens(t *testing.T) {
	chatBody := []byte(`{"id":"1","object":"chat.completion","model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":100,"completion_tokens":10,"total_tokens":110,"prompt_tokens_details":{"cached_tokens":40},"completion_tokens_details":{"reasoning_tokens":5}}}`)

	// Chat → Messages
	msgOut, err := ConvertResponse(config.ProtocolChat, config.ProtocolMessages, chatBody)
	if err != nil {
		t.Fatalf("chat→messages: %v", err)
	}
	var msgResp MsgResponse
	if err := json.Unmarshal(msgOut, &msgResp); err != nil {
		t.Fatalf("messages JSON: %v", err)
	}
	if msgResp.Usage == nil {
		t.Fatal("messages response missing usage")
	}
	if msgResp.Usage.CacheReadTokens != 40 {
		t.Fatalf("messages cache_read = %d, want 40", msgResp.Usage.CacheReadTokens)
	}
	if msgResp.Usage.ReasoningTokens != 5 {
		t.Fatalf("messages reasoning = %d, want 5", msgResp.Usage.ReasoningTokens)
	}

	// Chat → Responses
	respOut, err := ConvertResponse(config.ProtocolChat, config.ProtocolResponses, chatBody)
	if err != nil {
		t.Fatalf("chat→responses: %v", err)
	}
	var respResp RespResponse
	if err := json.Unmarshal(respOut, &respResp); err != nil {
		t.Fatalf("responses JSON: %v", err)
	}
	if respResp.Usage == nil || respResp.Usage.InputTokensDetails == nil {
		t.Fatalf("responses usage missing input_tokens_details: %#v", respResp.Usage)
	}
	if respResp.Usage.InputTokensDetails.CachedTokens != 40 {
		t.Fatalf("responses cached_tokens = %d, want 40", respResp.Usage.InputTokensDetails.CachedTokens)
	}
	if respResp.Usage.OutputTokensDetails == nil || respResp.Usage.OutputTokensDetails.ReasoningTokens != 5 {
		t.Fatalf("responses reasoning_tokens missing: %#v", respResp.Usage.OutputTokensDetails)
	}
}

// Messages → Chat must also carry cache/reasoning tokens through IR.
func TestConvertMessagesResponsePreservesCacheTokens(t *testing.T) {
	msgBody := []byte(`{"id":"1","type":"message","role":"assistant","content":[{"type":"text","text":"hi"}],"model":"m","stop_reason":"end_turn","usage":{"input_tokens":100,"output_tokens":10,"cache_read_input_tokens":40,"cache_creation_input_tokens":8,"reasoning_tokens":5}}`)
	chatOut, err := ConvertResponse(config.ProtocolMessages, config.ProtocolChat, msgBody)
	if err != nil {
		t.Fatalf("messages→chat: %v", err)
	}
	var chatResp ChatResponse
	if err := json.Unmarshal(chatOut, &chatResp); err != nil {
		t.Fatalf("chat JSON: %v", err)
	}
	if chatResp.Usage == nil {
		t.Fatal("chat response missing usage")
	}
	if chatResp.Usage.PromptTokensDetails == nil || chatResp.Usage.PromptTokensDetails.CachedTokens != 40 {
		t.Fatalf("chat cached_tokens missing: %#v", chatResp.Usage.PromptTokensDetails)
	}
	if chatResp.Usage.CompletionTokensDetails == nil || chatResp.Usage.CompletionTokensDetails.ReasoningTokens != 5 {
		t.Fatalf("chat reasoning_tokens missing: %#v", chatResp.Usage.CompletionTokensDetails)
	}
}

// Streaming chat → responses must propagate cache/reasoning token counts into
// the emitted response.completed event's usage block.
func TestStreamConverterChatUsageDetailsPreservedToResponses(t *testing.T) {
	src := strings.NewReader("data: {\"id\":\"chatcmpl-1\",\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"}}]}\n\n" +
		"data: {\"id\":\"chatcmpl-1\",\"model\":\"m\",\"choices\":[],\"usage\":{\"prompt_tokens\":100,\"completion_tokens\":10,\"total_tokens\":110,\"prompt_tokens_details\":{\"cached_tokens\":40},\"completion_tokens_details\":{\"reasoning_tokens\":5}}}\n\n" +
		"data: [DONE]\n\n")
	var dst bytes.Buffer
	resp, err := StreamConverterWithUsage(&dst, src, config.ProtocolChat, config.ProtocolResponses)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if resp == nil || resp.Usage == nil {
		t.Fatal("usage lost across stream conversion")
	}
	if resp.Usage.CacheReadTokens != 40 {
		t.Fatalf("IR cache read = %d, want 40", resp.Usage.CacheReadTokens)
	}
	if resp.Usage.ReasoningTokens != 5 {
		t.Fatalf("IR reasoning = %d, want 5", resp.Usage.ReasoningTokens)
	}
	if !strings.Contains(dst.String(), "cached_tokens") {
		t.Fatalf("responses stream missing cached_tokens:\n%s", dst.String())
	}
}

// DecodeResponsesSSE must keep every parallel function_call item's id/name
// instead of overwriting index 0. See the multi-tool index bug fix.
func TestDecodeResponsesSSEPreservesMultipleToolCalls(t *testing.T) {
	src := strings.NewReader(
		`data: {"type":"response.created","response":{"id":"resp_1","model":"m"}}` + "\n\n" +
			`data: {"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","id":"fc_0","call_id":"call_0","name":"a","arguments":""}}` + "\n\n" +
			`data: {"type":"response.function_call_arguments.delta","output_index":0,"delta":"{}"}` + "\n\n" +
			`data: {"type":"response.output_item.done","output_index":0,"item":{"type":"function_call","id":"fc_0","call_id":"call_0","name":"a","arguments":"{}"}}` + "\n\n" +
			`data: {"type":"response.output_item.added","output_index":1,"item":{"type":"function_call","id":"fc_1","call_id":"call_1","name":"b","arguments":""}}` + "\n\n" +
			`data: {"type":"response.function_call_arguments.delta","output_index":1,"delta":"{}"}` + "\n\n" +
			`data: {"type":"response.output_item.done","output_index":1,"item":{"type":"function_call","id":"fc_1","call_id":"call_1","name":"b","arguments":"{}"}}` + "\n\n" +
			`data: {"type":"response.completed","response":{"id":"resp_1","model":"m","usage":{"input_tokens":1,"output_tokens":2,"total_tokens":3}}}` + "\n\n" +
			"data: [DONE]\n\n")
	resp, err := DecodeResponsesSSE(src)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(resp.Choices) != 1 || resp.Choices[0].Message == nil {
		t.Fatalf("no message: %#v", resp)
	}
	tcs := resp.Choices[0].Message.ToolCalls
	if len(tcs) != 2 {
		t.Fatalf("tool calls = %d, want 2: %#v", len(tcs), tcs)
	}
	if tcs[0].ID != "call_0" || tcs[0].Name != "a" {
		t.Fatalf("tool[0] = %#v, want call_0/a", tcs[0])
	}
	if tcs[1].ID != "call_1" || tcs[1].Name != "b" {
		t.Fatalf("tool[1] = %#v, want call_1/b", tcs[1])
	}
}

// emitMessagesStream must not panic when a choice carries no message
// (e.g. the upstream returned a tool-only payload that was already drained).
func TestEmitMessagesStreamDoesNotPanicOnNilMessage(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("emitMessagesStream panicked on nil Message: %v", r)
		}
	}()
	resp := &IRResponse{ID: "resp_1", Model: "m", Choices: []IRChoice{{Index: 0}}}
	var out strings.Builder
	if err := emitMessagesStream(&out, resp); err != nil {
		t.Fatalf("err: %v", err)
	}
	if !strings.Contains(out.String(), "message_stop") {
		t.Fatalf("messages stream missing message_stop: %s", out.String())
	}
	// An empty message must not emit a tool_use content block.
	if strings.Contains(out.String(), "tool_use") {
		t.Fatalf("empty message emitted tool_use: %s", out.String())
	}
}

// ─────────────── cross-protocol tool_use regression tests ───────────────

// wantMsgContent returns the content blocks of the single choice from a
// Chat → Messages response conversion, failing the test on shape errors.
func wantMsgContent(t *testing.T, body []byte) []MsgContent {
	t.Helper()
	out, err := ConvertResponse(config.ProtocolChat, config.ProtocolMessages, body)
	if err != nil {
		t.Fatalf("chat→messages: %v", err)
	}
	var resp MsgResponse
	if err := json.Unmarshal(out, &resp); err != nil {
		t.Fatalf("chat→messages output not valid Messages JSON: %v\n%s", err, string(out))
	}
	if resp.StopReason != "tool_use" {
		t.Fatalf("stop_reason = %q, want %q (body=%s)", resp.StopReason, "tool_use", string(out))
	}
	return resp.Content
}

// TestConvertResponse_ChatToMessagesToolCallsOnly verifies that a Chat
// response carrying only tool_calls (and no visible content) produces an
// Anthropic tool_use block instead of an empty content array.
func TestConvertResponse_ChatToMessagesToolCallsOnly(t *testing.T) {
	body := []byte(`{
		"id": "chatcmpl-1",
		"object": "chat.completion",
		"model": "glm-5.2",
		"choices": [{
			"index": 0,
			"message": {
				"role": "assistant",
				"tool_calls": [{
					"id": "call_1",
					"type": "function",
					"function": {"name": "get_weather", "arguments": "{\"city\":\"sf\"}"}
				}]
			},
			"finish_reason": "tool_calls"
		}],
		"usage": {"prompt_tokens": 5, "completion_tokens": 3, "total_tokens": 8}
	}`)
	content := wantMsgContent(t, body)
	if len(content) != 1 {
		t.Fatalf("expected 1 tool_use block, got %d: %#v", len(content), content)
	}
	if content[0].Type != "tool_use" {
		t.Fatalf("block type = %q, want tool_use", content[0].Type)
	}
	if content[0].ID != "call_1" || content[0].Name != "get_weather" {
		t.Fatalf("tool_use id/name = %q/%q, want call_1/get_weather", content[0].ID, content[0].Name)
	}
	if string(content[0].Input) != `{"city":"sf"}` {
		t.Fatalf("tool_use input = %q, want {\"city\":\"sf\"}", string(content[0].Input))
	}
}

// TestConvertResponse_ChatToMessagesTextAndToolCalls verifies that a Chat
// response carrying both visible text content and tool_calls preserves the
// text as a text block — not dropping it once tool calls are present.
func TestConvertResponse_ChatToMessagesTextAndToolCalls(t *testing.T) {
	body := []byte(`{
		"id": "chatcmpl-2",
		"object": "chat.completion",
		"model": "deepseek-v4-pro",
		"choices": [{
			"index": 0,
			"message": {
				"role": "assistant",
				"content": "Let me check the weather for you.",
				"tool_calls": [{
					"id": "call_2",
					"type": "function",
					"function": {"name": "get_weather", "arguments": "{\"city\":\"sf\"}"}
				}]
			},
			"finish_reason": "tool_calls"
		}],
		"usage": {"prompt_tokens": 5, "completion_tokens": 3, "total_tokens": 8}
	}`)
	content := wantMsgContent(t, body)
	if len(content) != 2 {
		t.Fatalf("expected text + tool_use (2 blocks), got %d: %#v", len(content), content)
	}
	if content[0].Type != "text" || content[0].Text != "Let me check the weather for you." {
		t.Fatalf("first block = %#v, want text block with visible content", content[0])
	}
	if content[1].Type != "tool_use" || content[1].ID != "call_2" || content[1].Name != "get_weather" {
		t.Fatalf("second block = %#v, want tool_use call_2/get_weather", content[1])
	}
}

// TestIrMsgToMsgContentRoundtripNoDuplicateToolUse verifies that a Messages
// assistant response with a tool_use block survives a Messages → IR →
// Messages roundtrip without being duplicated. msgContentToIRMessage stores
// tool_use in both Content and ToolCalls, so irMsgToMsgContent must not
// re-emit a block that was already emitted from Content.
func TestIrMsgToMsgContentRoundtripNoDuplicateToolUse(t *testing.T) {
	original := []MsgContent{
		{Type: "text", Text: "Calling a tool."},
		{Type: "tool_use", ID: "tu_1", Name: "get_weather", Input: json.RawMessage(`{"city":"sf"}`)},
	}
	ir := msgContentToIRMessage(original)
	if len(ir.ToolCalls) != 1 || ir.ToolCalls[0].ID != "tu_1" {
		t.Fatalf("IR should carry the tool call, got %#v", ir.ToolCalls)
	}
	roundtripped := irMsgToMsgContent(ir)
	if len(roundtripped) != 2 {
		t.Fatalf("expected 2 blocks after roundtrip, got %d: %#v", len(roundtripped), roundtripped)
	}
	if roundtripped[0].Type != "text" || roundtripped[1].Type != "tool_use" {
		t.Fatalf("unexpected block ordering: %#v", roundtripped)
	}
	// Count tool_use blocks — must be exactly one despite ToolCalls mirroring Content.
	toolUseCount := 0
	for _, b := range roundtripped {
		if b.Type == "tool_use" {
			toolUseCount++
		}
	}
	if toolUseCount != 1 {
		t.Fatalf("expected exactly 1 tool_use block after roundtrip, got %d: %#v", toolUseCount, roundtripped)
	}
	if roundtripped[1].ID != "tu_1" || roundtripped[1].Name != "get_weather" {
		t.Fatalf("duplicated/lost tool_use identity: %#v", roundtripped[1])
	}
}

// TestIrMsgToMsgContentDedupeSeparatesDistinctToolUseIDs verifies that when
// Content holds one tool_use block and ToolCalls holds a different one
// (e.g. mixed-origin IR), both distinct calls are emitted — only the
// already-emitted one is skipped.
func TestIrMsgToMsgContentDedupeSeparatesDistinctToolUseIDs(t *testing.T) {
	m := IRMessage{
		Role: "assistant",
		Content: []IRContent{
			{Type: "tool_use", ID: "tu_a", Name: "fn_a", Input: json.RawMessage(`{}`)},
		},
		ToolCalls: []IRToolCall{
			{ID: "tu_a", Name: "fn_a", Arguments: `{}`}, // duplicate id → skip
			{ID: "tu_b", Name: "fn_b", Arguments: `{}`}, // distinct → emit
		},
	}
	out := irMsgToMsgContent(m)
	ids := map[string]bool{}
	for _, b := range out {
		if b.Type == "tool_use" {
			ids[b.ID] = true
		}
	}
	if len(ids) != 2 || !ids["tu_a"] || !ids["tu_b"] {
		t.Fatalf("expected both distinct tool_use ids preserved, got %#v -> %#v", ids, out)
	}
}
