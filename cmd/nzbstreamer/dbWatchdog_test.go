package main

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestAFailingDatabaseIsOnlyDeadOnceItStaysThatWay(t *testing.T) {
	answer := errors.New("gone")
	watchdog := watchDB(t.Context(), func(context.Context) (int, error) { return 0, answer })

	if !watchdog.alive() {
		t.Error("a database that just stopped answering is not a dead process yet")
	}

	watchdog.mu.Lock()
	watchdog.answered = time.Now().Add(-2 * dbDeadAfter)
	watchdog.mu.Unlock()

	if watchdog.alive() {
		t.Error("a database unanswered past dbDeadAfter must read as dead")
	}

	answer = nil
	watchdog.check(t.Context())
	if !watchdog.alive() {
		t.Error("an answer brings the process back")
	}
}
