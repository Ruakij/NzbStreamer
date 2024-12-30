package main

import (
	"golang.org/x/net/webdav"
	"net/http"
)

func setupWebdav(filesystem webdav.FileSystem, listenAddress string) error {
	webdavHandler := &webdav.Handler{
		FileSystem: filesystem,
		//FileSystem: webdav.NewMemFS(),
		LockSystem: webdav.NewMemLS(),
	}

	err := http.ListenAndServe(listenAddress, webdavHandler)
	return err
}
