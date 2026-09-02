package filehealth

import (
	"errors"
	"fmt"
	"sync"

	"git.ruekov.eu/ruakij/nzbStreamer/pkg/nzbparser"
)

var ErrSegmentsMissing = errors.New("segments missing on server")

// SegmentExistsFunc reports whether a segment is retrievable from the server.
type SegmentExistsFunc func(id string) (bool, error)

type CheckerConfig struct {
	// Segments checked per file, spread evenly across it; 0 disables checking, -1 checks all
	SegmentsPerFile int
	// Maximum concurrent segment-checks
	MaxParallel int
}

// Ensure DefaultChecker implements Checker interface
var _ Checker = (*DefaultChecker)(nil)

// DefaultChecker verifies that a files segments are still present on the server.
type DefaultChecker struct {
	config CheckerConfig
	exists SegmentExistsFunc
}

func NewDefaultChecker(config CheckerConfig, exists SegmentExistsFunc) *DefaultChecker {
	if config.MaxParallel < 1 {
		config.MaxParallel = 1
	}
	return &DefaultChecker{config: config, exists: exists}
}

// FileHealthError represents a file health check error
type FileHealthError struct {
	Path string
	Err  error
}

func (e *FileHealthError) Error() string {
	return fmt.Sprintf("health check failed for %s: %v", e.Path, e.Err)
}

func (e *FileHealthError) Unwrap() error {
	return e.Err
}

type fileResult struct {
	checked int
	missing int
	err     error
}

func (c *DefaultChecker) CheckFiles(nzbData *nzbparser.NzbData) []error {
	if c.config.SegmentsPerFile == 0 {
		return nil
	}

	results := make([]fileResult, len(nzbData.Files))

	var (
		mu  sync.Mutex
		wg  sync.WaitGroup
		sem = make(chan struct{}, c.config.MaxParallel)
	)

	for fileIndex := range nzbData.Files {
		file := &nzbData.Files[fileIndex]
		for _, segmentIndex := range sampleIndices(len(file.Segments), c.config.SegmentsPerFile) {
			id := file.Segments[segmentIndex].ID

			wg.Add(1)
			sem <- struct{}{}
			go func() {
				defer wg.Done()
				defer func() { <-sem }()

				exists, err := c.exists(id)

				mu.Lock()
				defer mu.Unlock()
				result := &results[fileIndex]
				result.checked++
				switch {
				case err != nil:
					if result.err == nil {
						result.err = err
					}
				case !exists:
					result.missing++
				}
			}()
		}
	}
	wg.Wait()

	var errs []error
	for fileIndex, result := range results {
		var err error
		switch {
		case result.err != nil:
			err = result.err
		case result.missing > 0:
			err = fmt.Errorf("%w: %d of %d checked", ErrSegmentsMissing, result.missing, result.checked)
		default:
			continue
		}

		errs = append(errs, &FileHealthError{
			Path: nzbData.Files[fileIndex].Filename,
			Err:  err,
		})
	}
	return errs
}

// sampleIndices picks count indices spread evenly over [0, length), always
// including the first and last one. A negative count selects everything.
func sampleIndices(length, count int) []int {
	if length == 0 {
		return nil
	}
	if count < 0 || count >= length {
		indices := make([]int, length)
		for i := range indices {
			indices[i] = i
		}
		return indices
	}
	if count == 1 {
		return []int{0}
	}

	indices := make([]int, count)
	for i := range indices {
		indices[i] = i * (length - 1) / (count - 1)
	}
	return indices
}
