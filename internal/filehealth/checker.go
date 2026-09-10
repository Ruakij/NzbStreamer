// Package filehealth probes a sample of a group's segments on the server and
// reports the groups that lost a segment.
package filehealth

import (
	"context"
	"errors"
	"math"
	"sync"
	"time"

	"git.ruekov.eu/ruakij/nzbStreamer/pkg/filenameops"
	"git.ruekov.eu/ruakij/nzbStreamer/pkg/nzbparser"
)

var ErrSegmentsMissing = errors.New("segments missing on server")

// SegmentExistsFunc reports whether a segment is retrievable from the server.
type SegmentExistsFunc func(id string) (bool, error)

type CheckerConfig struct {
	// AddConfidence is the fraction of a group's segments probed at add time.
	// With the tolerance at zero it is also the covered fraction: being c sure
	// that nothing is missing means probing c of the segments. 0 disables
	// checking.
	AddConfidence float64
	// Maximum concurrent segment-checks.
	MaxParallel int

	// Reserved for a nonzero tolerance (par2 repair), and unused while the
	// tolerance is zero:
	MaxMissingPercent float64
	Par2Safety        float64
	UndecidedAccept   bool
}

// Ensure DefaultChecker implements Checker interface
var _ Checker = (*DefaultChecker)(nil)

// DefaultChecker verifies that a groups segments are still present on the server.
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

// group is what a verdict attaches to: an archive's volumes together, and every
// other content file on its own.
type group struct {
	Name     string // grouped filename, as filenameops.GroupPartFilenames names it
	Files    []*nzbparser.File
	Segments int // across all files, the population N
}

// groups splits the nzb's content files into their units. Volume sets are
// grouped with the same function the build uses, so the check and the tree
// cannot disagree about what a unit is. The nzb's own file order is the walk
// order, so the groups come back in a stable order.
func groups(nzbData *nzbparser.NzbData) []group {
	content := contentFiles(nzbData)

	grouped := filenameops.GroupPartFilenames(groupedNames(content))
	filenameops.SortGroupedFilenames(grouped)

	// Map each filename back to its group name, so the nzb's file order can
	// supply the group order and a stable first-encounter grouping.
	fileToGroup := make(map[string]string, len(content))
	for name, files := range grouped {
		for _, f := range files {
			fileToGroup[f] = name
		}
	}
	byFile := make(map[string]*nzbparser.File, len(content))
	for _, file := range content {
		byFile[file.Filename] = file
	}

	// Walk content in the nzb's own order, creating a group on first encounter.
	order := make([]string, 0, len(grouped))
	seen := make(map[string]bool, len(grouped))
	for _, file := range content {
		name := fileToGroup[file.Filename]
		if !seen[name] {
			seen[name] = true
			order = append(order, name)
		}
	}

	result := make([]group, 0, len(order))
	for _, name := range order {
		g := group{Name: name}
		for _, filename := range grouped[name] {
			file := byFile[filename]
			g.Files = append(g.Files, file)
			g.Segments += len(file.Segments)
		}
		result = append(result, g)
	}
	return result
}

// groupedNames returns the filenames of content as a fresh slice, which
// GroupPartFilenames consumes.
func groupedNames(content []*nzbparser.File) []string {
	names := make([]string, len(content))
	for i, file := range content {
		names[i] = file.Filename
	}
	return names
}

// probeOrder is the order the segments of a group are probed in, such that any
// prefix of it is spread evenly over the whole: 0, N/2, N/4, 3N/4, ...
// Bit-reversal gives that for free, so stopping anywhere leaves a sample with no
// gap larger than twice the smallest, and resuming is an index into it.
// length<=1 returns {0} or the single index; length==0 returns nil.
func probeOrder(length int) []int {
	if length <= 1 {
		if length == 0 {
			return nil
		}
		return []int{0}
	}

	// smallest power of two >= length
	size := 1
	for size < length {
		size <<= 1
	}

	order := make([]int, 0, length)
	for i := 0; i < size; i++ {
		// bit-reverse i over log2(size) bits
		rev := 0
		for x, b := i, size>>1; b > 0; b >>= 1 {
			rev = rev<<1 | x&1
			x >>= 1
		}
		if rev < length {
			order = append(order, rev)
		}
	}
	return order
}

// progressReporter counts the probes of one group against the probes that group
// planned, and hands both to whoever asked. A nil report is the caller that
// does not want to know.
type progressReporter struct {
	report ProgressFunc

	mu    sync.Mutex
	done  int
	total int
}

func (p *progressReporter) plan(segments int) {
	p.update(0, segments)
}

func (p *progressReporter) step() {
	p.update(1, 0)
}

func (p *progressReporter) update(done, total int) {
	if p.report == nil {
		return
	}

	// Reported under the lock, so what the caller sees only ever moves forward
	p.mu.Lock()
	defer p.mu.Unlock()

	p.done += done
	p.total += total
	p.report(p.done, p.total)
}

// CheckFiles scans every content group to the add-time confidence and returns
// the groups that lost a segment.
func (c *DefaultChecker) CheckFiles(ctx context.Context, nzbData *nzbparser.NzbData, progress ProgressFunc) []FailedGroup {
	if c.config.AddConfidence <= 0 {
		return nil
	}
	started := time.Now()
	var (
		failed        []FailedGroup
		totalSegments int
	)
	for _, g := range groups(nzbData) {
		totalSegments += g.Segments
		target := int(math.Round(c.config.AddConfidence * float64(g.Segments)))
		if target < 1 {
			target = 1
		}
		if target > g.Segments {
			target = g.Segments
		}
		if _, err := c.scan(ctx, g, nil, target, progress); err != nil {
			files := make([]string, len(g.Files))
			for i, f := range g.Files {
				files[i] = f.Filename
			}
			failed = append(failed, FailedGroup{Name: g.Name, Files: files})
		}
	}
	recordCheck(ctx, started, totalSegments)
	return failed
}

// PlannedProbes is the work a check of this nzb would be, in segments it would
// ask the server about, worked out without asking about any of them.
func (c *DefaultChecker) PlannedProbes(nzbData *nzbparser.NzbData) int {
	if c.config.AddConfidence <= 0 {
		return 0
	}
	planned := 0
	for _, g := range groups(nzbData) {
		target := int(math.Round(c.config.AddConfidence * float64(g.Segments)))
		if target < 1 {
			target = 1
		}
		if target > g.Segments {
			target = g.Segments
		}
		planned += target
	}
	return planned
}

// scan probes the order over g's segments until target of them count as present
// or one is missing. known holds the positions that already count and are
// skipped. It returns how many count as present and stops early on a missing
// segment, because at zero tolerance there is nothing further to learn.
func (c *DefaultChecker) scan(ctx context.Context, g group, known []int, target int, progress ProgressFunc) (covered int, err error) {
	if g.Segments == 0 || target <= 0 {
		return 0, nil
	}
	if target > g.Segments {
		target = g.Segments
	}

	order := probeOrder(g.Segments)

	knownSet := make(map[int]bool, len(known))
	for _, pos := range known {
		knownSet[pos] = true
	}

	// Positions known to be present already count.
	for _, pos := range known {
		if pos >= 0 && pos < g.Segments {
			covered++
		}
	}

	reporter := &progressReporter{report: progress}
	reporter.plan(target)

	// Flatten the files' segments into one index space, so the order walks the
	// group end to end. Positions below the known coverage are skipped.
	var (
		mu      sync.Mutex
		wg      sync.WaitGroup
		sem     = make(chan struct{}, c.config.MaxParallel)
		missing bool
	)
	gctx, cancel := context.WithCancel(ctx)
	defer cancel()

	for i := 0; i < target && i < len(order); i++ {
		if knownSet[order[i]] {
			continue
		}

		id := segmentAt(g, order[i])
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()

			if gctx.Err() != nil {
				return
			}

			exists, err := c.exists(id)

			reporter.step()

			mu.Lock()
			defer mu.Unlock()
			switch {
			case err != nil:
				recordProbe(gctx, "error")
				missing = true
			case !exists:
				recordProbe(gctx, "missing")
				missing = true
				cancel()
			default:
				recordProbe(gctx, "present")
				covered++
			}
		}()
	}
	wg.Wait()

	if missing {
		return covered, ErrSegmentsMissing
	}
	return covered, nil
}

// segmentAt resolves a group-index position to the message-id of the segment
// that holds it.
func segmentAt(g group, pos int) string {
	for _, file := range g.Files {
		if pos < len(file.Segments) {
			return file.Segments[pos].ID
		}
		pos -= len(file.Segments)
	}
	return ""
}

// contentFiles picks the files whose loss would make the release unusable. They
// are the only ones grouped and probed: a missing par2 or nfo costs nothing
// being measured here.
func contentFiles(nzbData *nzbparser.NzbData) []*nzbparser.File {
	var files []*nzbparser.File
	for i := range nzbData.Files {
		if filenameops.Classify(nzbData.Files[i].Filename) == filenameops.ClassContent && len(nzbData.Files[i].Segments) > 0 {
			files = append(files, &nzbData.Files[i])
		}
	}
	return files
}
