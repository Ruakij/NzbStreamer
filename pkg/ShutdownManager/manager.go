// Package shutdownmanager provides graceful shutdown coordination for services
// with timeout handling and cleanup actions.
package shutdownmanager

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"sync"
	"time"
)

var ErrShutdownTimeout = errors.New("shutdown timed out")

// ShutdownManager coordinates graceful shutdown of services with timeout handling.
// It uses a WaitGroup to track active services and provides timeout-based shutdown.
type ShutdownManager struct {
	wg            sync.WaitGroup
	cancel        context.CancelFunc
	timeout       time.Duration
	timeoutAction func()
}

// NewShutdownManager creates a new ShutdownManager with the specified timeout duration and timeout action.
// It returns the manager and a context that will be cancelled when shutdown begins.
func NewShutdownManager(timeout time.Duration, timeoutAction func()) (*ShutdownManager, context.Context) {
	ctx, cancel := context.WithCancel(context.Background())
	return &ShutdownManager{
		cancel:        cancel,
		timeout:       timeout,
		timeoutAction: timeoutAction,
	}, ctx
}

// AddService registers a new service with the shutdown manager.
// Must be called before the service starts.
func (sm *ShutdownManager) AddService() {
	sm.wg.Add(1)
}

// ServiceDone marks a service as completed.
// Must be called when a service finishes its shutdown.
func (sm *ShutdownManager) ServiceDone() {
	sm.wg.Done()
}

// Shutdown initiates a graceful shutdown of all registered services.
// It cancels the context and waits for all services to complete within the configured timeout period.
// If timeout occurs, it executes the timeout action.
func (sm *ShutdownManager) Shutdown() {
	sm.cancel() // Signal all services to stop

	// Wait for graceful shutdown with timeout
	done := make(chan struct{})
	go func() {
		sm.wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		slog.Info("Clean shutdown completed")
	case <-time.After(sm.timeout):
		if sm.timeoutAction != nil {
			sm.timeoutAction()
		}
	}
}

// Shutdown initiates a graceful shutdown of all registered services.
// It cancels the context and waits for all services to complete within the configured timeout period.
// If timeout occurs, it executes the timeout action.
// Then os.Exit is called with the code
func (sm *ShutdownManager) ShutdownAndExit(code int) {
	sm.Shutdown()
	os.Exit(code)
}
