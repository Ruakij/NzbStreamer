// Package sqlstore persists nzb records in SQLite.
package sqlstore

import (
	"bytes"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"git.ruekov.eu/ruakij/nzbStreamer/internal/nzbstore"
	"git.ruekov.eu/ruakij/nzbStreamer/pkg/nzbparser"
	"github.com/pressly/goose/v3"
	_ "modernc.org/sqlite"
)

//go:embed migrations/sqlite/*.sql
var migrations embed.FS

var logger = slog.With("Module", "SqlStore")

type Store struct {
	db *sql.DB

	// Segment sizes are learned on the read path, so they are buffered here and
	// written by flushLoop instead of by the read
	pendingMutex sync.Mutex
	pending      map[string]int64
	closing      chan struct{}
	flusherDone  sync.WaitGroup
	closeOnce    sync.Once
}

// New opens the database at path, creating it and its directory if needed, and
// migrates it to the current schema.
//
// The pragmas are per-connection settings, so they belong in the DSN where every
// connection the pool opens gets them, not in a one-off Exec that lands on one
// arbitrary connection. WAL keeps a write from blocking the mount's reads,
// synchronous(NORMAL) skips the per-commit fsync because losing the last
// transactions costs a re-add and never correctness, and _txlock=immediate takes
// the write lock up front so two read-then-write transactions queue instead of
// deadlocking past busy_timeout.
func New(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("failed creating directory for %s: %w", path, err)
	}

	dsn := "file:" + path +
		"?_pragma=journal_mode(WAL)" +
		"&_pragma=busy_timeout(5000)" +
		"&_pragma=foreign_keys(ON)" +
		"&_pragma=synchronous(NORMAL)" +
		"&_txlock=immediate"

	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("failed opening database %s: %w", path, err)
	}

	goose.SetBaseFS(migrations)
	goose.SetLogger(goose.NopLogger())
	if err := goose.SetDialect("sqlite"); err != nil {
		db.Close()
		return nil, fmt.Errorf("failed selecting goose dialect: %w", err)
	}
	if err := goose.Up(db, "migrations/sqlite"); err != nil {
		db.Close()
		return nil, fmt.Errorf("failed migrating database %s: %w", path, err)
	}

	store := &Store{
		db:      db,
		pending: make(map[string]int64),
		closing: make(chan struct{}),
	}
	store.flusherDone.Add(1)
	go store.flushLoop()

	return store, nil
}

// Close flushes what the read path has learned and shuts the database down.
func (s *Store) Close() error {
	var err error
	s.closeOnce.Do(func() {
		close(s.closing)
		s.flusherDone.Wait()
		err = s.db.Close()
	})

	return err
}

func (s *Store) List() ([]nzbstore.Record, error) {
	rows, err := s.db.Query("SELECT name, raw, stage, error, added_at, finished_at FROM nzb ORDER BY added_at")
	if err != nil {
		return nil, fmt.Errorf("failed listing nzbs: %w", err)
	}
	defer rows.Close()

	var list []nzbstore.Record
	for rows.Next() {
		var record nzbstore.Record
		var name string
		var raw []byte
		var addedAt int64
		var finishedAt sql.NullInt64
		if err := rows.Scan(&name, &raw, &record.Stage, &record.Err, &addedAt, &finishedAt); err != nil {
			return nil, fmt.Errorf("failed reading nzb row: %w", err)
		}

		// The name is the identity here, and it comes from the file the nzb was
		// read from rather than from anything inside it, so it is passed back in
		data, err := nzbparser.ParseNzb(bytes.NewReader(raw), "")
		if err != nil {
			return nil, fmt.Errorf("failed parsing stored nzb %s: %w", name, err)
		}
		data.MetaName = name

		record.Data = data
		record.AddedAt = time.Unix(addedAt, 0)
		if finishedAt.Valid {
			record.FinishedAt = time.Unix(finishedAt.Int64, 0)
		}

		list = append(list, record)
	}

	return list, rows.Err()
}

// ErrNoRaw reports an nzb that was not parsed from bytes, which is the only thing
// worth storing - a re-encoding would preserve what the parser understood today
// rather than the nzb itself.
var ErrNoRaw = errors.New("nzb has no raw bytes")

// Add records an accepted nzb. A name recorded before is superseded, since the
// later attempt is the one worth reporting and the earlier one is over.
func (s *Store) Add(data *nzbparser.NzbData, stage string) error {
	if len(data.Raw) == 0 {
		return fmt.Errorf("%w: %s", ErrNoRaw, data.MetaName)
	}

	_, err := s.db.Exec(
		"INSERT INTO nzb (name, raw, stage, added_at) VALUES (?, ?, ?, ?)"+
			" ON CONFLICT (name) DO UPDATE SET raw = excluded.raw, stage = excluded.stage, error = '', added_at = excluded.added_at, finished_at = NULL",
		data.MetaName, data.Raw, stage, time.Now().Unix(),
	)
	if err != nil {
		return fmt.Errorf("failed storing nzb %s: %w", data.MetaName, err)
	}
	return nil
}

func (s *Store) SetStage(name, stage, errMessage string) error {
	_, err := s.db.Exec(
		"UPDATE nzb SET stage = ?, error = ?, finished_at = ? WHERE name = ?",
		stage, errMessage, time.Now().Unix(), name,
	)
	if err != nil {
		return fmt.Errorf("failed recording stage of %s: %w", name, err)
	}
	return nil
}

func (s *Store) Delete(name string) error {
	_, err := s.db.Exec("DELETE FROM nzb WHERE name = ?", name)
	if err != nil {
		return fmt.Errorf("failed deleting nzb %s: %w", name, err)
	}
	return nil
}
