package admin

import "testing"

func TestNormalizeOllamaCookiePreservesSessionPairs(t *testing.T) {
	got := normalizeOllamaCookie("Cookie: session=abc; theme=dark; Path=/; HttpOnly")
	want := "session=abc; theme=dark"
	if got != want {
		t.Fatalf("normalizeOllamaCookie() = %q, want %q", got, want)
	}
}

func TestParseOllamaQuotaPage(t *testing.T) {
	raw := []byte(`<!doctype html>
<html><body>
  <h2>Cloud plan</h2><div>Max</div>
  <section><h2>Session usage</h2><p>250 / 1000 requests (25%). Reset in 2 hours.</p></section>
  <section><h2>Weekly usage</h2><p>Models: gpt-oss:120b. 10 requests. 100 / 500. Reset at tomorrow 08:00.</p></section>
  <section><h2>Extra usage</h2><p>Balance: $12.50. Add credits or set a monthly limit.</p></section>
</body></html>`)

	result, err := parseOllamaQuotaPage(raw)
	if err != nil {
		t.Fatalf("parseOllamaQuotaPage() error = %v", err)
	}
	if result.Plan != "Max" {
		t.Fatalf("plan = %q, want Max", result.Plan)
	}
	if result.Session == nil || result.Session.Used != "250" || result.Session.Limit != "1000" {
		t.Fatalf("session = %#v, want used=250 limit=1000", result.Session)
	}
	if result.Session.UsagePercent == nil || *result.Session.UsagePercent != 25 {
		t.Fatalf("session usage percent = %#v, want 25", result.Session.UsagePercent)
	}
	if result.Session.Requests != "1000" {
		t.Fatalf("session requests = %q, want 1000", result.Session.Requests)
	}
	if result.Weekly == nil || result.Weekly.Model == "" || result.Weekly.ResetAt == "" {
		t.Fatalf("weekly = %#v, expected model and reset text", result.Weekly)
	}
	if result.ExtraUsage == nil || result.ExtraUsage.Balance != "$12.50" {
		t.Fatalf("extra usage = %#v, want balance $12.50", result.ExtraUsage)
	}
}

func TestParseOllamaQuotaPageRejectsLoginPage(t *testing.T) {
	_, err := parseOllamaQuotaPage([]byte(`<html><body><h1>Sign in to Ollama</h1></body></html>`))
	if err == nil {
		t.Fatal("parseOllamaQuotaPage() accepted a login page")
	}
}

func TestParseOllamaQuotaPageRejectsUnknownPage(t *testing.T) {
	_, err := parseOllamaQuotaPage([]byte(`<html><body><h1>Account</h1><p>Nothing here</p></body></html>`))
	if err == nil {
		t.Fatal("parseOllamaQuotaPage() accepted a page without usage sections")
	}
}
