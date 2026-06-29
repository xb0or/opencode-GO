package admin

import (
	"strings"
	"testing"

	"github.com/xb0or/opencode-GO/store"
)

func TestExtractOpenCodeKeyValues(t *testing.T) {
	raw := `;(($R=>$R[0]=[{key:"sk-live_test123456789",name:"go"},{value:"opencode_abcdEFGH1234"}])($R["server-fn:0"]))`
	got := extractOpenCodeKeyValues(raw)
	if len(got) != 2 || got[0] != "sk-live_test123456789" || got[1] != "opencode_abcdEFGH1234" {
		t.Fatalf("unexpected keys: %#v", got)
	}
}

func TestParseLoginFormCollectsHiddenFields(t *testing.T) {
	form, err := findLoginForm(`<form action="/session"><input name="authenticity_token" value="abc"><button name="commit" value="Sign in"></button></form>`, "/session")
	if err != nil {
		t.Fatalf("find form: %v", err)
	}
	if form.Values.Get("authenticity_token") != "abc" || form.Values.Get("commit") != "Sign in" {
		t.Fatalf("unexpected form values: %#v", form.Values)
	}
}

func TestUpsertImportedGoKeysCreatesAndUpdatesCookie(t *testing.T) {
	if err := store.InitForTest("file:github_import_keys?mode=memory&cache=shared"); err != nil {
		t.Fatalf("init test db: %v", err)
	}
	req := githubImportRequest{Label: "GitHub", Weight: 2}
	imported, created, updated, err := upsertImportedGoKeys([]string{"sk-imported123456"}, "auth=cookie-a", "wrk_TEST123456", req)
	if err != nil {
		t.Fatalf("first import: %v", err)
	}
	if created != 1 || updated != 0 || len(imported) != 1 || !imported[0].Created {
		t.Fatalf("unexpected first import result: imported=%#v created=%d updated=%d", imported, created, updated)
	}
	imported, created, updated, err = upsertImportedGoKeys([]string{"sk-imported123456"}, "auth=cookie-b", "wrk_TEST123456", req)
	if err != nil {
		t.Fatalf("second import: %v", err)
	}
	if created != 0 || updated != 1 || len(imported) != 1 || imported[0].Created {
		t.Fatalf("unexpected second import result: imported=%#v created=%d updated=%d", imported, created, updated)
	}
	if strings.Contains(imported[0].Value, "imported123456") {
		t.Fatalf("import response should mask key value: %#v", imported[0])
	}
	var saved store.Key
	if err := store.DB().Where("value = ?", "sk-imported123456").First(&saved).Error; err != nil {
		t.Fatalf("load key: %v", err)
	}
	if saved.Cookie != "auth=cookie-b" || saved.WorkspaceID != "wrk_TEST123456" || saved.Weight != 2 {
		t.Fatalf("key not updated: %#v", saved)
	}
}
