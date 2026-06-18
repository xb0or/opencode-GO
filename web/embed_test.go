package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAdminHandlerDisablesStaticAssetCaching(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/admin/js/app.js?v=test", nil)
	rec := httptest.NewRecorder()

	AdminHandler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if got := rec.Header().Get("Content-Type"); !strings.Contains(got, "application/javascript") {
		t.Fatalf("Content-Type = %q, want javascript", got)
	}
	if got := rec.Header().Get("Cache-Control"); !strings.Contains(got, "no-store") {
		t.Fatalf("Cache-Control = %q, want no-store", got)
	}
	if got := rec.Header().Get("Surrogate-Control"); got != "no-store" {
		t.Fatalf("Surrogate-Control = %q, want no-store", got)
	}
}

func TestAdminHandlerDisablesFallbackCaching(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/admin/usage", nil)
	rec := httptest.NewRecorder()

	AdminHandler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if got := rec.Header().Get("Content-Type"); !strings.Contains(got, "text/html") {
		t.Fatalf("Content-Type = %q, want html", got)
	}
	if got := rec.Header().Get("Cache-Control"); !strings.Contains(got, "no-store") {
		t.Fatalf("Cache-Control = %q, want no-store", got)
	}
}
