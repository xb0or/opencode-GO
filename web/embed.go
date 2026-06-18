package web

import (
	"embed"
	"net/http"
	"strings"
)

//go:embed admin.html css/*.css js/*.js js/pages/*.js
var content embed.FS

// AdminHandler returns an http.Handler that serves the admin SPA.
//
// When called from Gin's NoRoute, c.Request.URL.Path is the full path
// (e.g. /admin/css/admin.css). We strip the /admin prefix to find
// the correct embedded file.
func AdminHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Extract the sub-path after /admin
		path := strings.TrimPrefix(r.URL.Path, "/admin")

		// Determine which embedded file to serve
		var filePath string
		switch {
		case path == "" || path == "/":
			filePath = "admin.html"
		case strings.HasPrefix(path, "/"):
			filePath = path[1:] // strip leading /
		default:
			filePath = path
		}

		data, err := content.ReadFile(filePath)
		if err != nil {
			// SPA fallback: serve admin.html for any unknown sub-path
			data, _ = content.ReadFile("admin.html")
			setNoStoreHeaders(w)
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.WriteHeader(http.StatusOK)
			w.Write(data)
			return
		}

		setNoStoreHeaders(w)
		// Set correct Content-Type based on file extension
		if strings.HasSuffix(filePath, ".css") {
			w.Header().Set("Content-Type", "text/css; charset=utf-8")
		} else if strings.HasSuffix(filePath, ".js") {
			w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
		} else if strings.HasSuffix(filePath, ".html") {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
		} else {
			w.Header().Set("Content-Type", http.DetectContentType(data))
		}

		w.WriteHeader(http.StatusOK)
		w.Write(data)
	})
}

func setNoStoreHeaders(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate, proxy-revalidate")
	w.Header().Set("Pragma", "no-cache")
	w.Header().Set("Expires", "0")
	w.Header().Set("Surrogate-Control", "no-store")
}
