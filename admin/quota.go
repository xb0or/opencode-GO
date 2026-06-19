package admin

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"
)

// GoQuotaResponse represents the quota usage data returned by opencode.ai.
type GoQuotaResponse struct {
	Mine         bool            `json:"mine"`
	UseBalance   bool            `json:"useBalance"`
	RollingUsage *GoQuotaBucket  `json:"rollingUsage,omitempty"`
	WeeklyUsage  *GoQuotaBucket  `json:"weeklyUsage,omitempty"`
	MonthlyUsage *GoQuotaBucket  `json:"monthlyUsage,omitempty"`
	Error        string          `json:"error,omitempty"`
	Raw          json.RawMessage `json:"-"`
}

// GoQuotaBucket holds one quota bucket.
type GoQuotaBucket struct {
	Status       string `json:"status"`
	ResetInSec   int64  `json:"resetInSec"`
	UsagePercent int    `json:"usagePercent"`
}

// serverRefHash values are fixed server reference hashes observed from opencode.ai.
const quotaServerHash = "c7389bd0e731f80f49593e5ee53835475f4e28594dd6bd83eb229bab753498cd"
const workspacesServerHash = "def39973159c7f0483d8793a822b8dbb10d067e12c65455fcb4608459ba0234f"

var authCookiePattern = regexp.MustCompile(`(?i)(?:^|[;\s])auth=([^;\s]+)`)
var workspaceIDPattern = regexp.MustCompile(`(?i)"(?:workspace[_-]?id|workspaceID|id)"\s*:\s*"([^";\s]+)"`)

// normalizeAuthCookie accepts pasted browser cookie/header fragments and returns
// the minimal Cookie header value required by opencode.ai quota RPC: auth=<token>.
func normalizeAuthCookie(raw string) string {
	s := strings.TrimSpace(raw)
	if s == "" {
		return ""
	}
	s = strings.TrimPrefix(s, "Cookie:")
	s = strings.TrimPrefix(s, "cookie:")
	s = strings.TrimPrefix(s, "Set-Cookie:")
	s = strings.TrimPrefix(s, "set-cookie:")
	s = strings.TrimSpace(s)
	if m := authCookiePattern.FindStringSubmatch(s); len(m) == 2 {
		return "auth=" + strings.TrimSpace(m[1])
	}
	if !strings.Contains(s, "=") {
		return "auth=" + s
	}
	return s
}

// OpenCodeWorkspace is the minimal workspace shape used for quota discovery.
type OpenCodeWorkspace struct {
	ID   string `json:"id"`
	Name string `json:"name,omitempty"`
}

func fetchOpenCodeWorkspaces(cookie string) ([]OpenCodeWorkspace, error) {
	cookie = normalizeAuthCookie(cookie)
	if cookie == "" {
		return nil, fmt.Errorf("cookie is empty")
	}

	var lastErr error
	for instance := 0; instance < 6; instance++ {
		req, err := http.NewRequest(http.MethodPost, "https://opencode.ai/_server", nil)
		if err != nil {
			return nil, fmt.Errorf("build workspaces request: %w", err)
		}
		req.Header.Set("X-Server-Id", workspacesServerHash)
		req.Header.Set("X-Server-Instance", fmt.Sprintf("server-fn:%d", instance))
		req.Header.Set("Cookie", cookie)

		client := &http.Client{Timeout: 15 * time.Second}
		resp, err := client.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		raw, readErr := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if readErr != nil {
			lastErr = readErr
			continue
		}
		if resp.StatusCode >= 400 {
			lastErr = fmt.Errorf("workspaces returned HTTP %d", resp.StatusCode)
			continue
		}
		workspaces, err := parseOpenCodeWorkspaces(raw)
		if err != nil {
			lastErr = err
			continue
		}
		if len(workspaces) > 0 {
			return workspaces, nil
		}
		lastErr = fmt.Errorf("workspace list is empty")
	}
	if lastErr != nil {
		return nil, lastErr
	}
	return nil, fmt.Errorf("workspace list is empty")
}

func parseOpenCodeWorkspaces(raw []byte) ([]OpenCodeWorkspace, error) {
	payload, err := decodeFirstJSONValue(raw)
	if err == nil {
		if workspaces := collectOpenCodeWorkspaces(payload); len(workspaces) > 0 {
			return workspaces, nil
		}
	}
	if workspaces := collectWorkspaceIDsFromText(string(raw)); len(workspaces) > 0 {
		return workspaces, nil
	}
	if err != nil {
		return nil, fmt.Errorf("decode workspaces response: %w", err)
	}
	return nil, fmt.Errorf("workspace list is empty")
}

func decodeFirstJSONValue(raw []byte) (any, error) {
	s := strings.TrimSpace(string(raw))
	if s == "" {
		return nil, fmt.Errorf("empty response")
	}
	start := -1
	for i, r := range s {
		if r == '{' || r == '[' {
			start = i
			break
		}
	}
	if start < 0 {
		return nil, fmt.Errorf("response does not contain JSON")
	}
	dec := json.NewDecoder(strings.NewReader(s[start:]))
	var payload any
	if err := dec.Decode(&payload); err != nil {
		return nil, err
	}
	return payload, nil
}

func collectWorkspaceIDsFromText(raw string) []OpenCodeWorkspace {
	seen := map[string]bool{}
	var out []OpenCodeWorkspace
	for _, match := range workspaceIDPattern.FindAllStringSubmatch(raw, -1) {
		if len(match) != 2 {
			continue
		}
		id := strings.TrimSpace(match[1])
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, OpenCodeWorkspace{ID: id})
	}
	return out
}

func collectOpenCodeWorkspaces(v any) []OpenCodeWorkspace {
	seen := map[string]bool{}
	var out []OpenCodeWorkspace
	var walk func(any)
	walk = func(x any) {
		switch value := x.(type) {
		case []any:
			for _, item := range value {
				walk(item)
			}
		case map[string]any:
			id, _ := value["id"].(string)
			id = strings.TrimSpace(id)
			if id != "" && !seen[id] {
				name := firstStringField(value, "name", "title", "slug")
				out = append(out, OpenCodeWorkspace{ID: id, Name: name})
				seen[id] = true
			}
			for _, item := range value {
				walk(item)
			}
		}
	}
	walk(v)
	return out
}

func firstStringField(m map[string]any, keys ...string) string {
	for _, key := range keys {
		if s, ok := m[key].(string); ok && strings.TrimSpace(s) != "" {
			return strings.TrimSpace(s)
		}
	}
	return ""
}

// serovalString encodes a single string argument as the Seroval format expected
// by opencode.ai RPC calls.
func serovalString(s string) json.RawMessage {
	// {"t":{"t":9,"i":0,"l":1,"f":[{"t":1,"s":"<value>"}],"o":0},"f":31,"m":[]}
	return json.RawMessage(fmt.Sprintf(
		`{"t":{"t":9,"i":0,"l":1,"f":[{"t":1,"s":%q}],"o":0},"f":31,"m":[]}`,
		s,
	))
}

// fetchGoQuota calls the opencode.ai Go subscription RPC endpoint using the
// provided session cookie and workspace ID. Returns nil when the key has no
// cookie configured (silent skip).
func fetchGoQuota(cookie, workspaceID string) (*GoQuotaResponse, error) {
	cookie = normalizeAuthCookie(cookie)
	if cookie == "" || workspaceID == "" {
		return nil, nil
	}

	body := serovalString(workspaceID)
	req, err := http.NewRequest(http.MethodPost, "https://opencode.ai/_server", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Server-Id", quotaServerHash)
	req.Header.Set("X-Server-Instance", "server-fn:0")
	req.Header.Set("Cookie", cookie)

	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("http call: %w", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}

	var result GoQuotaResponse
	if err := json.Unmarshal(raw, &result); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	result.Raw = raw

	if errMsg := result.Error; errMsg != "" {
		return &result, nil
	}
	// Check if we got a valid response (not an auth error / HTML page).
	if result.RollingUsage == nil && result.WeeklyUsage == nil && result.MonthlyUsage == nil {
		return nil, fmt.Errorf("unexpected response (cookie may be invalid or expired)")
	}
	return &result, nil
}

// formatGoQuotaReset returns a human-readable string for the reset duration.
func formatGoQuotaReset(sec int64) string {
	if sec <= 0 {
		return "—"
	}
	d := time.Duration(sec) * time.Second
	days := int(d.Hours()) / 24
	hours := int(d.Hours()) % 24
	mins := int(d.Minutes()) % 60
	if days > 0 {
		return fmt.Sprintf("%dd %dh", days, hours)
	}
	if hours > 0 {
		return fmt.Sprintf("%dh %dm", hours, mins)
	}
	return fmt.Sprintf("%dm", mins)
}
