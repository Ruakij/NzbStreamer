package webdav

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/emersion/go-webdav"
)

var logger = slog.With("Module", "Webdav")

const (
	ReadTimeout       = 30 * time.Second
	WriteTimeout      = 30 * time.Second
	IdleTimeout       = 60 * time.Second
	ReadHeaderTimeout = 10 * time.Second
)

type BasicAuthConfig struct {
	Username string
	Password string
}

func basicAuth(handler http.Handler, config *BasicAuthConfig) http.Handler {
	if config == nil || config.Username == "" {
		return handler
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, pass, ok := r.BasicAuth()
		if !ok || user != config.Username || pass != config.Password {
			w.Header().Set("WWW-Authenticate", `Basic realm="Restricted"`)
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		handler.ServeHTTP(w, r)
	})
}

func Listen(ctx context.Context, listenAddress string, webdavHandler *webdav.Handler, authConfig *BasicAuthConfig) error {
	srv := &http.Server{
		Addr:              listenAddress,
		Handler:           basicAuth(webdavHandler, authConfig),
		ReadTimeout:       ReadTimeout,
		WriteTimeout:      WriteTimeout,
		IdleTimeout:       IdleTimeout,
		ReadHeaderTimeout: ReadHeaderTimeout,
	}

	go func() {
		<-ctx.Done()
		slog.Debug("Shutting down server")
		shutdownCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			logger.Error("Server shutdown error", "error", err)
		}
	}()

	logger.Info("Server starting", "Address", listenAddress)
	if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("failed listening: %w", err)
	}
	return nil
}
