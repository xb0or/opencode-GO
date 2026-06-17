package protocol

import "strings"

func appendIRToolIfValid(tools []IRTool, tool IRTool) []IRTool {
	if cleaned, ok := cleanIRTool(tool); ok {
		return append(tools, cleaned)
	}
	return tools
}

func cleanIRTools(tools []IRTool) []IRTool {
	out := make([]IRTool, 0, len(tools))
	seen := map[string]bool{}
	for _, tool := range tools {
		cleaned, ok := cleanIRTool(tool)
		if !ok || seen[cleaned.Name] {
			continue
		}
		seen[cleaned.Name] = true
		out = append(out, cleaned)
	}
	return out
}

func cleanIRTool(tool IRTool) (IRTool, bool) {
	tool.Name = strings.TrimSpace(tool.Name)
	if !isValidToolName(tool.Name) {
		return IRTool{}, false
	}
	return tool, true
}

func isFunctionToolType(toolType string) bool {
	switch strings.ToLower(strings.TrimSpace(toolType)) {
	case "", "function":
		return true
	default:
		return false
	}
}

func isValidToolName(name string) bool {
	name = strings.TrimSpace(name)
	if name == "" || len(name) > 64 {
		return false
	}
	for _, r := range name {
		if r >= 'a' && r <= 'z' {
			continue
		}
		if r >= 'A' && r <= 'Z' {
			continue
		}
		if r >= '0' && r <= '9' {
			continue
		}
		if r == '_' || r == '-' {
			continue
		}
		return false
	}
	return true
}

func cleanIRToolCalls(calls []IRToolCall) []IRToolCall {
	out := make([]IRToolCall, 0, len(calls))
	for _, call := range calls {
		call.Name = strings.TrimSpace(call.Name)
		if !isValidToolName(call.Name) {
			continue
		}
		out = append(out, call)
	}
	return out
}

func appendIRToolCallIfValid(calls []IRToolCall, call IRToolCall) []IRToolCall {
	call.Name = strings.TrimSpace(call.Name)
	if !isValidToolName(call.Name) {
		return calls
	}
	return append(calls, call)
}

func toolNameSet(tools []IRTool) map[string]bool {
	names := map[string]bool{}
	for _, tool := range tools {
		if isValidToolName(tool.Name) {
			names[tool.Name] = true
		}
	}
	return names
}
