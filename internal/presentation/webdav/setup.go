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

func Listen(ctx context.Context, listenAddress string, webdavHandler *webdav.Handler) error {
	srv := &http.Server{
		Addr:              listenAddress,
		Handler:           webdavHandler,
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
