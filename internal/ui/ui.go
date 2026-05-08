package ui

import (
	"embed"
	"net/http"
)

//go:embed index.html
var content embed.FS

func Handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		data, _ := content.ReadFile("index.html")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(data)
	}
}
