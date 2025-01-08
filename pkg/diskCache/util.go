package diskCache

import (
	"os"
	"path/filepath"
)

func ensureDirExists(dirPath string) error {
	if _, err := os.Stat(dirPath); os.IsNotExist(err) {
		return os.MkdirAll(dirPath, 0755)
	}
	return nil
}

func clearDirectory(dirPath string) error {
	entries, err := os.ReadDir(dirPath)
	if err != nil {
		return err
	}

	for _, entry := range entries {
		entryPath := filepath.Join(dirPath, entry.Name())
		err := os.RemoveAll(entryPath)
		if err != nil {
			return err
		}
	}

	return nil
}
