package web

import (
	_ "embed"
	"net/http"
)

//go:embed admin.html
var adminHTML []byte

// AdminHandler returns an http.Handler that serves the admin SPA.
func AdminHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		w.Write(adminHTML)
	})
}
