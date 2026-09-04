package diskcache

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const DirFileMode = 0o755

var ErrInvalidKey = errors.New("invalid cache key")

// Key names an item. Its parts are directories and the last one is the file, so
// a caller groups what belongs together without knowing where the cache puts it.
type Key []string

func (k Key) String() string {
	return strings.Join(k, "/")
}

// path places a key under dir.
func (k Key) path(dir string) (string, error) {
	if len(k) == 0 {
		return "", fmt.Errorf("%w: empty", ErrInvalidKey)
	}
	for _, part := range k {
		if part == "" || part == "." || part == ".." || strings.ContainsRune(part, filepath.Separator) || strings.ContainsRune(part, '/') {
			return "", fmt.Errorf("%w: '%s'", ErrInvalidKey, part)
		}
	}
	return filepath.Join(append([]string{dir}, k...)...), nil
}

func ensureDirExists(dirPath string) error {
	if err := os.MkdirAll(dirPath, DirFileMode); err != nil {
		return fmt.Errorf("failed creating dir '%s': %w", dirPath, err)
	}
	return nil
}

func clearDirectory(dirPath string) error {
	entries, err := os.ReadDir(dirPath)
	if err != nil {
		return fmt.Errorf(": %w", err)
	}

	for _, entry := range entries {
		entryPath := filepath.Join(dirPath, entry.Name())
		err := os.RemoveAll(entryPath)
		if err != nil {
			return fmt.Errorf("failed clearing directory '%s': %w", dirPath, err)
		}
	}

	return nil
}
