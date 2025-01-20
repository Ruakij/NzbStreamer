package webdav

import (
	"fmt"
	"log/slog"
	"net/http"

	"github.com/emersion/go-webdav"

var logger = slog.With("Module", "Webdav")
)

func Listen(listenAddress string, webdavHandler *webdav.Handler) error {
	slog.Info("Webdav listening on", "Address", listenAddress)
	err := http.ListenAndServe(listenAddress, webdavHandler)
	return fmt.Errorf("failed listening: %w", err)
}
