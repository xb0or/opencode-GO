package admin

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
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
const maxServerFnInstance = 80

var authCookiePattern = regexp.MustCompile(`(?i)(?:^|[;\s])auth=([^;\s]+)`)
var workspaceInputPattern = regexp.MustCompile(`(?i)\b(wrk_[A-Za-z0-9][A-Za-z0-9_-]{5,127})\b`)
var workspaceIDPattern = regexp.MustCompile(`(?i)"(?:workspace[_-]?id|workspaceID|id)"\s*:\s*"([^";\s]+)"`)
var quotedStringPattern = regexp.MustCompile(`"([^"\\]*(?:\\.[^"\\]*)*)"`)
var serovalErrorPattern = regexp.MustCompile(`new Error\("((?:\\.|[^"\\])*)"\)`)
var workspaceCandidatePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{5,127}$`)
var hydratedQuotaPattern = regexp.MustCompile(`mine:!(0|1),useBalance:!(0|1),rollingUsage:(?:\$R\[\d+\]=)?\{status:"([^"]+)",resetInSec:(\d+),usagePercent:(\d+)\},weeklyUsage:(?:\$R\[\d+\]=)?\{status:"([^"]+)",resetInSec:(\d+),usagePercent:(\d+)\},monthlyUsage:(?:\$R\[\d+\]=)?\{status:"([^"]+)",resetInSec:(\d+),usagePercent:(\d+)\}`)

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

func normalizeWorkspaceID(raw string) string {
	s := strings.TrimSpace(strings.Trim(raw, `"'`))
	if s == "" {
		return ""
	}
	if m := workspaceInputPattern.FindStringSubmatch(s); len(m) == 2 {
		return m[1]
	}
	return s
}

// OpenCodeWorkspace is the minimal workspace shape used for quota discovery.
type OpenCodeWorkspace struct {
	ID   string `json:"id"`
	Name string `json:"name,omitempty"`
}

type workspaceAutoDetectError struct {
	Candidates []OpenCodeWorkspace
	Cause      error
}

func (e *workspaceAutoDetectError) Error() string {
	if e == nil {
		return ""
	}
	if len(e.Candidates) > 0 {
		return fmt.Sprintf("found %d workspace candidates but %v", len(e.Candidates), e.Cause)
	}
	if e.Cause != nil {
		return e.Cause.Error()
	}
	return "workspace auto-detect failed"
}

func fetchOpenCodeWorkspaces(cookie string) ([]OpenCodeWorkspace, error) {
	cookie = normalizeAuthCookie(cookie)
	if cookie == "" {
		return nil, fmt.Errorf("cookie is empty")
	}

	var lastErr error
	seen := map[string]OpenCodeWorkspace{}
	for instance := 0; instance <= maxServerFnInstance; instance++ {
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
		for _, ws := range workspaces {
			addWorkspaceCandidate(seen, ws.ID, ws.Name)
		}
		// Found workspaces from this instance — no need to try
		// server-fn:1..80. opencode.ai populates all active workspaces
		// in a single server-fn response.
		break
	}
	out := workspaceCandidateList(seen)
	if len(out) > 0 {
		return out, nil
	}
	if lastErr != nil {
		return nil, lastErr
	}
	return nil, fmt.Errorf("workspace list is empty")
}

func resolveWorkspaceForQuota(cookie string) (string, *GoQuotaResponse, error) {
	workspaces, err := fetchOpenCodeWorkspaces(cookie)
	if err != nil {
		return "", nil, &workspaceAutoDetectError{Cause: err}
	}
	var lastErr error
	for _, ws := range workspaces {
		result, err := fetchGoQuota(cookie, ws.ID)
		if err == nil && result != nil {
			return ws.ID, result, nil
		}
		lastErr = err
	}
	if lastErr != nil {
		return "", nil, &workspaceAutoDetectError{
			Candidates: workspaces,
			Cause:      fmt.Errorf("quota validation failed: %w", lastErr),
		}
	}
	return "", nil, fmt.Errorf("workspace list is empty")
}

func parseOpenCodeWorkspaces(raw []byte) ([]OpenCodeWorkspace, error) {
	if msg := serovalErrorMessage(raw); msg != "" {
		return nil, fmt.Errorf("workspaces returned upstream error: %s (cookie may be invalid or expired)", msg)
	}
	if looksLikeHTML(raw) {
		return nil, fmt.Errorf("workspaces returned HTML login page (cookie may be invalid or expired)")
	}

	seen := map[string]OpenCodeWorkspace{}
	payload, err := decodeFirstJSONValue(raw)
	if err == nil {
		for _, ws := range collectOpenCodeWorkspaces(payload) {
			addWorkspaceCandidate(seen, ws.ID, ws.Name)
		}
		for _, id := range collectStringCandidatesFromJSON(payload) {
			addWorkspaceCandidate(seen, id, "")
		}
	}
	for _, ws := range collectWorkspaceIDsFromText(string(raw)) {
		addWorkspaceCandidate(seen, ws.ID, ws.Name)
	}
	out := workspaceCandidateList(seen)
	if len(out) > 0 {
		return out, nil
	}
	if err != nil {
		return nil, fmt.Errorf("decode workspaces response: %w", err)
	}
	return nil, fmt.Errorf("workspace list is empty")
}

func serovalErrorMessage(raw []byte) string {
	match := serovalErrorPattern.FindSubmatch(raw)
	if len(match) != 2 {
		return ""
	}
	quoted := `"` + string(match[1]) + `"`
	msg, err := strconv.Unquote(quoted)
	if err != nil {
		msg = string(match[1])
	}
	return strings.TrimSpace(msg)
}

func looksLikeHTML(raw []byte) bool {
	s := strings.TrimSpace(strings.ToLower(string(raw)))
	return strings.HasPrefix(s, "<!doctype html") || strings.HasPrefix(s, "<html") || strings.Contains(s, "<body")
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
	seen := map[string]OpenCodeWorkspace{}
	for _, match := range workspaceIDPattern.FindAllStringSubmatch(raw, -1) {
		if len(match) == 2 {
			addWorkspaceCandidate(seen, match[1], "")
		}
	}
	for _, match := range quotedStringPattern.FindAllStringSubmatch(raw, -1) {
		if len(match) == 2 {
			candidate := strings.ReplaceAll(match[1], `\"`, `"`)
			addWorkspaceCandidate(seen, candidate, "")
		}
	}
	return workspaceCandidateList(seen)
}

func collectOpenCodeWorkspaces(v any) []OpenCodeWorkspace {
	seen := map[string]OpenCodeWorkspace{}
	var walk func(any)
	walk = func(x any) {
		switch value := x.(type) {
		case []any:
			for _, item := range value {
				walk(item)
			}
		case map[string]any:
			id, _ := value["id"].(string)
			name := firstStringField(value, "name", "title", "slug")
			addWorkspaceCandidate(seen, id, name)
			for _, item := range value {
				walk(item)
			}
		}
	}
	walk(v)
	return workspaceCandidateList(seen)
}

func collectStringCandidatesFromJSON(v any) []string {
	seen := map[string]bool{}
	var out []string
	var walk func(any)
	walk = func(x any) {
		switch value := x.(type) {
		case string:
			if !seen[value] && looksLikeWorkspaceCandidate(value) {
				seen[value] = true
				out = append(out, value)
			}
		case []any:
			for _, item := range value {
				walk(item)
			}
		case map[string]any:
			for _, item := range value {
				walk(item)
			}
		}
	}
	walk(v)
	return out
}

func addWorkspaceCandidate(seen map[string]OpenCodeWorkspace, id, name string) {
	id = strings.TrimSpace(id)
	if !looksLikeWorkspaceCandidate(id) {
		return
	}
	if existing, ok := seen[id]; ok {
		if existing.Name == "" && strings.TrimSpace(name) != "" {
			existing.Name = strings.TrimSpace(name)
			seen[id] = existing
		}
		return
	}
	seen[id] = OpenCodeWorkspace{ID: id, Name: strings.TrimSpace(name)}
}

func workspaceCandidateList(seen map[string]OpenCodeWorkspace) []OpenCodeWorkspace {
	out := make([]OpenCodeWorkspace, 0, len(seen))
	for _, ws := range seen {
		out = append(out, ws)
	}
	sort.SliceStable(out, func(i, j int) bool {
		return workspaceCandidateScore(out[i].ID) > workspaceCandidateScore(out[j].ID)
	})
	if len(out) > 50 {
		out = out[:50]
	}
	return out
}

func workspaceCandidateScore(id string) int {
	lower := strings.ToLower(id)
	score := 0
	if strings.Contains(lower, "workspace") || strings.HasPrefix(lower, "wsp_") || strings.HasPrefix(lower, "ws_") {
		score += 100
	}
	if strings.Contains(lower, "user") || strings.Contains(lower, "session") || strings.Contains(lower, "token") {
		score -= 100
	}
	if len(id) >= 16 {
		score += 10
	}
	return score
}

func looksLikeWorkspaceCandidate(id string) bool {
	if !workspaceCandidatePattern.MatchString(id) {
		return false
	}
	lower := strings.ToLower(id)
	blocked := map[string]bool{
		"rollingusage": true, "weeklyusage": true, "monthlyusage": true,
		"usebalance": true, "configured": true, "workspace": true,
		"workspaceid": true, "server-fn": true, "public": true,
		"account": true, "openauth": true,
	}
	if blocked[lower] {
		return false
	}
	if strings.HasPrefix(lower, "http") || strings.Contains(lower, ".") {
		return false
	}
	return true
}

func firstStringField(m map[string]any, keys ...string) string {
	for _, key := range keys {
		if s, ok := m[key].(string); ok && strings.TrimSpace(s) != "" {
			return strings.TrimSpace(s)
		}
	}
	return ""
}

// fetchGoQuota calls the opencode.ai Go subscription RPC endpoint using the
// provided session cookie and workspace ID. Returns nil when the key has no
// cookie configured (silent skip).
func fetchGoQuota(cookie, workspaceID string) (*GoQuotaResponse, error) {
	cookie = normalizeAuthCookie(cookie)
	workspaceID = normalizeWorkspaceID(workspaceID)
	if cookie == "" || workspaceID == "" {
		return nil, nil
	}

	var lastErr error
	for instance := 0; instance <= maxServerFnInstance; instance++ {
		result, err := fetchGoQuotaWithInstance(cookie, workspaceID, instance)
		if err != nil {
			lastErr = err
			continue
		}
		return result, nil
	}
	if pageResult, pageErr := fetchGoQuotaFromWorkspacePage(cookie, workspaceID); pageErr == nil && pageResult != nil {
		return pageResult, nil
	} else if lastErr != nil {
		return nil, fmt.Errorf("%w; workspace page fallback failed: %v", lastErr, pageErr)
	}
	if lastErr != nil {
		return nil, lastErr
	}
	return nil, fmt.Errorf("unexpected response (cookie may be invalid or expired)")
}

func fetchGoQuotaWithInstance(cookie, workspaceID string, instance int) (*GoQuotaResponse, error) {
	// SolidStart update: args must be in URL query as JSON array, not in POST body.
	// The seroval body format is no longer accepted by the server.
	args := url.QueryEscape(`["` + workspaceID + `"]`)
	serverURL := fmt.Sprintf("https://opencode.ai/_server?id=%s&args=%s", quotaServerHash, args)
	req, err := http.NewRequest(http.MethodPost, serverURL, nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("X-Server-Id", quotaServerHash)
	req.Header.Set("X-Server-Instance", fmt.Sprintf("server-fn:%d", instance))
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

	return parseGoQuotaResponse(raw, resp.StatusCode)
}

func parseGoQuotaResponse(raw []byte, statusCode int) (*GoQuotaResponse, error) {
	if msg := serovalErrorMessage(raw); msg != "" {
		return nil, fmt.Errorf("quota returned upstream error: %s", msg)
	}
	if looksLikeHTML(raw) {
		return nil, fmt.Errorf("quota returned HTML login page (cookie may be invalid or expired)")
	}
	var result GoQuotaResponse
	payload, err := decodeFirstJSONValue(raw)
	if err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	normalized, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("normalize response: %w", err)
	}
	if err := json.Unmarshal(normalized, &result); err != nil {
		return nil, fmt.Errorf("decode quota payload: %w", err)
	}
	result.Raw = normalized

	if errMsg := result.Error; errMsg != "" {
		return &result, nil
	}
	if statusCode >= 400 {
		return nil, fmt.Errorf("quota returned HTTP %d: %s", statusCode, responseSummary(payload, raw))
	}
	if result.RollingUsage == nil || result.WeeklyUsage == nil || result.MonthlyUsage == nil {
		return nil, fmt.Errorf("quota response missing usage buckets: %s", responseSummary(payload, raw))
	}
	return &result, nil
}

func fetchGoQuotaFromWorkspacePage(cookie, workspaceID string) (*GoQuotaResponse, error) {
	cookie = normalizeAuthCookie(cookie)
	workspaceID = normalizeWorkspaceID(workspaceID)
	if cookie == "" || workspaceID == "" {
		return nil, fmt.Errorf("cookie or workspace ID is empty")
	}

	pageURL := fmt.Sprintf("https://opencode.ai/workspace/%s/go", url.PathEscape(workspaceID))
	req, err := http.NewRequest(http.MethodGet, pageURL, nil)
	if err != nil {
		return nil, fmt.Errorf("build workspace page request: %w", err)
	}
	req.Header.Set("Accept", "text/html,application/xhtml+xml")
	req.Header.Set("Cookie", cookie)

	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("workspace page http call: %w", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read workspace page: %w", err)
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("workspace page returned HTTP %d", resp.StatusCode)
	}
	return parseGoQuotaFromWorkspacePage(raw)
}

func parseGoQuotaFromWorkspacePage(raw []byte) (*GoQuotaResponse, error) {
	match := hydratedQuotaPattern.FindSubmatch(raw)
	if len(match) != 12 {
		if looksLikeHTML(raw) {
			return nil, fmt.Errorf("workspace page does not include Go quota data (cookie may be invalid, expired, or not subscribed)")
		}
		return nil, fmt.Errorf("workspace page response does not include Go quota data")
	}

	bucket := func(status, reset, percent []byte) (*GoQuotaBucket, error) {
		resetInSec, err := strconv.ParseInt(string(reset), 10, 64)
		if err != nil {
			return nil, fmt.Errorf("parse resetInSec: %w", err)
		}
		usagePercent, err := strconv.Atoi(string(percent))
		if err != nil {
			return nil, fmt.Errorf("parse usagePercent: %w", err)
		}
		return &GoQuotaBucket{
			Status:       string(status),
			ResetInSec:   resetInSec,
			UsagePercent: usagePercent,
		}, nil
	}

	rolling, err := bucket(match[3], match[4], match[5])
	if err != nil {
		return nil, err
	}
	weekly, err := bucket(match[6], match[7], match[8])
	if err != nil {
		return nil, err
	}
	monthly, err := bucket(match[9], match[10], match[11])
	if err != nil {
		return nil, err
	}

	return &GoQuotaResponse{
		Mine:         string(match[1]) == "0",
		UseBalance:   string(match[2]) == "0",
		RollingUsage: rolling,
		WeeklyUsage:  weekly,
		MonthlyUsage: monthly,
		Raw:          json.RawMessage("{}"),
	}, nil
}

func responseSummary(payload any, raw []byte) string {
	if payload != nil {
		if msg := firstStringInPayload(payload, "error", "message", "statusText", "code"); msg != "" {
			if status := firstNumberInPayload(payload, "status", "statusCode"); status != "" {
				return status + " " + msg
			}
			return msg
		}
		if compact, err := json.Marshal(payload); err == nil {
			return truncateString(string(compact), 240)
		}
	}
	return truncateString(strings.TrimSpace(string(raw)), 240)
}

func firstStringInPayload(payload any, keys ...string) string {
	switch v := payload.(type) {
	case map[string]any:
		for _, key := range keys {
			if s, ok := v[key].(string); ok && strings.TrimSpace(s) != "" {
				return strings.TrimSpace(s)
			}
		}
		for _, item := range v {
			if s := firstStringInPayload(item, keys...); s != "" {
				return s
			}
		}
	case []any:
		for _, item := range v {
			if s := firstStringInPayload(item, keys...); s != "" {
				return s
			}
		}
	}
	return ""
}

func firstNumberInPayload(payload any, keys ...string) string {
	m, ok := payload.(map[string]any)
	if !ok {
		return ""
	}
	for _, key := range keys {
		switch v := m[key].(type) {
		case float64:
			return strconv.FormatInt(int64(v), 10)
		case string:
			if strings.TrimSpace(v) != "" {
				return strings.TrimSpace(v)
			}
		}
	}
	return ""
}

func truncateString(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	if max <= 0 || len(s) <= max {
		return s
	}
	return s[:max] + "..."
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
