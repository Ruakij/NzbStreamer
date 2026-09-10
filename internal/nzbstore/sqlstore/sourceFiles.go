package sqlstore

import (
	"database/sql"
	"fmt"
	"time"

	"git.ruekov.eu/ruakij/nzbStreamer/internal/nzbstore"
)

// EnsureSourceFiles records the nzb's own files, the rows a health verdict
// attaches to. The insert leaves an existing row alone, so a call that runs
// while a pass has already recorded a retry_after cannot reset it: the first
// answer a file's post date contributed is the one that stays.
func (s *Store) EnsureSourceFiles(nzbName string, files []nzbstore.SourceFile) error {
	for _, file := range files {
		_, err := s.db.Exec(
			"INSERT INTO nzb_source_file (nzb_name, filename, posted_at) VALUES (?, ?, ?)"+
				" ON CONFLICT (nzb_name, filename) DO NOTHING",
			nzbName, file.Filename, file.PostedAt.Unix(),
		)
		if err != nil {
			return fmt.Errorf("failed storing source file %s of %s: %w", file.Filename, nzbName, err)
		}
	}
	return nil
}

func (s *Store) SetRetryAfter(nzbName, filename string, after time.Time) error {
	_, err := s.db.Exec(
		"UPDATE nzb_source_file SET retry_after = ? WHERE nzb_name = ? AND filename = ?",
		after.Unix(), nzbName, filename,
	)
	if err != nil {
		return fmt.Errorf("failed storing retry_after of %s of %s: %w", filename, nzbName, err)
	}
	return nil
}

// SegmentVerdicts answers what is known about the segments of the named source
// files, in one query per chunk rather than one per segment. Only segments
// something was learned about have rows, and an absent entry is exactly the
// state of a segment nothing has touched.
func (s *Store) SegmentVerdicts(nzbName string, filenames []string) (map[string]map[int]nzbstore.SegmentVerdict, error) {
	verdicts := make(map[string]map[int]nzbstore.SegmentVerdict)

	for start := 0; start < len(filenames); start += lookupChunk {
		batch := filenames[start:min(start+lookupChunk, len(filenames))]

		args := make([]any, 0, len(batch)+1)
		args = append(args, nzbName)
		for _, filename := range batch {
			args = append(args, filename)
		}

		rows, err := s.db.Query(
			"SELECT f.filename, s.index_, s.message_id, coalesce(s.size, -1), s.present, coalesce(s.checked_at, 0), coalesce(s.fetched_at, 0), s.fetches"+
				" FROM segment s JOIN nzb_source_file f ON f.id = s.file_id"+
				" WHERE f.nzb_name = ? AND f.filename IN ("+placeholders(len(batch))+")",
			args...)
		if err != nil {
			return nil, fmt.Errorf("failed reading segment verdicts of %s: %w", nzbName, err)
		}
		for rows.Next() {
			var verdict nzbstore.SegmentVerdict
			var present, checkedAt, fetchedAt int64
			if err := rows.Scan(&verdict.Filename, &verdict.Index, &verdict.MessageID,
				&verdict.Size, &present, &checkedAt, &fetchedAt, &verdict.Fetches); err != nil {
				rows.Close()
				return nil, fmt.Errorf("failed reading segment verdict row of %s: %w", nzbName, err)
			}
			verdict.Present = present != 0
			verdict.CheckedAt = time.Unix(checkedAt, 0)
			verdict.FetchedAt = time.Unix(fetchedAt, 0)
			if verdicts[verdict.Filename] == nil {
				verdicts[verdict.Filename] = make(map[int]nzbstore.SegmentVerdict)
			}
			verdicts[verdict.Filename][verdict.Index] = verdict
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, fmt.Errorf("failed reading segment verdicts of %s: %w", nzbName, err)
		}
		rows.Close()
	}

	return verdicts, nil
}

// RecordProbes writes what a scan or a read observed. Probes are rare next to
// the reads that stream a file, so they flush here and now rather than riding
// the read path's buffered activity: a verdict that decides whether a file
// stays presented is worth one synchronous write.
//
// A probe for a source file the store has no row for is an error rather than a
// dropped row: a probe is the evidence a removal acts on, and quietly losing
// one leaves a damaged file presented.
func (s *Store) RecordProbes(nzbName string, probes []nzbstore.ProbeResult) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("failed storing probes of %s: %w", nzbName, err)
	}
	defer tx.Rollback() //nolint:errcheck // a committed transaction rolls back to nothing

	fileIDs, err := s.sourceFileIDs(tx, nzbName, probeFilenames(probes))
	if err != nil {
		return err
	}

	check, err := tx.Prepare(
		"INSERT INTO segment (file_id, index_, message_id, checked_at, present) VALUES (?, ?, ?, ?, ?)" +
			" ON CONFLICT (file_id, index_) DO UPDATE SET message_id = excluded.message_id, checked_at = excluded.checked_at, present = excluded.present")
	if err != nil {
		return fmt.Errorf("failed preparing probe insert: %w", err)
	}
	defer check.Close()

	missing, err := tx.Prepare(
		"INSERT INTO segment_missing (file_id, index_, server, checked_at) VALUES (?, ?, ?, ?)" +
			" ON CONFLICT (file_id, index_, server) DO UPDATE SET checked_at = excluded.checked_at")
	if err != nil {
		return fmt.Errorf("failed preparing missing insert: %w", err)
	}
	defer missing.Close()

	now := time.Now().Unix()
	for _, probe := range probes {
		fileID, ok := fileIDs[probe.Filename]
		if !ok {
			return fmt.Errorf("probe of %s of %s: no source file row, EnsureSourceFiles was not called", probe.Filename, nzbName)
		}

		if _, err := check.Exec(fileID, probe.Index, probe.MessageID, now, probe.Present); err != nil {
			return fmt.Errorf("failed storing probe of %s of %s: %w", probe.Filename, nzbName, err)
		}
		if probe.Server != "" && !probe.Present {
			// The per-server row is only for a no: a yes says nothing about
			// which server will be asked next
			if _, err := missing.Exec(fileID, probe.Index, probe.Server, now); err != nil {
				return fmt.Errorf("failed storing missing on %s of %s of %s: %w", probe.Server, probe.Filename, nzbName, err)
			}
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("failed storing probes of %s: %w", nzbName, err)
	}
	return nil
}

// RemoveFiles deletes the nzb_file rows of the given paths, so a presented file
// pulled back by a verdict does not reappear on a restart.
func (s *Store) RemoveFiles(name string, paths []string) error {
	for start := 0; start < len(paths); start += lookupChunk {
		batch := paths[start:min(start+lookupChunk, len(paths))]

		args := make([]any, 0, len(batch)+1)
		args = append(args, name)
		for _, p := range batch {
			args = append(args, p)
		}

		if _, err := s.db.Exec("DELETE FROM nzb_file WHERE nzb_name = ? AND path IN ("+placeholders(len(batch))+")", args...); err != nil {
			return fmt.Errorf("failed removing files of %s: %w", name, err)
		}
	}
	return nil
}

// sourceFileIDs resolves the ids of the named source files in chunks, for
// writers that key their reports by (nzb name, filename) and need the row id to
// reach a segment.
func (s *Store) sourceFileIDs(q interface {
	Query(string, ...any) (*sql.Rows, error)
}, nzbName string, filenames []string) (map[string]int64, error) {
	ids := make(map[string]int64, len(filenames))

	for start := 0; start < len(filenames); start += lookupChunk {
		batch := filenames[start:min(start+lookupChunk, len(filenames))]

		args := make([]any, 0, len(batch)+1)
		args = append(args, nzbName)
		for _, filename := range batch {
			args = append(args, filename)
		}

		rows, err := q.Query(
			"SELECT id, filename FROM nzb_source_file WHERE nzb_name = ? AND filename IN ("+placeholders(len(batch))+")",
			args...)
		if err != nil {
			return nil, fmt.Errorf("failed reading source file ids of %s: %w", nzbName, err)
		}
		for rows.Next() {
			var id int64
			var filename string
			if err := rows.Scan(&id, &filename); err != nil {
				rows.Close()
				return nil, fmt.Errorf("failed reading source file id row of %s: %w", nzbName, err)
			}
			ids[filename] = id
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, fmt.Errorf("failed reading source file ids of %s: %w", nzbName, err)
		}
		rows.Close()
	}

	return ids, nil
}

func probeFilenames(probes []nzbstore.ProbeResult) []string {
	seen := make(map[string]struct{}, len(probes))
	filenames := make([]string, 0, len(probes))
	for _, probe := range probes {
		if _, ok := seen[probe.Filename]; ok {
			continue
		}
		seen[probe.Filename] = struct{}{}
		filenames = append(filenames, probe.Filename)
	}
	return filenames
}
