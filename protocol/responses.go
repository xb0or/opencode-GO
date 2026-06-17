package protocol

import (
	"encoding/json"
	"fmt"
)

// ──────────────────────── OpenAI Responses API ──────────────────────

// RespRequest is the wire format for POST /v1/responses.
type RespRequest struct {
	Model        string          `json:"model"`
	Input        json.RawMessage `json:"input"` // string | []RespInputItem
	Stream       bool            `json:"stream,omitempty"`
	Temperature  *float64        `json:"temperature,omitempty"`
	MaxTokens    int             `json:"max_output_tokens,omitempty"`
	Tools        []RespTool      `json:"tools,omitempty"`
	ToolChoice   json.RawMessage `json:"tool_choice,omitempty"`
	TopP         *float64        `json:"top_p,omitempty"`
	Instructions string          `json:"instructions,omitempty"`
}

type RespInputItem struct {
	Role      string          `json:"role,omitempty"`
	Type      string          `json:"type,omitempty"`    // message | function_call | function_call_output
	Content   json.RawMessage `json:"content,omitempty"` // string | []RespContent
	Name      string          `json:"name,omitempty"`
	CallID    string          `json:"call_id,omitempty"`
	Output    string          `json:"output,omitempty"`
	Arguments string          `json:"arguments,omitempty"`
}

type RespContent struct {
	Type string `json:"type"` // input_text | output_text
	Text string `json:"text,omitempty"`
}

type RespTool struct {
	Type        string          `json:"type"` // function
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
	Strict      bool            `json:"strict,omitempty"`
}

// RespResponse is the wire format for a non-streaming Responses API response.
type RespResponse struct {
	ID           string           `json:"id"`
	Object       string           `json:"object"` // response
	Model        string           `json:"model"`
	Output       []RespOutputItem `json:"output"`
	Usage        *RespUsage       `json:"usage,omitempty"`
	Status       string           `json:"status,omitempty"`
	Instructions string           `json:"instructions,omitempty"`
}

type RespOutputItem struct {
	Type      string        `json:"type"` // message | function_call
	ID        string        `json:"id,omitempty"`
	Role      string        `json:"role,omitempty"`
	Content   []RespContent `json:"content,omitempty"`
	Name      string        `json:"name,omitempty"`
	CallID    string        `json:"call_id,omitempty"`
	Arguments string        `json:"arguments,omitempty"`
}

type RespUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
	TotalTokens  int `json:"total_tokens"`
}

// RespStreamEvent is one SSE event for the Responses API streaming format.
type RespStreamEvent struct {
	Type           string          `json:"type"`
	Response       *RespResponse   `json:"response,omitempty"`
	OutputIndex    int             `json:"output_index,omitempty"`
	ContentIndex   int             `json:"content_index,omitempty"`
	Delta          string          `json:"delta,omitempty"`
	Item           *RespOutputItem `json:"item,omitempty"`
	SequenceNumber int             `json:"sequence_number,omitempty"`
}

// ──────────────────────── Decoders ──────────────────────────────────

// DecodeResponsesRequest parses an OpenAI Responses request body into IR.
func DecodeResponsesRequest(data []byte) (*IRRequest, error) {
	var req RespRequest
	if err := json.Unmarshal(data, &req); err != nil {
		return nil, fmt.Errorf("responses: decode request: %w", err)
	}
	ir := &IRRequest{
		Model:       req.Model,
		Temperature: req.Temperature,
		MaxTokens:   req.MaxTokens,
		Stream:      req.Stream,
		ToolChoice:  req.ToolChoice,
		TopP:        req.TopP,
	}
	if req.Instructions != "" {
		ir.System = req.Instructions
	}
	// Parse input field (string or array).
	var inputStr string
	if err := json.Unmarshal(req.Input, &inputStr); err == nil {
		ir.Messages = append(ir.Messages, IRMessage{Role: "user", Text: inputStr})
	} else {
		var items []RespInputItem
		if err := json.Unmarshal(req.Input, &items); err != nil {
			return nil, fmt.Errorf("responses: parse input: %w", err)
		}
		for _, item := range items {
			ir.Messages = append(ir.Messages, respItemToIR(item))
		}
	}
	for _, t := range req.Tools {
		ir.Tools = append(ir.Tools, IRTool{
			Name:        t.Name,
			Description: t.Description,
			Parameters:  t.Parameters,
			Strict:      t.Strict,
		})
	}
	return ir, nil
}

// EncodeResponsesRequest serializes an IR request into Responses API wire format.
func EncodeResponsesRequest(ir *IRRequest) ([]byte, error) {
	req := RespRequest{
		Model:       ir.Model,
		Temperature: ir.Temperature,
		MaxTokens:   ir.MaxTokens,
		Stream:      ir.Stream,
		ToolChoice:  normalizeToolChoiceForResponses(ir.ToolChoice),
		TopP:        ir.TopP,
	}
	// Collect system instructions from both ir.System and system-role messages.
	instructions := ir.System
	var items []RespInputItem
	for _, m := range ir.Messages {
		if isSystemLikeRole(m.Role) {
			// Fold system messages into instructions.
			text := m.Text
			if text == "" && len(m.Content) > 0 {
				for _, c := range m.Content {
					if c.Type == "text" {
						text += c.Text
					}
				}
			}
			if text != "" {
				if instructions != "" {
					instructions += "\n"
				}
				instructions += text
			}
			continue
		}
		items = append(items, irMsgToRespItem(m))
	}
	req.Instructions = instructions
	inputBytes, _ := json.Marshal(items)
	req.Input = inputBytes
	for _, t := range ir.Tools {
		req.Tools = append(req.Tools, RespTool{
			Type:        "function",
			Name:        t.Name,
			Description: t.Description,
			Parameters:  t.Parameters,
			Strict:      t.Strict,
		})
	}
	return json.Marshal(req)
}

// DecodeResponsesResponse parses an OpenAI Responses response into IR.
func DecodeResponsesResponse(data []byte) (*IRResponse, error) {
	var resp RespResponse
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, fmt.Errorf("responses: decode response: %w", err)
	}
	ir := &IRResponse{
		ID:    resp.ID,
		Model: resp.Model,
	}
	if resp.Usage != nil {
		ir.Usage = &IRUsage{
			PromptTokens:     resp.Usage.InputTokens,
			CompletionTokens: resp.Usage.OutputTokens,
			TotalTokens:      resp.Usage.TotalTokens,
		}
	}
	for _, item := range resp.Output {
		msg := respOutputToIR(item)
		ir.Choices = append(ir.Choices, IRChoice{
			Index:        0,
			Message:      &msg,
			FinishReason: mapFinishReason(resp.Status),
		})
	}
	return ir, nil
}

// EncodeResponsesResponse serializes an IR response into Responses API wire format.
func EncodeResponsesResponse(ir *IRResponse) ([]byte, error) {
	resp := RespResponse{
		ID:     ir.ID,
		Object: "response",
		Model:  ir.Model,
		Status: "completed",
	}
	if ir.Usage != nil {
		resp.Usage = &RespUsage{
			InputTokens:  ir.Usage.PromptTokens,
			OutputTokens: ir.Usage.CompletionTokens,
			TotalTokens:  ir.Usage.TotalTokens,
		}
	}
	for _, ch := range ir.Choices {
		if ch.Message != nil {
			resp.Output = append(resp.Output, irMsgToRespOutput(*ch.Message))
		}
	}
	return json.Marshal(resp)
}

// DecodeResponsesStreamEvent parses a Responses SSE event into an IRStreamEvent.
func DecodeResponsesStreamEvent(data []byte) (*IRStreamEvent, error) {
	var ev RespStreamEvent
	if err := json.Unmarshal(data, &ev); err != nil {
		return nil, fmt.Errorf("responses: decode stream event: %w", err)
	}
	ir := &IRStreamEvent{Type: ev.Type}
	switch ev.Type {
	case "response.created":
		if ev.Response != nil {
			ir.Response = &IRResponse{ID: ev.Response.ID, Model: ev.Response.Model}
		}
	case "response.output_item.added":
		if ev.Item != nil {
			msg := respOutputToIR(*ev.Item)
			ir.Choice = &IRChoice{Index: ev.OutputIndex, Delta: &msg}
		}
	case "response.content_part.added":
		// Text content part started
	case "response.output_text.delta":
		ir.Choice = &IRChoice{Index: ev.OutputIndex, Delta: &IRMessage{Role: "assistant"}}
		ir.ContentDelta = ev.Delta
	case "response.output_text.done":
		// Text complete
	case "response.function_call_arguments.delta":
		ir.Choice = &IRChoice{Index: ev.OutputIndex, Delta: &IRMessage{Role: "assistant"}}
		ir.ToolCallDelta = &IRToolCallDelta{Index: ev.OutputIndex, Arguments: ev.Delta}
	case "response.function_call_arguments.done":
		if ev.Item != nil {
			ir.Choice = &IRChoice{Index: ev.OutputIndex, Delta: &IRMessage{
				ToolCalls: []IRToolCall{{ID: ev.Item.CallID, Name: ev.Item.Name, Arguments: ev.Item.Arguments}},
			}}
		}
	case "response.output_item.done":
		if ev.Item != nil {
			msg := respOutputToIR(*ev.Item)
			fin := "stop"
			if ev.Item.Type == "function_call" {
				fin = "tool_calls"
			}
			ir.Choice = &IRChoice{Index: ev.OutputIndex, Message: &msg, FinishReason: fin}
		}
	case "response.completed":
		if ev.Response != nil {
			ir.Response = &IRResponse{ID: ev.Response.ID, Model: ev.Response.Model}
			if ev.Response.Usage != nil {
				ir.Response.Usage = &IRUsage{
					PromptTokens:     ev.Response.Usage.InputTokens,
					CompletionTokens: ev.Response.Usage.OutputTokens,
					TotalTokens:      ev.Response.Usage.TotalTokens,
				}
			}
		}
	case "response.incomplete":
		ir.Choice = &IRChoice{Index: 0, FinishReason: "length"}
	}
	return ir, nil
}

// EncodeResponsesStreamEvent serializes an IRStreamEvent into Responses SSE events.
func EncodeResponsesStreamEvent(ev *IRStreamEvent) ([]byte, error) {
	switch ev.Type {
	case "response.created":
		var resp *RespResponse
		if ev.Response != nil {
			resp = &RespResponse{ID: ev.Response.ID, Object: "response", Model: ev.Response.Model, Status: "in_progress"}
		}
		return json.Marshal(RespStreamEvent{Type: "response.created", Response: resp})

	case "response.output_item.added":
		item := &RespOutputItem{Type: "message", Role: "assistant"}
		if ev.Choice != nil && ev.Choice.Delta != nil && len(ev.Choice.Delta.ToolCalls) > 0 {
			tc := ev.Choice.Delta.ToolCalls[0]
			item = &RespOutputItem{Type: "function_call", Name: tc.Name, CallID: tc.ID}
		}
		idx := 0
		if ev.Choice != nil {
			idx = ev.Choice.Index
		}
		return json.Marshal(RespStreamEvent{Type: "response.output_item.added", OutputIndex: idx, Item: item})

	case "response.content_part.added":
		idx := 0
		if ev.Choice != nil {
			idx = ev.Choice.Index
		}
		return json.Marshal(RespStreamEvent{Type: "response.content_part.added", OutputIndex: idx})

	case "response.output_text.delta":
		idx := 0
		if ev.Choice != nil {
			idx = ev.Choice.Index
		}
		return json.Marshal(RespStreamEvent{Type: "response.output_text.delta", OutputIndex: idx, Delta: ev.ContentDelta})

	case "response.output_text.done":
		idx := 0
		if ev.Choice != nil {
			idx = ev.Choice.Index
		}
		return json.Marshal(RespStreamEvent{Type: "response.output_text.done", OutputIndex: idx})

	case "response.function_call_arguments.delta":
		idx := 0
		delta := ""
		if ev.ToolCallDelta != nil {
			idx = ev.ToolCallDelta.Index
			delta = ev.ToolCallDelta.Arguments
		}
		return json.Marshal(RespStreamEvent{Type: "response.function_call_arguments.delta", OutputIndex: idx, Delta: delta})

	case "response.function_call_arguments.done":
		idx := 0
		if ev.Choice != nil {
			idx = ev.Choice.Index
		}
		return json.Marshal(RespStreamEvent{Type: "response.function_call_arguments.done", OutputIndex: idx})

	case "response.output_item.done":
		idx := 0
		if ev.Choice != nil {
			idx = ev.Choice.Index
		}
		return json.Marshal(RespStreamEvent{Type: "response.output_item.done", OutputIndex: idx})

	case "response.completed":
		var resp *RespResponse
		if ev.Response != nil {
			resp = &RespResponse{ID: ev.Response.ID, Object: "response", Model: ev.Response.Model, Status: "completed"}
			if ev.Response.Usage != nil {
				resp.Usage = &RespUsage{
					InputTokens:  ev.Response.Usage.PromptTokens,
					OutputTokens: ev.Response.Usage.CompletionTokens,
					TotalTokens:  ev.Response.Usage.TotalTokens,
				}
			}
		}
		return json.Marshal(RespStreamEvent{Type: "response.completed", Response: resp})

	case "response.incomplete":
		return json.Marshal(RespStreamEvent{Type: "response.incomplete"})
	}
	return json.Marshal(RespStreamEvent{Type: ev.Type})
}

// ──────────────────────── helpers ───────────────────────────────────

func respItemToIR(item RespInputItem) IRMessage {
	ir := IRMessage{Role: item.Role}
	if item.Type == "function_call_output" {
		ir.Role = "tool"
		ir.ToolCallID = item.CallID
		ir.Text = item.Output
		return ir
	}
	if item.Type == "function_call" {
		ir.Role = "assistant"
		ir.ToolCalls = append(ir.ToolCalls, IRToolCall{ID: item.CallID, Name: item.Name, Arguments: item.Arguments})
		return ir
	}
	// Parse content (string or array).
	var s string
	if err := json.Unmarshal(item.Content, &s); err == nil {
		ir.Text = s
		return ir
	}
	var parts []RespContent
	if err := json.Unmarshal(item.Content, &parts); err == nil {
		for _, p := range parts {
			ir.Content = append(ir.Content, IRContent{Type: "text", Text: p.Text})
		}
		if len(ir.Content) == 1 {
			ir.Text = ir.Content[0].Text
		}
	}
	return ir
}

func irMsgToRespItem(m IRMessage) RespInputItem {
	if m.Role == "tool" || m.ToolCallID != "" {
		text := m.Text
		if text == "" && len(m.Content) > 0 {
			text = m.Content[0].Text
		}
		return RespInputItem{Type: "function_call_output", CallID: m.ToolCallID, Output: text}
	}
	if len(m.ToolCalls) > 0 {
		tc := m.ToolCalls[0]
		return RespInputItem{Type: "function_call", CallID: tc.ID, Name: tc.Name, Arguments: tc.Arguments}
	}
	item := RespInputItem{Role: m.Role, Type: "message"}
	if m.Text != "" && len(m.Content) == 0 {
		content, _ := json.Marshal(m.Text)
		item.Content = content
	} else {
		var parts []RespContent
		for _, c := range m.Content {
			if c.Type == "text" {
				parts = append(parts, RespContent{Type: "input_text", Text: c.Text})
			}
		}
		if len(parts) == 0 {
			parts = append(parts, RespContent{Type: "input_text", Text: m.Text})
		}
		content, _ := json.Marshal(parts)
		item.Content = content
	}
	return item
}

func respOutputToIR(item RespOutputItem) IRMessage {
	ir := IRMessage{Role: "assistant"}
	if item.Type == "function_call" {
		ir.ToolCalls = append(ir.ToolCalls, IRToolCall{ID: item.CallID, Name: item.Name, Arguments: item.Arguments})
		return ir
	}
	for _, c := range item.Content {
		ir.Content = append(ir.Content, IRContent{Type: "text", Text: c.Text})
	}
	if len(ir.Content) == 1 {
		ir.Text = ir.Content[0].Text
	}
	return ir
}

func irMsgToRespOutput(m IRMessage) RespOutputItem {
	if len(m.ToolCalls) > 0 {
		tc := m.ToolCalls[0]
		return RespOutputItem{Type: "function_call", CallID: tc.ID, Name: tc.Name, Arguments: tc.Arguments}
	}
	item := RespOutputItem{Type: "message", Role: "assistant"}
	if m.Text != "" && len(m.Content) == 0 {
		item.Content = []RespContent{{Type: "output_text", Text: m.Text}}
	} else {
		for _, c := range m.Content {
			switch c.Type {
			case "text":
				item.Content = append(item.Content, RespContent{Type: "output_text", Text: c.Text})
			case "thinking":
				// Thinking blocks from Anthropic are included as text in Responses
				// format since Responses API has no native thinking type.
				item.Content = append(item.Content, RespContent{Type: "output_text", Text: c.Text})
			}
		}
	}
	return item
}

func mapFinishReason(status string) string {
	switch status {
	case "completed":
		return "stop"
	case "incomplete":
		return "length"
	default:
		return status
	}
}
