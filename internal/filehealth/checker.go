package filehealth

import (
	"errors"
	"fmt"
	"io"

	"git.ruekov.eu/ruakij/nzbStreamer/internal/presentation"
)

type CheckerConfig struct {
	TryReadBytes      int64
	TryReadPercentage float32
}

// Ensure DefaultChecker implements Checker interface
var _ Checker = (*DefaultChecker)(nil)

// DefaultChecker implements basic file health checking
type DefaultChecker struct {
	config CheckerConfig
}

func NewDefaultChecker(config CheckerConfig) *DefaultChecker {
	return &DefaultChecker{config: config}
}

// FileHealthError represents a file health check error
type FileHealthError struct {
	Path string
	Err  error
}

func (e *FileHealthError) Error() string {
	return fmt.Sprintf("health check failed for %s: %v", e.Path, e.Err)
}

func (c *DefaultChecker) CheckFiles(files map[string]presentation.Openable) []error {
	if c.config.TryReadBytes <= 0 && c.config.TryReadPercentage <= 0 {
		return nil
	}

	var errs []error
	for path, file := range files {
		if err := c.checkFile(file); err != nil {
			errs = append(errs, &FileHealthError{
				Path: path,
				Err:  err,
			})
		}
	}
	return errs
}

func (c *DefaultChecker) checkFile(file presentation.Openable) error {
	f, err := file.Open()
	if err != nil {
		return fmt.Errorf("failed to open file: %w", err)
	}
	defer f.Close()

	size, err := f.Seek(0, io.SeekEnd)
	if err != nil {
		return fmt.Errorf("failed to get file size: %w", err)
	}

	// Calculate how much to read
	var readSize int64
	if c.config.TryReadBytes > 0 {
		readSize = c.config.TryReadBytes
	} else {
		readSize = int64(float32(size) * c.config.TryReadPercentage)
	}

	if readSize == 0 {
		readSize = 1 // Read at least 1 byte
	}
	if readSize > size {
		readSize = size
	}

	// Read from beginning
	_, err = f.Seek(0, io.SeekStart)
	if err != nil {
		return fmt.Errorf("failed to seek to start: %w", err)
	}

	buf := make([]byte, min(readSize, 1*1024*1024))
	var totalRead int64
	for totalRead < readSize {
		n, err := f.Read(buf)
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return fmt.Errorf("failed to read file: %w", err)
		}
		totalRead += int64(n)
	}

	return nil
}
