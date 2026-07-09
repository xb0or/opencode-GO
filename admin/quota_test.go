package admin

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/xb0or/opencode-GO/store"
)

func TestParseOpenCodeWorkspacesRejectsSerovalError(t *testing.T) {
	raw := []byte(`;0x00000260;((self.$R=self.$R||{})["server-fn:0"]=[],($R=>$R[0]=Object.assign(new Error("actor of type \"public\" is not associated with an account"),{stack:"Error"}))($R["server-fn:0"]))`)

	workspaces, err := parseOpenCodeWorkspaces(raw)
	if err == nil {
		t.Fatalf("expected error, got workspaces=%#v", workspaces)
	}
	if len(workspaces) != 0 {
		t.Fatalf("seroval error should not produce workspace candidates: %#v", workspaces)
	}
	if !strings.Contains(err.Error(), "cookie may be invalid or expired") {
		t.Fatalf("error should include cookie hint, got %q", err.Error())
	}
}

func TestParseOpenCodeWorkspacesFindsIDsInSerovalText(t *testing.T) {
	raw := []byte(`;0x00000042;(($R)=>$R[0]=[{"id":"ws_abc123","name":"Personal"}])($R["server-fn:0"])`)

	workspaces, err := parseOpenCodeWorkspaces(raw)
	if err != nil {
		t.Fatalf("parse workspaces: %v", err)
	}
	if len(workspaces) == 0 || workspaces[0].ID != "ws_abc123" {
		t.Fatalf("unexpected workspaces: %#v", workspaces)
	}
}

func TestNormalizeWorkspaceIDAcceptsWorkspaceURL(t *testing.T) {
	got := normalizeWorkspaceID("https://opencode.ai/workspace/wrk_01TESTWORKSPACEID00000000/go")
	if got != "wrk_01TESTWORKSPACEID00000000" {
		t.Fatalf("workspace ID = %q", got)
	}
}

func TestParseGoQuotaResponseSurfacesHTTPMessage(t *testing.T) {
	raw := []byte(`{"status":500,"unhandled":true,"message":"HTTPError"}`)

	result, err := parseGoQuotaResponse(raw, 500)
	if err == nil {
		t.Fatalf("expected error, got result=%#v", result)
	}
	if !strings.Contains(err.Error(), "quota returned HTTP 500: 500 HTTPError") {
		t.Fatalf("unexpected error: %q", err.Error())
	}
}

func TestParseGoQuotaFromWorkspacePage(t *testing.T) {
	raw := []byte(`$R[28]($R[18],$R[33]={mine:!0,useBalance:!1,rollingUsage:$R[34]={status:"ok",resetInSec:18000,usagePercent:0},weeklyUsage:$R[35]={status:"ok",resetInSec:490728,usagePercent:15},monthlyUsage:$R[36]={status:"ok",resetInSec:2591627,usagePercent:7}});`)

	result, err := parseGoQuotaFromWorkspacePage(raw)
	if err != nil {
		t.Fatalf("parse page quota: %v", err)
	}
	if !result.Mine || result.UseBalance {
		t.Fatalf("unexpected quota flags: %#v", result)
	}
	if result.WeeklyUsage == nil || result.WeeklyUsage.UsagePercent != 15 {
		t.Fatalf("unexpected weekly usage: %#v", result.WeeklyUsage)
	}
	if result.MonthlyUsage == nil || result.MonthlyUsage.UsagePercent != 7 {
		t.Fatalf("unexpected monthly usage: %#v", result.MonthlyUsage)
	}
}

func TestParseGoQuotaFromWorkspacePageIndependentBuckets(t *testing.T) {
	// Buckets separated by unrelated $R references and not in a single
	// consecutive mine/useBalance block — the format observed after
	// SolidStart upgrades.
	raw := []byte(`<html><script>$R[10]=new Date();` +
		`rollingUsage:$R[34]={status:"ok",resetInSec:3600,usagePercent:0};` +
		`someOther:$R[99]={foo:"bar"};` +
		`weeklyUsage:$R[35]={status:"ok",resetInSec:604800,usagePercent:35};` +
		`monthlyUsage:$R[36]={status:"ok",resetInSec:2592000,usagePercent:12};` +
		`</script></html>`)

	result, err := parseGoQuotaFromWorkspacePage(raw)
	if err != nil {
		t.Fatalf("parse page quota: %v", err)
	}
	if result.RollingUsage == nil || result.RollingUsage.UsagePercent != 0 {
		t.Fatalf("unexpected rolling usage: %#v", result.RollingUsage)
	}
	if result.WeeklyUsage == nil || result.WeeklyUsage.UsagePercent != 35 {
		t.Fatalf("unexpected weekly usage: %#v", result.WeeklyUsage)
	}
	if result.MonthlyUsage == nil || result.MonthlyUsage.UsagePercent != 12 {
		t.Fatalf("unexpected monthly usage: %#v", result.MonthlyUsage)
	}
}

func TestParseGoQuotaFromWorkspacePageNoRefPrefix(t *testing.T) {
	// Buckets without $R[N]= prefix (plain inline objects).
	raw := []byte(`rollingUsage:{status:"ok",resetInSec:7200,usagePercent:3},` +
		`weeklyUsage:{status:"ok",resetInSec:400000,usagePercent:20},` +
		`monthlyUsage:{status:"ok",resetInSec:2000000,usagePercent:8}`)

	result, err := parseGoQuotaFromWorkspacePage(raw)
	if err != nil {
		t.Fatalf("parse page quota: %v", err)
	}
	if result.RollingUsage == nil || result.RollingUsage.ResetInSec != 7200 {
		t.Fatalf("unexpected rolling usage: %#v", result.RollingUsage)
	}
	if result.WeeklyUsage == nil || result.WeeklyUsage.UsagePercent != 20 {
		t.Fatalf("unexpected weekly usage: %#v", result.WeeklyUsage)
	}
	if result.MonthlyUsage == nil || result.MonthlyUsage.UsagePercent != 8 {
		t.Fatalf("unexpected monthly usage: %#v", result.MonthlyUsage)
	}
}

func TestParseGoQuotaFromWorkspacePageLoginRedirect(t *testing.T) {
	// HTML page that contains sign-in markers but no quota data.
	raw := []byte(`<!DOCTYPE html><html><head><title>Sign In</title></head>` +
		`<body><a href="/sign-in">Login</a></body></html>`)

	_, err := parseGoQuotaFromWorkspacePage(raw)
	if err == nil {
		t.Fatal("expected error for login page without quota data")
	}
}

func TestIsCookieExpiredError(t *testing.T) {
	cases := []struct {
		msg string
		ok  bool
	}{
		{"cookie may be invalid or expired", true},
		{"authentication failed (HTTP 401)", true},
		{"session expired: redirected to /sign-in", true},
		{"workspace page is a login redirect", true},
		{"unexpected response", false},
		{"", false},
	}
	for _, tc := range cases {
		err := fmt.Errorf("%s", tc.msg)
		if got := isCookieExpiredError(err); got != tc.ok {
			t.Fatalf("isCookieExpiredError(%q) = %v, want %v", tc.msg, got, tc.ok)
		}
	}
}

func TestParseGoQuotaResponseSuccess(t *testing.T) {
	raw := []byte(`{
		"mine": true,
		"useBalance": false,
		"rollingUsage": {"status": "ok", "resetInSec": 15332, "usagePercent": 0},
		"weeklyUsage": {"status": "ok", "resetInSec": 201612, "usagePercent": 15},
		"monthlyUsage": {"status": "ok", "resetInSec": 2302511, "usagePercent": 7}
	}`)

	result, err := parseGoQuotaResponse(raw, 200)
	if err != nil {
		t.Fatalf("parse quota response: %v", err)
	}
	if result.WeeklyUsage == nil || result.WeeklyUsage.UsagePercent != 15 {
		t.Fatalf("unexpected quota result: %#v", result)
	}
}

func TestParseGoQuotaResponseMissingBucketsIncludesSummary(t *testing.T) {
	raw := []byte(`{"mine":true,"useBalance":false}`)

	result, err := parseGoQuotaResponse(raw, 200)
	if err == nil {
		t.Fatalf("expected error, got result=%#v", result)
	}
	if !strings.Contains(err.Error(), "quota response missing usage buckets") {
		t.Fatalf("unexpected error: %q", err.Error())
	}
}

func TestPersistKeyQuotaSnapshotDecoratesKey(t *testing.T) {
	if err := store.InitForTest("file:admin_quota_snapshot?mode=memory&cache=shared"); err != nil {
		t.Fatalf("init test db: %v", err)
	}
	key := &store.Key{Value: "sk-test", Group: "go", Enabled: true, Weight: 1}
	if err := store.DB().Create(key).Error; err != nil {
		t.Fatalf("create key: %v", err)
	}

	checkedAt := time.Date(2026, 6, 21, 9, 0, 0, 0, time.UTC)
	persistKeyQuotaSnapshot(key.ID, gin.H{
		"configured": true,
		"checkedAt":  checkedAt.Format(time.RFC3339),
		"quota": gin.H{
			"rolling": gin.H{"usagePercent": 2, "resetIn": "4 小时", "resetInSec": int64(14400)},
		},
	}, checkedAt)

	var saved store.Key
	if err := store.DB().First(&saved, key.ID).Error; err != nil {
		t.Fatalf("load key: %v", err)
	}
	if saved.QuotaSnapshot == "" || saved.QuotaUpdatedAt == nil {
		t.Fatalf("quota snapshot was not persisted: %#v", saved)
	}
	dto := decorateKey(saved)
	body, err := json.Marshal(dto)
	if err != nil {
		t.Fatalf("marshal dto: %v", err)
	}
	if !strings.Contains(string(body), `"last_quota"`) || !strings.Contains(string(body), `"usagePercent":2`) {
		t.Fatalf("decorated key missing quota snapshot: %s", body)
	}
	if strings.Contains(string(body), "quota_snapshot") {
		t.Fatalf("internal quota snapshot leaked in response: %s", body)
	}
}

func TestKeyUsagePayloadReturnsLocalUsage(t *testing.T) {
	if err := store.InitForTest("file:admin_key_usage?mode=memory&cache=shared"); err != nil {
		t.Fatalf("init test db: %v", err)
	}
	key := &store.Key{Value: "sk-usage", Group: "go", Enabled: true, Weight: 1}
	if err := store.DB().Create(key).Error; err != nil {
		t.Fatalf("create key: %v", err)
	}
	now := time.Now()
	rows := []store.UsageLog{
		{KeyID: key.ID, StatusCode: 200, InputTokens: 10, OutputTokens: 5, TotalTokens: 15, CreatedAt: now.Add(-30 * time.Minute)},
		{KeyID: key.ID, StatusCode: 200, InputTokens: 20, OutputTokens: 10, TotalTokens: 30, CreatedAt: now.Add(-3 * 24 * time.Hour)},
		{KeyID: key.ID, StatusCode: 200, InputTokens: 30, OutputTokens: 20, TotalTokens: 50, CreatedAt: now.Add(-20 * 24 * time.Hour)},
		{KeyID: key.ID, StatusCode: 200, InputTokens: 40, OutputTokens: 20, TotalTokens: 60, CreatedAt: now.Add(-40 * 24 * time.Hour)},
	}
	for _, row := range rows {
		if err := store.DB().Create(&row).Error; err != nil {
			t.Fatalf("create usage log: %v", err)
		}
	}

	payload := keyUsagePayload(key.ID)
	total := payload["total"].(quotaUsageStats)
	rolling := payload["rolling"].(quotaUsageStats)
	weekly := payload["weekly"].(quotaUsageStats)
	monthly := payload["monthly"].(quotaUsageStats)

	if total.Requests != 4 || total.TotalTokens != 155 {
		t.Fatalf("total usage=%#v, want 4 requests / 155 tokens", total)
	}
	if rolling.Requests != 1 || rolling.TotalTokens != 15 {
		t.Fatalf("rolling usage=%#v, want 1 request / 15 tokens", rolling)
	}
	if weekly.Requests != 2 || weekly.TotalTokens != 45 {
		t.Fatalf("weekly usage=%#v, want 2 requests / 45 tokens", weekly)
	}
	if monthly.Requests != 3 || monthly.TotalTokens != 95 {
		t.Fatalf("monthly usage=%#v, want 3 requests / 95 tokens", monthly)
	}
}
