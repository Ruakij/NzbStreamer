package iofsops

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path/filepath"
)

var ErrPathNotDirectory = errors.New("not a directory")

// Converts the Filesystem files, starting from start, into Files with their full path
func BuildFileList(filesystem fs.FS, start string) (map[string]fs.FileInfo, error) {
	result := make(map[string]fs.FileInfo, 1)

	// Using a stack represented by slice
	type pathInfo struct {
		path     string
		fullPath string
	}

	stack := []pathInfo{{start, start}}

	for len(stack) > 0 {
		// Pop from stack
		n := len(stack) - 1
		current := stack[n]
		stack = stack[:n]

		file, err := filesystem.Open(current.path)
		if err != nil {
			return nil, fmt.Errorf("failed opening file: %w", err)
		}

		dir, ok := file.(fs.ReadDirFile)
		if !ok {
			file.Close()
			return nil, fmt.Errorf("failed converting path '%s': %w", current.path, ErrPathNotDirectory)
		}

		entries, err := dir.ReadDir(-1)
		file.Close()
		if err != nil && errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("failed reading dir: %w", err)
		}
		for _, entry := range entries {
			entryPath := filepath.Join(current.path, entry.Name())
			fullEntryPath := filepath.Join(current.fullPath, entry.Name())
			info, err := entry.Info()
			if err != nil {
				return nil, fmt.Errorf("failed reading info from file '%s': %w", entryPath, err)
			}

			if entry.IsDir() {
				// Push directory onto the stack
				stack = append(stack, pathInfo{entryPath, fullEntryPath})
			} else {
				result[fullEntryPath] = info
			}
		}

	}
	return result, nil
}
