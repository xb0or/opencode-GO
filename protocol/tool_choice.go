package protocol

import (
	"encoding/json"
	"strings"
)

// normalizeToolChoiceForChat converts source-protocol tool_choice values into
// the OpenAI Chat shape. Default "auto" choices are omitted because providers
// already default to auto when tools are present, and some OpenAI-compatible
// backends reject the Anthropic object form {"type":"auto"}.
func normalizeToolChoiceForChat(raw json.RawMessage) json.RawMessage {
	return normalizeToolChoiceForOpenAI(raw)
}

// normalizeToolChoiceForResponses converts source-protocol tool_choice values
// into the OpenAI Responses shape, which follows the same string/function
// object convention for the choices we support here.
func normalizeToolChoiceForResponses(raw json.RawMessage) json.RawMessage {
	return normalizeToolChoiceForOpenAI(raw)
}

// normalizeToolChoiceForMessages converts OpenAI Chat/Responses tool_choice
// values into the Anthropic Messages shape.
func normalizeToolChoiceForMessages(raw json.RawMessage) json.RawMessage {
	raw = compactRawMessage(raw)
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	if s, ok := rawString(raw); ok {
		switch strings.ToLower(strings.TrimSpace(s)) {
		case "", "auto":
			return nil
		case "required":
			return mustRaw(map[string]any{"type": "any"})
		case "none":
			return mustRaw(map[string]any{"type": "none"})
		default:
			return raw
		}
	}

	obj, ok := rawObject(raw)
	if !ok {
		return raw
	}
	switch strings.ToLower(stringValue(obj["type"])) {
	case "", "auto":
		return nil
	case "required", "any":
		return mustRaw(map[string]any{"type": "any"})
	case "none":
		return mustRaw(map[string]any{"type": "none"})
	case "function":
		if name := functionChoiceName(obj); name != "" {
			return mustRaw(map[string]any{"type": "tool", "name": name})
		}
	case "tool":
		if name := stringValue(obj["name"]); name != "" {
			return mustRaw(map[string]any{"type": "tool", "name": name})
		}
	}
	return raw
}

func normalizeToolChoiceForOpenAI(raw json.RawMessage) json.RawMessage {
	raw = compactRawMessage(raw)
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	if s, ok := rawString(raw); ok {
		switch strings.ToLower(strings.TrimSpace(s)) {
		case "", "auto":
			return nil
		case "none", "required":
			return mustRaw(strings.ToLower(strings.TrimSpace(s)))
		default:
			return raw
		}
	}

	obj, ok := rawObject(raw)
	if !ok {
		return raw
	}
	switch strings.ToLower(stringValue(obj["type"])) {
	case "", "auto":
		return nil
	case "none":
		return mustRaw("none")
	case "any", "required":
		return mustRaw("required")
	case "tool", "function":
		if name := functionChoiceName(obj); name != "" {
			return mustRaw(map[string]any{
				"type":     "function",
				"function": map[string]any{"name": name},
			})
		}
	}
	return raw
}

func functionChoiceName(obj map[string]any) string {
	if name := stringValue(obj["name"]); name != "" {
		return name
	}
	if fn, ok := obj["function"].(map[string]any); ok {
		return stringValue(fn["name"])
	}
	return ""
}

func compactRawMessage(raw json.RawMessage) json.RawMessage {
	raw = json.RawMessage(strings.TrimSpace(string(raw)))
	if len(raw) == 0 {
		return nil
	}
	return raw
}

func rawString(raw json.RawMessage) (string, bool) {
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return "", false
	}
	return s, true
}

func rawObject(raw json.RawMessage) (map[string]any, bool) {
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil, false
	}
	return obj, true
}

func stringValue(v any) string {
	s, _ := v.(string)
	return strings.TrimSpace(s)
}

func mustRaw(v any) json.RawMessage {
	raw, _ := json.Marshal(v)
	return raw
}
