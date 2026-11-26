package ui

import (
	"embed"
	"net/http"
)

//go:embed index.html dashboard.css dashboard.js
var content embed.FS

func Handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var filename, contentType string
		switch r.URL.Path {
		case "/":
			filename, contentType = "index.html", "text/html; charset=utf-8"
		case "/dashboard.css":
			filename, contentType = "dashboard.css", "text/css; charset=utf-8"
		case "/dashboard.js":
			filename, contentType = "dashboard.js", "text/javascript; charset=utf-8"
		default:
			http.NotFound(w, r)
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", contentType)
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Content-Security-Policy", "default-src 'none'; script-src 'self'; style-src 'self'; connect-src 'self'; form-action 'self'; frame-ancestors 'none'; base-uri 'none'")
		data, _ := content.ReadFile(filename)
		_, _ = w.Write(data)
	}
}
