package sqlstore

import (
	"fmt"
	"log/slog"
	"strings"
	"time"
)

// flushInterval bounds how long a learned size stays only in memory. Losing the
// buffer costs a re-measurement, never correctness, so there is nothing to gain
// from flushing sooner.
const flushInterval = 5 * time.Second

// lookupChunk keeps a statement well under the bound on placeholders, which a
// release-sized nzb would otherwise reach in one go.
const lookupChunk = 500

func placeholders(n int) string {
	return "?" + strings.Repeat(",?", n-1)
}

// A decoded segment length is a fact about an immutable post, so it is only ever
// learned, never invalidated, and two nzbs describing the same post share it.
// That is why the key is the message-id and not a position in an nzb.

// activity is what a read path has observed about a segment since the last
// flush. A read is counted whether the bytes came from the cache or from the
// server; a fetch is only the latter.
type activity struct {
	size    int64
	fetched bool
	read    bool
}

// SegmentSizes returns the known decoded lengths among ids. Absent ids are
// absent from the map; not knowing one is the normal state, not an error.
func (s *Store) SegmentSizes(ids []string) (map[string]int64, error) {
	sizes := make(map[string]int64, len(ids))

	for start := 0; start < len(ids); start += lookupChunk {
		batch := ids[start:min(start+lookupChunk, len(ids))]

		args := make([]any, len(batch))
		for i, id := range batch {
			args[i] = id
		}

		rows, err := s.db.Query("SELECT message_id, size FROM segment WHERE message_id IN ("+placeholders(len(batch))+")", args...)
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

	pending := s.pending[messageID]
	pending.size, pending.fetched = size, true
	s.pending[messageID] = pending
}

// RecordSegmentRead notes that a segment was read, from the cache or from the
// server. What was read within a timespan is the working set the cache has to
// hold, so this is called on every read and not only on the ones that missed.
func (s *Store) RecordSegmentRead(messageID string) {
	s.pendingMutex.Lock()
	defer s.pendingMutex.Unlock()

	pending := s.pending[messageID]
	pending.read = true
	s.pending[messageID] = pending
}

// ForgetSegments drops what is known about ids, for posts nobody will read
// again. A size is knowledge that costs a fetch to regain and nothing to lose,
// so this is the only thing that ever removes one.
func (s *Store) ForgetSegments(ids []string) error {
	s.pendingMutex.Lock()
	for _, id := range ids {
		delete(s.pending, id)
	}
	s.pendingMutex.Unlock()

	for start := 0; start < len(ids); start += lookupChunk {
		batch := ids[start:min(start+lookupChunk, len(ids))]

		args := make([]any, len(batch))
		for i, id := range batch {
			args[i] = id
		}

		_, err := s.db.Exec("DELETE FROM segment WHERE message_id IN ("+placeholders(len(batch))+")", args...)
		if err != nil {
			return fmt.Errorf("failed forgetting segment sizes: %w", err)
		}
	}

	return nil
}

func (s *Store) flushLoop() {
	defer s.flusherDone.Done()

	ticker := time.NewTicker(flushInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			s.flushSegmentSizes()
			s.flushServerUsage()
		case <-s.closing:
			s.flushSegmentSizes()
			s.flushServerUsage()
			return
		}
	}
}

func (s *Store) flushSegmentSizes() {
	s.pendingMutex.Lock()
	pending := s.pending
	s.pending = make(map[string]activity, len(pending))
	s.pendingMutex.Unlock()

	if len(pending) == 0 {
		return
	}

	if err := s.writeSegmentActivity(pending); err != nil {
		slog.Error("Failed storing segment sizes", "count", len(pending), "error", err)
	}
}

// writeSegmentActivity stamps the buffer with the time it is written, so the
// times are as coarse as flushInterval. Nothing reads them at a finer grain than
// a window of hours.
func (s *Store) writeSegmentActivity(pending map[string]activity) (err error) {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("failed starting transaction: %w", err)
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()

	// The row count comes back so a second fetch of the same segment can be
	// counted where it is the only place that can see it was one
	fetch, err := tx.Prepare(
		"INSERT INTO segment (message_id, size, fetched_at, fetches) VALUES (?, ?, ?, 1)" +
			" ON CONFLICT (message_id) DO UPDATE SET size = excluded.size, fetched_at = excluded.fetched_at, fetches = segment.fetches + 1" +
			" RETURNING fetches")
	if err != nil {
		return fmt.Errorf("failed preparing insert: %w", err)
	}
	defer fetch.Close()

	// A read of a segment with no row is one whose size was forgotten while its
	// cached bytes were not, which leaves it out of the working set until it is
	// fetched again
	read, err := tx.Prepare("UPDATE segment SET read_at = ? WHERE message_id = ?")
	if err != nil {
		return fmt.Errorf("failed preparing update: %w", err)
	}
	defer read.Close()

	now := time.Now().Unix()
	for id, seen := range pending {
		if seen.fetched {
			var fetches int64
			if err = fetch.QueryRow(id, seen.size, now).Scan(&fetches); err != nil {
				return fmt.Errorf("failed storing size of %s: %w", id, err)
			}
			if fetches > 1 {
				s.refetchedBytes.Add(seen.size)
				s.refetchedSegments.Add(1)
			}
		}
		if seen.read {
			if _, err = read.Exec(now, id); err != nil {
				return fmt.Errorf("failed storing read of %s: %w", id, err)
			}
		}
	}

	if err = tx.Commit(); err != nil {
		return fmt.Errorf("failed committing segment sizes: %w", err)
	}
	return nil
}

// SegmentActivity is what the segments read within a window say about the cache.
// It is a window rather than a running total because it is a set: a segment read
// a hundred times is one segment of the working set, and a cardinality cannot be
// summed back out of per-scrape values the way a count of events can.
type SegmentActivity struct {
	// WorkingSetBytes is the bytes of the distinct segments read in the window,
	// which is the size a cache would have to have to hold all of them
	WorkingSetBytes int64
	// WorkingSetSegments is the same measured in segments, which against the
	// bytes is the mean size of what is being read
	WorkingSetSegments int64
}

// SegmentActivitySince measures the reads since each of the given points in
// time, returning one measurement per cutoff in the order they were asked for.
// Every cutoff is a conditional sum over the same scan rather than a scan of its
// own, and the scan is bounded by the oldest of them.
//
// It reads the whole segment table, so it belongs behind a cached value rather
// than in a request.
func (s *Store) SegmentActivitySince(cutoffs []time.Time) ([]SegmentActivity, error) {
	if len(cutoffs) == 0 {
		return nil, nil
	}

	args := make([]any, len(cutoffs)+1)
	columns := make([]string, 0, len(cutoffs)*2)
	oldest := cutoffs[0]
	for i, cutoff := range cutoffs {
		args[i] = cutoff.Unix()
		columns = append(columns,
			fmt.Sprintf("coalesce(sum(CASE WHEN read_at > ?%d THEN size END), 0)", i+1),
			fmt.Sprintf("coalesce(sum(CASE WHEN read_at > ?%d THEN 1 END), 0)", i+1))
		if cutoff.Before(oldest) {
			oldest = cutoff
		}
	}
	args[len(cutoffs)] = oldest.Unix()

	activity := make([]SegmentActivity, len(cutoffs))
	scan := make([]any, 0, len(cutoffs)*2)
	for i := range activity {
		scan = append(scan, &activity[i].WorkingSetBytes, &activity[i].WorkingSetSegments)
	}

	query := "SELECT " + strings.Join(columns, ", ") +
		fmt.Sprintf(" FROM segment WHERE read_at > ?%d", len(cutoffs)+1)
	if err := s.db.QueryRow(query, args...).Scan(scan...); err != nil {
		return nil, fmt.Errorf("failed measuring segment activity: %w", err)
	}

	return activity, nil
}

// Refetches is what has been downloaded a second time since the process started.
// Every refetch is an event with a size, so unlike the working set this counts
// up and leaves the window to whatever reads it.
func (s *Store) Refetches() (bytes, segments int64) {
	return s.refetchedBytes.Load(), s.refetchedSegments.Load()
}
