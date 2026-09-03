// Package sqlstore persists nzb records in SQLite.
package sqlstore

import (
	"bytes"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"git.ruekov.eu/ruakij/nzbStreamer/pkg/nzbparser"
	"github.com/pressly/goose/v3"
	_ "modernc.org/sqlite"
)

//go:embed migrations/sqlite/*.sql
var migrations embed.FS

type Store struct {
	db *sql.DB
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

	return &Store{db: db}, nil
}

func (s *Store) Close() error {
	return s.db.Close()
}

func (s *Store) List() ([]nzbparser.NzbData, error) {
	rows, err := s.db.Query("SELECT name, raw FROM nzb")
	if err != nil {
		return nil, fmt.Errorf("failed listing nzbs: %w", err)
	}
	defer rows.Close()

	var list []nzbparser.NzbData
	for rows.Next() {
		var name string
		var raw []byte
		if err := rows.Scan(&name, &raw); err != nil {
			return nil, fmt.Errorf("failed reading nzb row: %w", err)
		}

		// The name is the identity here, and it comes from the file the nzb was
		// read from rather than from anything inside it, so it is passed back in
		data, err := nzbparser.ParseNzb(bytes.NewReader(raw), "")
		if err != nil {
			return nil, fmt.Errorf("failed parsing stored nzb %s: %w", name, err)
		}
		data.MetaName = name

		list = append(list, *data)
	}

	return list, rows.Err()
}

// ErrNoRaw reports an nzb that was not parsed from bytes, which is the only thing
// worth storing - a re-encoding would preserve what the parser understood today
// rather than the nzb itself.
var ErrNoRaw = errors.New("nzb has no raw bytes")

func (s *Store) Set(data *nzbparser.NzbData) error {
	if len(data.Raw) == 0 {
		return fmt.Errorf("%w: %s", ErrNoRaw, data.MetaName)
	}

	_, err := s.db.Exec(
		"INSERT INTO nzb (name, raw, added_at) VALUES (?, ?, ?) ON CONFLICT (name) DO UPDATE SET raw = excluded.raw",
		data.MetaName, data.Raw, time.Now().Unix(),
	)
	if err != nil {
		return fmt.Errorf("failed storing nzb %s: %w", data.MetaName, err)
	}
	return nil
}

func (s *Store) Delete(data *nzbparser.NzbData) error {
	_, err := s.db.Exec("DELETE FROM nzb WHERE name = ?", data.MetaName)
	if err != nil {
		return fmt.Errorf("failed deleting nzb %s: %w", data.MetaName, err)
	}
	return nil
}
