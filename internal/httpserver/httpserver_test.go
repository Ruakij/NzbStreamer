package httpserver

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func pathRecorder(got *string) http.Handler {
	return http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		*got = r.URL.Path
	})
}

func TestMuxRoutes(t *testing.T) {
	var webdavPath, sabnzbdPath string
	mux := NewMux(Routes{
		Webdav:  pathRecorder(&webdavPath),
		Sabnzbd: pathRecorder(&sabnzbdPath),
	})

	// webdav sees its path intact, since go-webdav builds the href of every
	// response out of it
	mux.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/webdav/x", nil))
	if webdavPath != "/webdav/x" {
		t.Errorf("webdav handler got path %q, want /webdav/x", webdavPath)
	}

	mux.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/sabnzbd/api?mode=version", nil))
	if sabnzbdPath != "/sabnzbd/api" {
		t.Errorf("sabnzbd handler got path %q, want /sabnzbd/api", sabnzbdPath)
	}
}
