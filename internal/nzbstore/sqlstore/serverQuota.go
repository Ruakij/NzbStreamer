package sqlstore

import (
	"database/sql"
	"errors"
	"fmt"
	"time"
)

type usage struct {
	used        int64
	periodStart time.Time
}

// ServerUsage reports what a server has spent in its current period. A server
// that has never been recorded reads as zero in a period starting now.
func (s *Store) ServerUsage(name string) (int64, time.Time, error) {
	var used, start int64
	err := s.db.QueryRow("SELECT used_bytes, period_start FROM server WHERE name = ?", name).Scan(&used, &start)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, time.Time{}, nil
	}
	if err != nil {
		return 0, time.Time{}, fmt.Errorf("failed reading usage of server %s: %w", name, err)
	}

	return used, time.Unix(start, 0), nil
}

// RecordServerUsage notes a servers running total. It is called once per fetched
// article, so it buffers the way segment sizes do; a crash loses the last few
// seconds of accounting, which only ever decides when to stop.
func (s *Store) RecordServerUsage(name string, used int64, periodStart time.Time) {
	s.pendingMutex.Lock()
	defer s.pendingMutex.Unlock()

	s.pendingUsage[name] = usage{used: used, periodStart: periodStart}
}

func (s *Store) flushServerUsage() {
	s.pendingMutex.Lock()
	pending := s.pendingUsage
	s.pendingUsage = make(map[string]usage, len(pending))
	s.pendingMutex.Unlock()

	for name, u := range pending {
		_, err := s.db.Exec(
			"INSERT INTO server (name, used_bytes, period_start) VALUES (?, ?, ?)"+
				" ON CONFLICT (name) DO UPDATE SET used_bytes = excluded.used_bytes, period_start = excluded.period_start",
			name, u.used, u.periodStart.Unix(),
		)
		if err != nil {
			logger.Error("Failed storing server usage", "server", name, "error", err)
		}
	}
}
