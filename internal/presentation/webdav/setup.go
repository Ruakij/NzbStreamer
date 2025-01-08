package webdav

import (
	"fmt"
	"net/http"

	"github.com/emersion/go-webdav"
)

func Listen(listenAddress string, webdavHandler *webdav.Handler) error {
	fmt.Printf("Webdav listening on %s\n", listenAddress)
	err := http.ListenAndServe(listenAddress, webdavHandler)
	return err
}
