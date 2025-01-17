package diskcache

import (
	"fmt"
	"os"
	"path/filepath"
)

const DirFileMode = 0o755

func ensureDirExists(dirPath string) error {
	if _, err := os.Stat(dirPath); os.IsNotExist(err) {
		err := os.Mkdir(dirPath, DirFileMode)
		if err != nil {
			return fmt.Errorf("failed creating dir '%s': %w", dirPath, err)
		}
		return nil
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
