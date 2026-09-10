package sqlstore

import (
	"fmt"
	"log/slog"
	"strings"
	"time"
)

// flushInterval bounds how long what a read learned stays only in memory.
// Losing the buffer costs a re-measurement, never correctness, so there is
// nothing to gain from flushing sooner.
const flushInterval = 5 * time.Second

// lookupChunk keeps a statement well under the bound on placeholders, which a
// release-sized nzb would otherwise reach in one go.
const lookupChunk = 500

func placeholders(n int) string {
	return "?" + strings.Repeat(",?", n-1)
}

// segmentKey locates a segment as the nzb addresses it: its own file within the
// nzb, and its position within that file. A decoded length and the activity of
// a read belong to a segment of a file of one nzb, which is the hierarchy the
// verdicts hang off; the message-id rides along because the segment row states
// it, and the same post named by another nzb gets a row there of its own.
type segmentKey struct {
	nzbName  string
	filename string
	index    int
}

// activity is what a read path has observed about a segment since the last
// flush. A read is counted whether the bytes came from the cache or from the
// server; a fetch is only the latter.
type activity struct {
	messageID string
	size      int64
	fetched   bool
	read      bool
}

// SegmentSizes returns the known decoded lengths among the message-ids of one
// nzb. Absent ids are absent from the map; not knowing one is the normal state,
// not an error.
func (s *Store) SegmentSizes(nzbName string, ids []string) (map[string]int64, error) {
	sizes := make(map[string]int64, len(ids))

	for start := 0; start < len(ids); start += lookupChunk {
		batch := ids[start:min(start+lookupChunk, len(ids))]

		args := make([]any, len(batch)+1)
		args[0] = nzbName
		for i, id := range batch {
			args[i+1] = id
		}

		rows, err := s.db.Query(
			"SELECT s.message_id, s.size FROM segment s JOIN nzb_source_file f ON f.id = s.file_id"+
				" WHERE f.nzb_name = ? AND s.size IS NOT NULL AND s.message_id IN ("+placeholders(len(batch))+")",
			args...)
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

// RecordSegmentSize notes the decoded length of a segment, which a fetch has
// just measured. It is called from the read path, so it buffers rather than
// writing, and a read never waits on the database.
func (s *Store) RecordSegmentSize(nzbName, filename, messageID string, index int, size int64) {
	s.pendingMutex.Lock()
	defer s.pendingMutex.Unlock()

	key := segmentKey{nzbName: nzbName, filename: filename, index: index}
	pending := s.pending[key]
	pending.messageID, pending.size, pending.fetched = messageID, size, true
	s.pending[key] = pending
}

// RecordSegmentRead notes that a segment was read, from the cache or from the
// server. What was read within a timespan is the working set the cache has to
// hold, so this is called on every read and not only on the ones that missed.
func (s *Store) RecordSegmentRead(nzbName, filename string, index int) {
	s.pendingMutex.Lock()
	defer s.pendingMutex.Unlock()

	key := segmentKey{nzbName: nzbName, filename: filename, index: index}
	pending := s.pending[key]
	pending.read = true
	s.pending[key] = pending
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
	s.pending = make(map[segmentKey]activity, len(pending))
	s.pendingMutex.Unlock()

	if len(pending) == 0 {
		return
	}

	if err := s.writeSegmentActivity(pending); err != nil {
		slog.Error("Failed storing segment activity", "count", len(pending), "error", err)
	}
}

// writeSegmentActivity stamps the buffer with the time it is written, so the
// times are as coarse as flushInterval. Nothing reads them at a finer grain than
// a window of hours.
//
// A key whose source file does not resolve - an nzb deleted while its reads
// were still buffered, or a file nothing ever ensured - is skipped: it costs a
// few seconds of activity metrics, never correctness.
func (s *Store) writeSegmentActivity(pending map[segmentKey]activity) (err error) {
	type pair struct {
		nzbName  string
		filename string
	}
	filenames := make(map[string]map[string]struct{})
	for key := range pending {
		if filenames[key.nzbName] == nil {
			filenames[key.nzbName] = make(map[string]struct{})
		}
		filenames[key.nzbName][key.filename] = struct{}{}
	}

	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("failed starting transaction: %w", err)
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()

	fileIDs := make(map[pair]int64)
	for nzbName, names := range filenames {
		list := make([]string, 0, len(names))
		for filename := range names {
			list = append(list, filename)
		}
		ids, err := s.sourceFileIDs(tx, nzbName, list)
		if err != nil {
			return err
		}
		for filename, id := range ids {
			fileIDs[pair{nzbName: nzbName, filename: filename}] = id
		}
	}

	// The row count comes back so a second fetch of the same segment can be
	// counted where it is the only place that can see it was one. A fetch that
	// returned bytes is itself the yes a probe would have asked for, so the
	// row it creates is present; a conflict leaves the answer a probe stored
	// alone, the fetch says nothing about the rotation.
	fetch, err := tx.Prepare(
		"INSERT INTO segment (file_id, index_, message_id, size, present, fetched_at, fetches) VALUES (?, ?, ?, ?, 1, ?, 1)" +
			" ON CONFLICT (file_id, index_) DO UPDATE SET message_id = excluded.message_id, size = excluded.size, fetched_at = excluded.fetched_at, fetches = segment.fetches + 1" +
			" RETURNING fetches")
	if err != nil {
		return fmt.Errorf("failed preparing insert: %w", err)
	}
	defer fetch.Close()

	// A read of a segment with no row is one whose size was never learned,
	// which leaves it out of the working set until it is fetched again
	read, err := tx.Prepare("UPDATE segment SET read_at = ? WHERE file_id = ? AND index_ = ?")
	if err != nil {
		return fmt.Errorf("failed preparing update: %w", err)
	}
	defer read.Close()

	now := time.Now().Unix()
	unresolved := 0
	for key, seen := range pending {
		fileID, ok := fileIDs[pair{nzbName: key.nzbName, filename: key.filename}]
		if !ok {
			unresolved++
			continue
		}

		if seen.fetched {
			var fetches int64
			if err = fetch.QueryRow(fileID, key.index, seen.messageID, seen.size, now).Scan(&fetches); err != nil {
				return fmt.Errorf("failed storing size of %s of %s: %w", key.filename, key.nzbName, err)
			}
			if fetches > 1 {
				s.refetchedBytes.Add(seen.size)
				s.refetchedSegments.Add(1)
			}
		}
		if seen.read {
			if _, err = read.Exec(now, fileID, key.index); err != nil {
				return fmt.Errorf("failed storing read of %s of %s: %w", key.filename, key.nzbName, err)
			}
		}
	}
	if unresolved > 0 {
		slog.Debug("Segment activity skipped for files the store has no rows for",
			"segments", unresolved)
	}

	if err = tx.Commit(); err != nil {
		return fmt.Errorf("failed committing segment activity: %w", err)
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
// The same post can sit under several files of one nzb, so the scan groups by
// message-id first and every measurement counts a post once, whatever the rows
// say.
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
			fmt.Sprintf("coalesce(sum(CASE WHEN last_read > ?%d THEN size END), 0)", i+1),
			fmt.Sprintf("coalesce(sum(CASE WHEN last_read > ?%d THEN 1 END), 0)", i+1))
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

	// max(size) picks a row's measured length over the nulls of the sibling
	// rows the same post earned under another nzb
	query := "SELECT " + strings.Join(columns, ", ") +
		fmt.Sprintf(" FROM (SELECT message_id, max(read_at) AS last_read, max(size) AS size FROM segment WHERE read_at > ?%d GROUP BY message_id)", len(cutoffs)+1)
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
