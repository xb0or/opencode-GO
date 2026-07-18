package protocol

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/xb0or/opencode-GO/config"
)

// ---------------------------------------------------------------------------
// Round-3 audit verification tests.
//
// These tests exercise the cross-protocol streaming conversion engine
// (StreamConvertIncremental) directly — no HTTP, no api package. They cover
// the round-3 audit findings P0-1, P0-2, P1-1, P1-3, P1-4, P1-5, P1-6.
// ---------------------------------------------------------------------------

// runConvert is a small helper that invokes StreamConvertIncremental with the
// full upstream stream passed as firstEvent (rest is empty). onFirstEvent is
// passed through; pass nil when you don't care.
func runConvert(t *testing.T, upProto, dstProto config.Protocol, upstream string, onFirstEvent func() error) (*IRResponse, error, *bytes.Buffer) {
	t.Helper()
	var dst bytes.Buffer
	flush := func() {}
	rest := strings.NewReader("")
	resp, err := StreamConvertIncremental(string(upProto), string(dstProto), []byte(upstream), rest, &dst, flush, onFirstEvent)
	return resp, err, &dst
}

// parseMessagesFrames parses Anthropic Messages SSE output into a list of
// data-line JSON objects (one map per frame). Each Messages frame is:
//
//	event: <type>\n
//	data: <json>\n\n
func parseMessagesFrames(t *testing.T, out string) []map[string]any {
	t.Helper()
	var frames []map[string]any
	for _, block := range strings.Split(out, "\n\n") {
		block = strings.TrimSpace(block)
		if block == "" {
			continue
		}
		for _, line := range strings.Split(block, "\n") {
			line = strings.TrimSpace(line)
			if !strings.HasPrefix(line, "data: ") {
				continue
			}
			payload := line[len("data: "):]
			var m map[string]any
			if err := json.Unmarshal([]byte(payload), &m); err != nil {
				t.Fatalf("parseMessagesFrames: invalid JSON in data line: %v\nline: %s", err, payload)
			}
			frames = append(frames, m)
			break
		}
	}
	return frames
}

// ──────────────────────── P0-1: EOF without terminal ────────────────

// TestR3_P0_1_EOFWOTerminalNoSuccess: upstream (messages) sends a content delta
// then a clean EOF with NO message_stop. The conversion must surface an
// unexpected-EOF error and must NOT emit success terminal events ([DONE] or a
// finish_reason chunk) on the chat target.
func TestR3_P0_1_EOFWOTerminalNoSuccess(t *testing.T) {
	upstream := "data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"hi\"}}\n\n"
	resp, err, dst := runConvert(t, config.ProtocolMessages, config.ProtocolChat, upstream, nil)
	_ = resp
	if err == nil {
		t.Fatalf("expected io.ErrUnexpectedEOF, got nil error")
	}
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("expected error wrapping io.ErrUnexpectedEOF, got: %v", err)
	}
	out := dst.String()
	if strings.Contains(out, "[DONE]") {
		t.Fatalf("output must NOT contain [DONE] on truncated stream:\n%s", out)
	}
	if strings.Contains(out, "finish_reason") {
		t.Fatalf("output must NOT contain finish_reason on truncated stream:\n%s", out)
	}
}

// ──────────────────────── P0-2: empty / error objects, no onStart ───

// TestR3_P0_2_EmptyObjectNoStart: upstream (messages) sends `data: {}` then
// EOF. The messages decoder rejects the empty object; onFirstEvent must NOT be
// called (no commit).
func TestR3_P0_2_EmptyObjectNoStart(t *testing.T) {
	upstream := "data: {}\n\n"
	called := false
	onFirst := func() error {
		called = true
		return nil
	}
	_, err, dst := runConvert(t, config.ProtocolMessages, config.ProtocolChat, upstream, onFirst)
	if err == nil {
		t.Fatalf("expected error for empty object, got nil")
	}
	if called {
		t.Fatalf("onFirstEvent must NOT be called for empty object; stream committed prematurely")
	}
	if dst.Len() != 0 {
		t.Fatalf("output buffer must be empty when no valid event was emitted, got:\n%s", dst.String())
	}
}

// TestR3_P0_2_ErrorObjectNoStart: upstream (messages) sends an error object
// then EOF. The decoder must surface the error message and NOT call
// onFirstEvent.
func TestR3_P0_2_ErrorObjectNoStart(t *testing.T) {
	upstream := "data: {\"error\":{\"message\":\"overloaded\"}}\n\n"
	called := false
	onFirst := func() error {
		called = true
		return nil
	}
	_, err, dst := runConvert(t, config.ProtocolMessages, config.ProtocolChat, upstream, onFirst)
	if err == nil {
		t.Fatalf("expected error for error object, got nil")
	}
	if !strings.Contains(err.Error(), "overloaded") {
		t.Fatalf("error must contain \"overloaded\", got: %v", err)
	}
	if called {
		t.Fatalf("onFirstEvent must NOT be called for error object; stream committed prematurely")
	}
	if dst.Len() != 0 {
		t.Fatalf("output buffer must be empty when no valid event was emitted, got:\n%s", dst.String())
	}
}

// ──────────────────────── P1-1: interleaved tool blocks ─────────────

// TestR3_P1_1_InterleavedToolBlocks: upstream (chat) sends two interleaved
// tool calls (start0, start1, delta0, delta1, delta0, delta1). Target =
// messages. The Messages output must have TWO content_block_start events with
// tool_use, with distinct indices, and the partial_json deltas must route to
// the correct block index (Taipei on index 0, news on index 1).
func TestR3_P1_1_InterleavedToolBlocks(t *testing.T) {
	upstream := strings.Join([]string{
		`data: {"id":"x","model":"m","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_0","function":{"name":"get_weather"}}]}}]}` + "\n\n",
		`data: {"id":"x","model":"m","choices":[{"index":0,"delta":{"tool_calls":[{"index":1,"id":"call_1","function":{"name":"search"}}]}}]}` + "\n\n",
		`data: {"id":"x","model":"m","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"city\":"}}]}}]}` + "\n\n",
		`data: {"id":"x","model":"m","choices":[{"index":0,"delta":{"tool_calls":[{"index":1,"function":{"arguments":"{\"q\":"}}]}}]}` + "\n\n",
		`data: {"id":"x","model":"m","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"Taipei\"}"}}]}}]}` + "\n\n",
		`data: {"id":"x","model":"m","choices":[{"index":0,"delta":{"tool_calls":[{"index":1,"function":{"arguments":"\"news\"}"}}]}}]}` + "\n\n",
		`data: {"id":"x","model":"m","choices":[{"index":0,"finish_reason":"tool_calls"}]}` + "\n\n",
		`data: [DONE]` + "\n\n",
	}, "")

	_, err, dst := runConvert(t, config.ProtocolChat, config.ProtocolMessages, upstream, nil)
	if err != nil {
		t.Fatalf("StreamConvertIncremental error: %v", err)
	}
	frames := parseMessagesFrames(t, dst.String())

	// Collect content_block_start frames whose content_block.type == "tool_use".
	var toolStarts []map[string]any
	for _, f := range frames {
		if f["type"] != "content_block_start" {
			continue
		}
		cb, _ := f["content_block"].(map[string]any)
		if cb == nil {
			continue
		}
		if cb["type"] == "tool_use" {
			toolStarts = append(toolStarts, f)
		}
	}
	if len(toolStarts) != 2 {
		t.Fatalf("expected 2 tool_use content_block_start events, got %d:\n%s", len(toolStarts), dst.String())
	}

	// Verify distinct block indices.
	indices := map[int]bool{}
	for _, f := range toolStarts {
		idx, _ := f["index"].(float64)
		indices[int(idx)] = true
	}
	if len(indices) != 2 {
		t.Fatalf("expected 2 distinct tool block indices, got %v", indices)
	}
	if !indices[0] || !indices[1] {
		t.Fatalf("expected tool block indices {0,1}, got %v", indices)
	}

	// For content_block_delta frames with input_json_delta, verify routing:
	// the delta containing "Taipei" must be on index 0, the one containing
	// "news" must be on index 1.
	taipeiBlockIdx := -1
	newsBlockIdx := -1
	for _, f := range frames {
		if f["type"] != "content_block_delta" {
			continue
		}
		delta, _ := f["delta"].(map[string]any)
		if delta == nil || delta["type"] != "input_json_delta" {
			continue
		}
		pj, _ := delta["partial_json"].(string)
		idx, _ := f["index"].(float64)
		i := int(idx)
		if strings.Contains(pj, "Taipei") {
			taipeiBlockIdx = i
		}
		if strings.Contains(pj, "news") {
			newsBlockIdx = i
		}
	}
	if taipeiBlockIdx != 0 {
		t.Fatalf("expected Taipei partial_json on block index 0, got %d\n%s", taipeiBlockIdx, dst.String())
	}
	if newsBlockIdx != 1 {
		t.Fatalf("expected news partial_json on block index 1, got %d\n%s", newsBlockIdx, dst.String())
	}
}

// ──────────────────────── P1-3: no duplicate args ────────────────────

// TestR3_P1_3_NoDuplicateArgs: upstream (responses) sends a tool call with
// argument deltas followed by a done event carrying the full arguments.
// Target = chat. The chat output must NOT duplicate the arguments — the
// accumulated delta arguments must be exactly `{"city":"Tokyo"}` (count 1),
// not doubled by the done event.
func TestR3_P1_3_NoDuplicateArgs(t *testing.T) {
	upstream := strings.Join([]string{
		`data: {"type":"response.created","response":{"id":"resp_1","model":"m"}}` + "\n\n",
		`data: {"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","id":"fc_0","call_id":"call_0","name":"get_weather","arguments":""}}` + "\n\n",
		`data: {"type":"response.function_call_arguments.delta","output_index":0,"delta":"{\"city\":"}` + "\n\n",
		`data: {"type":"response.function_call_arguments.delta","output_index":0,"delta":"\"Tokyo\"}"}` + "\n\n",
		`data: {"type":"response.function_call_arguments.done","output_index":0,"item":{"type":"function_call","id":"fc_0","call_id":"call_0","name":"get_weather","arguments":"{\"city\":\"Tokyo\"}"}}` + "\n\n",
		`data: {"type":"response.completed","response":{"id":"resp_1","model":"m"}}` + "\n\n",
		`data: [DONE]` + "\n\n",
	}, "")

	_, err, dst := runConvert(t, config.ProtocolResponses, config.ProtocolChat, upstream, nil)
	if err != nil {
		t.Fatalf("StreamConvertIncremental error: %v", err)
	}

	// Decode the chat output to get the accumulated tool call arguments.
	accResp, decErr := DecodeChatSSE(dst)
	if decErr != nil {
		t.Fatalf("DecodeChatSSE of chat output error: %v", decErr)
	}
	if accResp == nil || len(accResp.Choices) == 0 || accResp.Choices[0].Message == nil {
		t.Fatalf("decoded chat response missing message: %#v", accResp)
	}
	msg := accResp.Choices[0].Message
	if len(msg.ToolCalls) == 0 {
		t.Fatalf("decoded chat response missing tool call: %#v", msg)
	}
	args := msg.ToolCalls[0].Arguments
	want := `{"city":"Tokyo"}`
	if args != want {
		t.Fatalf("accumulated arguments = %q, want %q (no duplication)", args, want)
	}
	if got := strings.Count(args, `"city":"Tokyo"`); got != 1 {
		t.Fatalf("occurrences of \"city\":\"Tokyo\" in accumulated args = %d, want 1 (args=%q)", got, args)
	}
}

// ──────────────────────── P1-4: finish + usage same chunk ────────────

// TestR3_P1_4_FinishUsageSameChunk: upstream (chat) sends finish_reason and
// usage in the SAME chunk. Target = messages. The message_delta event must
// carry the usage (output_tokens = 20).
func TestR3_P1_4_FinishUsageSameChunk(t *testing.T) {
	upstream := strings.Join([]string{
		`data: {"id":"x","model":"m","choices":[{"index":0,"delta":{"content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":20,"total_tokens":30}}` + "\n\n",
		`data: [DONE]` + "\n\n",
	}, "")

	_, err, dst := runConvert(t, config.ProtocolChat, config.ProtocolMessages, upstream, nil)
	if err != nil {
		t.Fatalf("StreamConvertIncremental error: %v", err)
	}
	out := dst.String()
	// The message_delta must carry usage with output_tokens=20.
	if !strings.Contains(out, `"output_tokens":20`) {
		t.Fatalf("messages output missing usage with output_tokens=20:\n%s", out)
	}
	// Verify a message_delta event was emitted with the usage.
	frames := parseMessagesFrames(t, out)
	var foundMessageDeltaWithUsage bool
	for _, f := range frames {
		if f["type"] != "message_delta" {
			continue
		}
		if u, ok := f["usage"].(map[string]any); ok {
			if ot, _ := u["output_tokens"].(float64); ot == 20 {
				foundMessageDeltaWithUsage = true
			}
		}
	}
	if !foundMessageDeltaWithUsage {
		t.Fatalf("no message_delta event with usage output_tokens=20 found:\n%s", out)
	}
}

// ──────────────────────── P1-5: finish reason normalization ──────────

// TestR3_P1_5_FinishReasonNormalized_MessagesToChat: upstream (messages) sends
// stop_reason "end_turn"; target=chat must emit finish_reason "stop", NOT
// "end_turn".
func TestR3_P1_5_FinishReasonNormalized_MessagesToChat(t *testing.T) {
	upstream := strings.Join([]string{
		`data: {"type":"message_start","message":{"id":"msg_1","model":"m"}}` + "\n\n",
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}` + "\n\n",
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi"}}` + "\n\n",
		`data: {"type":"content_block_stop","index":0}` + "\n\n",
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"}}` + "\n\n",
		`data: {"type":"message_stop"}` + "\n\n",
	}, "")

	_, err, dst := runConvert(t, config.ProtocolMessages, config.ProtocolChat, upstream, nil)
	if err != nil {
		t.Fatalf("StreamConvertIncremental error: %v", err)
	}
	out := dst.String()
	if !strings.Contains(out, `"finish_reason":"stop"`) {
		t.Fatalf("chat output must contain finish_reason \"stop\":\n%s", out)
	}
	if strings.Contains(out, `"finish_reason":"end_turn"`) {
		t.Fatalf("chat output must NOT contain finish_reason \"end_turn\":\n%s", out)
	}
}

// TestR3_P1_5_FinishReasonNormalized_ChatToMessages: upstream (chat) sends
// finish_reason "stop"; target=messages must emit stop_reason "end_turn", NOT
// "stop".
func TestR3_P1_5_FinishReasonNormalized_ChatToMessages(t *testing.T) {
	upstream := strings.Join([]string{
		`data: {"id":"x","model":"m","choices":[{"index":0,"delta":{"content":"hi"}}]}` + "\n\n",
		`data: {"id":"x","model":"m","choices":[{"index":0,"finish_reason":"stop"}]}` + "\n\n",
		`data: [DONE]` + "\n\n",
	}, "")

	_, err, dst := runConvert(t, config.ProtocolChat, config.ProtocolMessages, upstream, nil)
	if err != nil {
		t.Fatalf("StreamConvertIncremental error: %v", err)
	}
	out := dst.String()
	if !strings.Contains(out, `"stop_reason":"end_turn"`) {
		t.Fatalf("messages output must contain stop_reason \"end_turn\":\n%s", out)
	}
	if strings.Contains(out, `"stop_reason":"stop"`) {
		t.Fatalf("messages output must NOT contain stop_reason \"stop\":\n%s", out)
	}
}

// ──────────────────────── P1-6: chat id/model preserved ──────────────

// TestR3_P1_6_ChatIDModelPreserved: upstream (chat) sends the first chunk with
// id and model. Target = messages. The Messages message_start event must
// carry the original id and model.
func TestR3_P1_6_ChatIDModelPreserved(t *testing.T) {
	upstream := strings.Join([]string{
		`data: {"id":"chatcmpl-xyz","model":"gpt-4","choices":[{"index":0,"delta":{"content":"hi"}}]}` + "\n\n",
		`data: {"id":"chatcmpl-xyz","model":"gpt-4","choices":[{"index":0,"finish_reason":"stop"}]}` + "\n\n",
		`data: [DONE]` + "\n\n",
	}, "")

	_, err, dst := runConvert(t, config.ProtocolChat, config.ProtocolMessages, upstream, nil)
	if err != nil {
		t.Fatalf("StreamConvertIncremental error: %v", err)
	}
	out := dst.String()
	if !strings.Contains(out, `"id":"chatcmpl-xyz"`) {
		t.Fatalf("messages output must preserve id \"chatcmpl-xyz\":\n%s", out)
	}
	if !strings.Contains(out, `"model":"gpt-4"`) {
		t.Fatalf("messages output must preserve model \"gpt-4\":\n%s", out)
	}
	// Verify the message_start event specifically carries id and model.
	frames := parseMessagesFrames(t, out)
	var foundStart bool
	for _, f := range frames {
		if f["type"] != "message_start" {
			continue
		}
		msg, _ := f["message"].(map[string]any)
		if msg == nil {
			continue
		}
		if id, _ := msg["id"].(string); id == "chatcmpl-xyz" {
			if model, _ := msg["model"].(string); model == "gpt-4" {
				foundStart = true
			}
		}
	}
	if !foundStart {
		t.Fatalf("no message_start event carrying id=chatcmpl-xyz and model=gpt-4 found:\n%s", out)
	}
}