package main

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

// How often the database is asked whether it is still there, how long it is
// given to answer, and how long it may go on not answering before the process
// calls itself dead. The last one is what a liveness probe restarts on, so it is
// several checks wide: a single slow answer under load is not a dead process.
//
// ponytail: three constants, make them settings if an installation needs to tune
// its probes against them.
const (
	dbCheckInterval = 10 * time.Second
	dbCheckTimeout  = 5 * time.Second
	dbDeadAfter     = time.Minute
)

// dbWatchdog answers what the metadata database is doing without ever waiting on
// it. The check runs on a timer with a deadline and the handlers read the last
// answer, because a database that has stopped answering would otherwise block
// the endpoint that exists to report it - and a health check that hangs reads to
// a probe as a timeout, which is the one thing it must not be confused with.
type dbWatchdog struct {
	ping func(context.Context) (int, error)

	mu       sync.Mutex
	nzbs     int
	err      error
	answered time.Time
}

// watchDB starts the timer and returns once the first check has been made, so
// nothing reads an answer that was never asked for. It stops with ctx.
func watchDB(ctx context.Context, ping func(context.Context) (int, error)) *dbWatchdog {
	// Startup counts as the last good answer, so the window a failure has to
	// last is measured from a real point in time rather than from the epoch
	watchdog := &dbWatchdog{ping: ping, answered: time.Now()}
	watchdog.check(ctx)

	go func() {
		ticker := time.NewTicker(dbCheckInterval)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				watchdog.check(ctx)
			}
		}
	}()

	return watchdog
}

func (w *dbWatchdog) check(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, dbCheckTimeout)
	defer cancel()

	nzbs, err := w.ping(ctx)

	w.mu.Lock()
	defer w.mu.Unlock()

	w.nzbs, w.err = nzbs, err
	if err == nil {
		w.answered = time.Now()
		return
	}

	slog.Warn("The metadata database did not answer", "error", err,
		"unanswered for", time.Since(w.answered).Round(time.Second))
}

// state is the last answer and how long it has been since there was a good one.
func (w *dbWatchdog) state() (nzbs int, since time.Duration, err error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	return w.nzbs, time.Since(w.answered), w.err
}

// alive is what a liveness probe reads: a database that has not answered for
// dbDeadAfter is not a temporary fault this process recovers from on its own -
// nothing it presents can be read and nothing new can be added - so being
// restarted is the only thing left that helps.
func (w *dbWatchdog) alive() bool {
	_, since, err := w.state()
	return err == nil || since < dbDeadAfter
}
