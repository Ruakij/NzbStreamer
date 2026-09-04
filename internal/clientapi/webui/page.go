package webui

import (
	_ "embed"
	"log/slog"
	"net/http"
)

//go:embed index.html
var indexHTML []byte

func page(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if _, err := w.Write(indexHTML); err != nil {
		slog.Error("Failed writing page", "error", err)
	}
}
