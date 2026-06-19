package admin

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// GoQuotaResponse represents the quota usage data returned by opencode.ai.
type GoQuotaResponse struct {
	Mine         bool                `json:"mine"`
	UseBalance   bool                `json:"useBalance"`
	RollingUsage *GoQuotaBucket      `json:"rollingUsage,omitempty"`
	WeeklyUsage  *GoQuotaBucket      `json:"weeklyUsage,omitempty"`
	MonthlyUsage *GoQuotaBucket      `json:"monthlyUsage,omitempty"`
	Error        string              `json:"error,omitempty"`
	Raw          json.RawMessage     `json:"-"`
}

// GoQuotaBucket holds one quota bucket.
type GoQuotaBucket struct {
	Status       string `json:"status"`
	ResetInSec   int64  `json:"resetInSec"`
	UsagePercent int    `json:"usagePercent"`
}

// serverRefHash is the fixed server reference hash for lite.subscription.get.
const quotaServerHash = "c7389bd0e731f80f49593e5ee53835475f4e28594dd6bd83eb229bab753498cd"

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