package webdav

import (
	"net/http"

	"github.com/emersion/go-webdav"
	"golang.org/x/exp/slog"
)

func Listen(listenAddress string, webdavHandler *webdav.Handler) error {
	slog.Info("Webdav listening on", "Address", listenAddress)
	err := http.ListenAndServe(listenAddress, webdavHandler)
	return err
}
