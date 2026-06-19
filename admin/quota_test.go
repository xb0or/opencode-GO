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
