package admin

import (
	"strings"
	"testing"
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
	got := normalizeWorkspaceID("https://opencode.ai/workspace/wrk_01KQ1EE29WRRFXGACZ6XB9QVSS/go")
	if got != "wrk_01KQ1EE29WRRFXGACZ6XB9QVSS" {
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
