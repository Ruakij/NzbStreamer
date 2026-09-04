// Package filehealth probes a sample of a file's segments on the server and
// reports the files that look too incomplete to serve.
package filehealth

import (
	"errors"
	"fmt"
	"log/slog"
	"math"
	"sync"

	"git.ruekov.eu/ruakij/nzbStreamer/pkg/filenameops"
	"git.ruekov.eu/ruakij/nzbStreamer/pkg/nzbparser"
)

var ErrSegmentsMissing = errors.New("segments missing on server")

// SegmentExistsFunc reports whether a segment is retrievable from the server.
type SegmentExistsFunc func(id string) (bool, error)

type CheckerConfig struct {
	// Sample size of the first pass, as a percentage of a content files
	// segments; 0 disables checking entirely
	InitialFilePercent float64
	// Floor and cap on that sample, so a short file is not rounded to nothing
	// and a huge one does not turn the add into a download
	InitialFileMinSegments int
	InitialFileMaxSegments int

	// Ceiling on the widened sample a file gets when the first pass cannot
	// decide it, as a percentage of its segments; 0 skips the second pass
	ExtensiveFilePercent     float64
	ExtensiveFileMaxSegments int

	// Ceiling on accepted damage regardless of what par2 could repair
	MaxMissingPercent float64
	// Fraction of the estimated par2 capacity to trust, since it is estimated
	Par2Safety float64
	// Whether a file the second pass still cannot decide is accepted
	UndecidedAccept bool
	// Confidence of the interval the verdict is taken from
	Confidence float64

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

// CheckFiles samples the content files of an nzb and reports the ones whose
// damage is worse than its par2 could repair.
//
// A cheap pass covers every content file, which settles a dead post on its first
// probe and a healthy one for the price of that pass. Only a file whose sample
// leaves the answer genuinely open is probed again, harder.
func (c *DefaultChecker) CheckFiles(nzbData *nzbparser.NzbData) []error {
	if c.config.InitialFilePercent <= 0 {
		return nil
	}

	content := contentFiles(nzbData)
	if len(content) == 0 {
		return nil
	}

	limit := c.limit(nzbData)
	counts := make([]int, len(content))
	for i, file := range content {
		counts[i] = clamp(
			int(math.Round(float64(len(file.Segments))*c.config.InitialFilePercent/100)),
			c.config.InitialFileMinSegments,
			min(c.config.InitialFileMaxSegments, len(file.Segments)),
		)
	}

	results := c.probe(content, counts)
	c.escalate(content, results, limit)

	var errs []error
	for i, result := range results {
		var err error
		switch {
		case result.err != nil:
			err = result.err
		case decide(result.missing, result.checked, limit, c.config.Confidence) == verdictDiscard:
			err = fmt.Errorf("%w: %d of %d checked", ErrSegmentsMissing, result.missing, result.checked)
		case result.missing > 0:
			slog.Warn("File has missing segments, but within what par2 could repair",
				"file", content[i].Filename,
				"missing", result.missing,
				"checked", result.checked,
				"limit", limit)
			continue
		default:
			continue
		}

		errs = append(errs, &FileHealthError{Path: content[i].Filename, Err: err})
	}
	return errs
}

// escalate re-probes, with a sample wide enough to resolve it, every file the
// first pass could not decide, and replaces its result. A widened sample is read
// on its own rather than added to the first: what it measures is the same
// fraction, only more precisely.
func (c *DefaultChecker) escalate(content []*nzbparser.File, results []fileResult, limit float64) {
	if c.config.ExtensiveFilePercent <= 0 {
		return
	}

	var (
		files  []*nzbparser.File
		counts []int
		at     []int
	)
	for i, result := range results {
		if result.err != nil || decide(result.missing, result.checked, limit, c.config.Confidence) != verdictUndecided {
			continue
		}

		segments := len(content[i].Segments)
		count := clamp(
			requiredSamples(result.missing, result.checked, limit, c.config.Confidence),
			result.checked,
			min(int(math.Round(float64(segments)*c.config.ExtensiveFilePercent/100)), c.config.ExtensiveFileMaxSegments, segments),
		)
		if count <= result.checked {
			continue
		}

		files = append(files, content[i])
		counts = append(counts, count)
		at = append(at, i)
	}
	if len(files) == 0 {
		return
	}

	for i, result := range c.probe(files, counts) {
		if !c.config.UndecidedAccept && decide(result.missing, result.checked, limit, c.config.Confidence) == verdictUndecided {
			result.err = fmt.Errorf("%w: %d of %d checked, still undecided", ErrSegmentsMissing, result.missing, result.checked)
		}
		results[at[i]] = result
	}
}

// probe checks counts[i] segments of files[i], spread evenly.
func (c *DefaultChecker) probe(files []*nzbparser.File, counts []int) []fileResult {
	results := make([]fileResult, len(files))

	var (
		mu  sync.Mutex
		wg  sync.WaitGroup
		sem = make(chan struct{}, c.config.MaxParallel)
	)

	for fileIndex, file := range files {
		for _, segmentIndex := range sampleIndices(len(file.Segments), counts[fileIndex]) {
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

	return results
}

// limit is the missing fraction a file may still be accepted with: what the
// nzbs par2 could repair, under the configured ceiling. Without par2 it is zero,
// and a single missing segment is a failure - which is the right answer, since
// nothing will make that file whole.
func (c *DefaultChecker) limit(nzbData *nzbparser.NzbData) float64 {
	return math.Min(c.config.MaxMissingPercent/100, par2Capacity(nzbData)*c.config.Par2Safety)
}

// par2Capacity estimates the fraction of the content an nzbs recovery files
// could rebuild, as the ratio of the bytes each carries. Recovery blocks rebuild
// an equal number of lost source blocks, so the byte ratio approximates the
// block ratio without reading anything; the index packet holds the real counts
// and costs a fetch.
func par2Capacity(nzbData *nzbparser.NzbData) float64 {
	var recovery, content int64
	for i := range nzbData.Files {
		file := &nzbData.Files[i]

		var bytes int64
		for _, segment := range file.Segments {
			bytes += int64(segment.BytesHint)
		}

		switch filenameops.Classify(file.Filename) {
		case filenameops.ClassRecovery:
			recovery += bytes
		case filenameops.ClassContent:
			content += bytes
		case filenameops.ClassOther:
		}
	}

	if content == 0 {
		return 0
	}
	return float64(recovery) / float64(content)
}

// contentFiles picks the files whose loss would make the release unusable. They
// are the only ones probed: a missing par2 or nfo costs nothing being measured
// here.
func contentFiles(nzbData *nzbparser.NzbData) []*nzbparser.File {
	var files []*nzbparser.File
	for i := range nzbData.Files {
		if filenameops.Classify(nzbData.Files[i].Filename) == filenameops.ClassContent && len(nzbData.Files[i].Segments) > 0 {
			files = append(files, &nzbData.Files[i])
		}
	}
	return files
}

func clamp(value, low, high int) int {
	return min(max(value, low), max(high, 0))
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
