package protocol

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/xb0or/opencode-GO/config"
)

// ---------------------------------------------------------------------------
// Round-5 audit verification tests (protocol-level).
//
// P0-1: response.failed → onError (no completed+[DONE]); response.incomplete
//       → response.incomplete event (not response.completed, no [DONE]).
// P0-2: Legitimate Responses lifecycle events (in_progress, output_text.done,
//       content_part.done, etc.) are safely ignored, not fatal errors.
// P1-2: Cross-protocol usage total_tokens recalculated when upstream sends
//       input and output tokens in separate events.
// ---------------------------------------------------------------------------

// TestR5_P0_1_ResponseFailedNoCompleted verifies that an upstream
// response.failed event does NOT produce a response.completed or [DONE] on
// the target side. The stream should be truncated via onError.
func TestR5_P0_1_ResponseFailedNoCompleted(t *testing.T) {
	upstream := strings.Join([]string{
		`data: {"type":"response.created","response":{"id":"resp_1","model":"m"}}`,
		``,
		`data: {"type":"response.failed","response":{"id":"resp_1","status":"failed","error":{"message":"overloaded"}}}`,
		``,
	}, "\n")

	// Target: chat
	_, err, dst := runConvert(t, config.ProtocolResponses, config.ProtocolChat, upstream, nil)
	output := dst.String()

	// The conversion should NOT emit [DONE] or a success finish_reason.
	if strings.Contains(output, "[DONE]") {
		t.Errorf("P0-1 FAIL: output contains [DONE] for a failed response:\n%s", output)
	}
	if strings.Contains(output, `"finish_reason":"stop"`) {
		t.Errorf("P0-1 FAIL: output contains finish_reason=stop for a failed response:\n%s", output)
	}
	// An error or nil is acceptable — the key is no fake success terminal.
	_ = err
}

// TestR5_P0_1_ResponseIncompleteEmitsIncomplete verifies that an upstream
// response.incomplete event produces response.incomplete (not
// response.completed) on the target side, with no [DONE].
func TestR5_P0_1_ResponseIncompleteEmitsIncomplete(t *testing.T) {
	upstream := strings.Join([]string{
		`data: {"type":"response.created","response":{"id":"resp_1","model":"m"}}`,
		``,
		`data: {"type":"response.output_text.delta","delta":"hi"}`,
		``,
		`data: {"type":"response.incomplete","response":{"id":"resp_1","status":"incomplete"}}`,
		``,
	}, "\n")

	// Target: responses (same protocol — test the emitter logic)
	_, err, dst := runConvert(t, config.ProtocolResponses, config.ProtocolResponses, upstream, nil)
	output := dst.String()

	if err != nil {
		t.Logf("conversion returned err=%v (acceptable for incomplete)", err)
	}

	if !strings.Contains(output, "response.incomplete") {
		t.Errorf("P0-1 FAIL: output should contain response.incomplete:\n%s", output)
	}
	if strings.Contains(output, "response.completed") {
		t.Errorf("P0-1 FAIL: output should NOT contain response.completed for incomplete:\n%s", output)
	}
	if strings.Contains(output, "[DONE]") {
		t.Errorf("P0-1 FAIL: output should NOT contain [DONE] for incomplete:\n%s", output)
	}
}

// TestR5_P0_2_LifecycleEventsIgnored verifies that legitimate Responses API
// lifecycle events (response.in_progress, response.output_text.done,
// response.content_part.done) do NOT cause fatal decoder errors.
func TestR5_P0_2_LifecycleEventsIgnored(t *testing.T) {
	upstream := strings.Join([]string{
		`data: {"type":"response.created","response":{"id":"resp_1","model":"m"}}`,
		``,
		`data: {"type":"response.in_progress","response":{"id":"resp_1"}}`,
		``,
		`data: {"type":"response.output_text.delta","delta":"hi"}`,
		``,
		`data: {"type":"response.output_text.done","text":"hi"}`,
		``,
		`data: {"type":"response.content_part.done","part":{"type":"output_text","text":"hi"}}`,
		``,
		`data: {"type":"response.completed","response":{"id":"resp_1","model":"m","usage":{"input_tokens":5,"output_tokens":3,"total_tokens":8}}}`,
		``,
		`data: [DONE]`,
		``,
	}, "\n")

	resp, err, dst := runConvert(t, config.ProtocolResponses, config.ProtocolChat, upstream, nil)
	if err != nil {
		t.Fatalf("P0-2 FAIL: conversion error from lifecycle events: %v", err)
	}
	output := dst.String()

	if !strings.Contains(output, "hi") {
		t.Errorf("P0-2 FAIL: output missing content 'hi':\n%s", output)
	}
	if !strings.Contains(output, `"finish_reason":"stop"`) && !strings.Contains(output, `"finish_reason": "stop"`) {
		t.Errorf("P0-2 FAIL: output missing finish_reason=stop:\n%s", output)
	}
	if !strings.Contains(output, "[DONE]") {
		t.Errorf("P0-2 FAIL: output missing [DONE]:\n%s", output)
	}
	if resp == nil || resp.Usage == nil || resp.Usage.TotalTokens != 8 {
		t.Errorf("P0-2 FAIL: usage not propagated, got resp=%+v", resp)
	}
}

// TestR5_P1_2_UsageTotalRecalculated verifies that when Messages API sends
// input_tokens in message_start and output_tokens in message_delta (neither
// carries total_tokens), the conversion recalculates total_tokens = input +
// output.
func TestR5_P1_2_UsageTotalRecalculated(t *testing.T) {
	upstream := strings.Join([]string{
		`event: message_start`,
		`data: {"type":"message_start","message":{"id":"msg_1","model":"m","usage":{"input_tokens":10}}}`,
		``,
		`event: content_block_start`,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi"}}`,
		``,
		`event: content_block_stop`,
		`data: {"type":"content_block_stop","index":0}`,
		``,
		`event: message_delta`,
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":5}}`,
		``,
		`event: message_stop`,
		`data: {"type":"message_stop"}`,
		``,
	}, "\n")

	resp, err, dst := runConvert(t, config.ProtocolMessages, config.ProtocolChat, upstream, nil)
	if err != nil {
		t.Fatalf("P1-2 FAIL: conversion error: %v", err)
	}

	if resp == nil || resp.Usage == nil {
		t.Fatalf("P1-2 FAIL: no usage in response, resp=%+v", resp)
	}
	if resp.Usage.TotalTokens != 15 {
		t.Errorf("P1-2 FAIL: total_tokens=%d, want 15 (input=10 + output=5). Usage=%+v",
			resp.Usage.TotalTokens, resp.Usage)
	}

	// Also verify the output chat SSE contains the correct total.
	output := dst.String()
	// Chat usage is in the final chunk.
	var foundTotal int
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		payload := strings.TrimPrefix(line, "data: ")
		if payload == "[DONE]" {
			continue
		}
		var chunk map[string]json.RawMessage
		if json.Unmarshal([]byte(payload), &chunk) != nil {
			continue
		}
		if u, ok := chunk["usage"]; ok {
			var usage struct {
				TotalTokens int `json:"total_tokens"`
			}
			if json.Unmarshal(u, &usage) == nil && usage.TotalTokens > 0 {
				foundTotal = usage.TotalTokens
			}
		}
	}
	if foundTotal != 15 {
		t.Errorf("P1-2 FAIL: output SSE usage total_tokens=%d, want 15", foundTotal)
	}
}

// TestR5_P1_2_ChatToMessagesUsageFields verifies that Chat→Messages
// conversion preserves usage fields (input_tokens, output_tokens).
func TestR5_P1_2_ChatToMessagesUsageFields(t *testing.T) {
	upstream := strings.Join([]string{
		`data: {"id":"chat_1","object":"chat.completion.chunk","model":"m","choices":[{"index":0,"delta":{"content":"hi"},"finish_reason":null}]}`,
		``,
		`data: {"id":"chat_1","object":"chat.completion.chunk","model":"m","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":20,"total_tokens":30}}`,
		``,
		`data: [DONE]`,
		``,
	}, "\n")

	resp, err, dst := runConvert(t, config.ProtocolChat, config.ProtocolMessages, upstream, nil)
	if err != nil {
		t.Fatalf("P1-2 FAIL: conversion error: %v", err)
	}

	if resp == nil || resp.Usage == nil {
		t.Fatalf("P1-2 FAIL: no usage, resp=%+v", resp)
	}
	if resp.Usage.PromptTokens != 10 {
		t.Errorf("P1-2 FAIL: prompt_tokens=%d, want 10", resp.Usage.PromptTokens)
	}
	if resp.Usage.CompletionTokens != 20 {
		t.Errorf("P1-2 FAIL: completion_tokens=%d, want 20", resp.Usage.CompletionTokens)
	}

	// Check the Messages SSE output contains the usage in message_delta.
	output := dst.String()
	frames := parseMessagesFrames(t, output)
	var foundOutput int
	for _, f := range frames {
		if t, ok := f["type"].(string); ok && t == "message_delta" {
			if u, ok := f["usage"].(map[string]any); ok {
				if ot, ok := u["output_tokens"].(float64); ok {
					foundOutput = int(ot)
				}
			}
		}
	}
	if foundOutput != 20 {
		t.Errorf("P1-2 FAIL: Messages output_tokens=%d, want 20", foundOutput)
	}
}