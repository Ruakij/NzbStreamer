// Package httpserver is the one address the process listens on: the web ui, its
// own api, the SABnzbd imitation, webdav, metrics and the debug handlers are
// paths on a single mux.
package httpserver

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/pprof"
	"time"

	"github.com/arl/statsviz"
)

// WebdavPrefix is the subtree webdav is served under, which its filesystem has
// to strip off a lookup.
const WebdavPrefix = "/webdav"

const (
	readHeaderTimeout = 10 * time.Second
	idleTimeout       = 60 * time.Second
	shutdownTimeout   = 3 * time.Second
)

// Routes are the handlers to serve; a nil one is not registered.
type Routes struct {
	// WebUI serves the page and this projects own api under the server root
	WebUI   http.Handler
	Sabnzbd http.Handler
	Webdav  http.Handler
	Metrics http.Handler
	Debug   bool
}

func NewMux(routes Routes) *http.ServeMux {
	mux := http.NewServeMux()

	if routes.WebUI != nil {
		mux.Handle("/", routes.WebUI)
	}
	if routes.Sabnzbd != nil {
		mux.Handle("/sabnzbd/api", routes.Sabnzbd)
	}
	if routes.Webdav != nil {
		mux.Handle(WebdavPrefix+"/", routes.Webdav)
	}
	if routes.Metrics != nil {
		mux.Handle("/metrics", routes.Metrics)
	}
	if routes.Debug {
		registerDebug(mux)
	}

	return mux
}

func registerDebug(mux *http.ServeMux) {
	mux.HandleFunc("/debug/pprof/", pprof.Index)
	mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("/debug/pprof/trace", pprof.Trace)

	if err := statsviz.Register(mux, statsviz.Root("/debug/statsviz")); err != nil {
		slog.Error("Failed registering statsviz", "error", err)
	}
}

// Listen serves until the context is cancelled. Read and write have no timeout:
// a webdav client streams a file for as long as it takes, and a PUT-less
// read-only server has nothing slow to read from a request.
func Listen(ctx context.Context, address string, handler http.Handler) error {
	srv := &http.Server{
		Addr:              address,
		Handler:           handler,
		IdleTimeout:       idleTimeout,
		ReadHeaderTimeout: readHeaderTimeout,
	}

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownTimeout)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			slog.Error("Server shutdown error", "error", err)
		}
	}()

	slog.Info("Server starting", "Address", address)
	if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("failed listening: %w", err)
	}
	return nil
}
