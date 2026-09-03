package sabnzbd

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"
)

const (
	readTimeout       = 30 * time.Second
	writeTimeout      = 30 * time.Second
	idleTimeout       = 60 * time.Second
	readHeaderTimeout = 10 * time.Second
)

// Listen serves the api until the context is cancelled. Everything is answered
// under `/api`, wherever a client's url base points, since that is the only path
// SABnzbd has.
func Listen(ctx context.Context, address string, handler *Handler) error {
	mux := http.NewServeMux()
	mux.Handle("/api", handler)
	mux.Handle("/{base}/api", handler)

	srv := &http.Server{
		Addr:              address,
		Handler:           mux,
		ReadTimeout:       readTimeout,
		WriteTimeout:      writeTimeout,
		IdleTimeout:       idleTimeout,
		ReadHeaderTimeout: readHeaderTimeout,
	}

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			logger.Error("Server shutdown error", "error", err)
		}
	}()

	logger.Info("Server starting", "Address", address)
	if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("failed listening: %w", err)
	}
	return nil
}
