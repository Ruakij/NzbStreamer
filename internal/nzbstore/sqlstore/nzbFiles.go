package sqlstore

import (
	"fmt"

	"git.ruekov.eu/ruakij/nzbStreamer/internal/nzbstore"
)

// SetFiles records what an nzb presents, together with the settings key it was
// built under. Rows and key move together in one transaction, since a key
// pointing at the wrong rows is worse than no rows at all.
func (s *Store) SetFiles(name, treeKey string, files []nzbstore.File) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("failed storing files of %s: %w", name, err)
	}
	defer tx.Rollback() //nolint:errcheck // a committed transaction rolls back to nothing

	if _, err := tx.Exec("DELETE FROM nzb_file WHERE nzb_name = ?", name); err != nil {
		return fmt.Errorf("failed clearing files of %s: %w", name, err)
	}

	for _, file := range files {
		if _, err := tx.Exec(
			"INSERT INTO nzb_file (nzb_name, path, size, exact) VALUES (?, ?, ?, ?)",
			name, file.Path, file.Size, file.Exact,
		); err != nil {
			return fmt.Errorf("failed storing file %s of %s: %w", file.Path, name, err)
		}
	}

	if _, err := tx.Exec("UPDATE nzb SET tree_key = ? WHERE name = ?", treeKey, name); err != nil {
		return fmt.Errorf("failed storing tree key of %s: %w", name, err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("failed storing files of %s: %w", name, err)
	}
	return nil
}

func (s *Store) Files(name string) ([]nzbstore.File, error) {
	rows, err := s.db.Query("SELECT path, size, exact FROM nzb_file WHERE nzb_name = ?", name)
	if err != nil {
		return nil, fmt.Errorf("failed listing files of %s: %w", name, err)
	}
	defer rows.Close()

	var files []nzbstore.File
	for rows.Next() {
		var file nzbstore.File
		if err := rows.Scan(&file.Path, &file.Size, &file.Exact); err != nil {
			return nil, fmt.Errorf("failed reading file row of %s: %w", name, err)
		}
		files = append(files, file)
	}

	return files, rows.Err()
}
