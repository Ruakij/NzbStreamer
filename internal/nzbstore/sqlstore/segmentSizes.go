package sqlstore

import (
	"fmt"
	"strings"
	"time"
)

// flushInterval bounds how long a learned size stays only in memory. Losing the
// buffer costs a re-measurement, never correctness, so there is nothing to gain
// from flushing sooner.
const flushInterval = 5 * time.Second

// A decoded segment length is a fact about an immutable post, so it is only ever
// learned, never invalidated, and two nzbs describing the same post share it.
// That is why the key is the message-id and not a position in an nzb.

// SegmentSizes returns the known decoded lengths among ids. Absent ids are
// absent from the map; not knowing one is the normal state, not an error.
func (s *Store) SegmentSizes(ids []string) (map[string]int64, error) {
	sizes := make(map[string]int64, len(ids))

	// Chunked to stay well under the bound on placeholders per statement, which a
	// release-sized nzb would otherwise reach
	const chunk = 500
	for start := 0; start < len(ids); start += chunk {
		end := min(start+chunk, len(ids))
		batch := ids[start:end]

		args := make([]any, len(batch))
		for i, id := range batch {
			args[i] = id
		}
		query := "SELECT message_id, size FROM segment WHERE message_id IN (?" +
			strings.Repeat(",?", len(batch)-1) + ")"

		rows, err := s.db.Query(query, args...)
		if err != nil {
			return nil, fmt.Errorf("failed reading segment sizes: %w", err)
		}
		for rows.Next() {
			var id string
			var size int64
			if err := rows.Scan(&id, &size); err != nil {
				rows.Close()
				return nil, fmt.Errorf("failed reading segment size row: %w", err)
			}
			sizes[id] = size
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, fmt.Errorf("failed reading segment sizes: %w", err)
		}
		rows.Close()
	}

	return sizes, nil
}

// RecordSegmentSize notes the decoded length of a segment. It is called from the
// read path once per fetched article, so it buffers rather than writing, and a
// read never waits on the database.
func (s *Store) RecordSegmentSize(messageID string, size int64) {
	s.pendingMutex.Lock()
	defer s.pendingMutex.Unlock()

	s.pending[messageID] = size
}

func (s *Store) flushLoop() {
	defer s.flusherDone.Done()

	ticker := time.NewTicker(flushInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			s.flushSegmentSizes()
		case <-s.closing:
			s.flushSegmentSizes()
			return
		}
	}
}

func (s *Store) flushSegmentSizes() {
	s.pendingMutex.Lock()
	pending := s.pending
	s.pending = make(map[string]int64, len(pending))
	s.pendingMutex.Unlock()

	if len(pending) == 0 {
		return
	}

	if err := s.writeSegmentSizes(pending); err != nil {
		logger.Error("Failed storing segment sizes", "count", len(pending), "error", err)
	}
}

func (s *Store) writeSegmentSizes(sizes map[string]int64) (err error) {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("failed starting transaction: %w", err)
	}
	defer func() {
		if err != nil {
			tx.Rollback()
		}
	}()

	stmt, err := tx.Prepare("INSERT INTO segment (message_id, size) VALUES (?, ?) ON CONFLICT (message_id) DO UPDATE SET size = excluded.size")
	if err != nil {
		return fmt.Errorf("failed preparing insert: %w", err)
	}
	defer stmt.Close()

	for id, size := range sizes {
		if _, err = stmt.Exec(id, size); err != nil {
			return fmt.Errorf("failed storing size of %s: %w", id, err)
		}
	}

	if err = tx.Commit(); err != nil {
		return fmt.Errorf("failed committing segment sizes: %w", err)
	}
	return nil
}
