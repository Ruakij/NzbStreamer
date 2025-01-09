package ioFsOps

import (
	"fmt"
	"io"
	"io/fs"
	"path/filepath"
)

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
			return nil, err
		}
		defer file.Close()

		if dir, ok := file.(fs.ReadDirFile); ok {
			entries, err := dir.ReadDir(-1)
			if err != nil && err != io.EOF {
				return nil, err
			}
			for _, entry := range entries {
				entryPath := filepath.Join(current.path, entry.Name())
				fullEntryPath := filepath.Join(current.fullPath, entry.Name())
				info, err := entry.Info()
				if err != nil {
					return nil, err
				}

				if entry.IsDir() {
					// Push directory onto the stack
					stack = append(stack, pathInfo{entryPath, fullEntryPath})
				} else {
					result[fullEntryPath] = info
				}
			}
		} else {
			return nil, fmt.Errorf("path %s is not a directory", current.path)
		}
	}
	return result, nil
}
