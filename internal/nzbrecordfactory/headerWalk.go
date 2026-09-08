package nzbrecordfactory

import (
	"io"
	"path"
	"sync"

	"git.ruekov.eu/ruakij/nzbStreamer/pkg/filenameops"
	"git.ruekov.eu/ruakij/nzbStreamer/pkg/resource"
)

// ProgressFunc reports the volumes a build has read against the ones it means
// to read.
type ProgressFunc func(done, total int)

// buildProgress counts the volumes every header walk of one build opens against
// the ones those walks planned. A walk of a set nested in another is volumes the
// plan did not have, and so is a volume rardecode goes back to, so the total
// grows rather than the fraction passing one. A nil report is the caller that
// does not want to know.
type buildProgress struct {
	report ProgressFunc

	mu    sync.Mutex
	done  int
	total int
}

func (p *buildProgress) plan(volumes int) {
	p.update(0, volumes)
}

func (p *buildProgress) step() {
	p.update(1, 0)
}

func (p *buildProgress) update(done, total int) {
	if p == nil || p.report == nil {
		return
	}

	// Reported under the lock, so what the caller sees only ever moves forward
	p.mu.Lock()
	defer p.mu.Unlock()

	p.done += done
	p.total = max(p.total+total, p.done)
	p.report(p.done, p.total)
}

// headerVolumes is what an archive is listed through: the volumes, and one step
// of progress per volume opened. A walk opens each of them once, which is what
// makes the count the progress of the build.
func headerVolumes(volumes []resource.ReadSeekCloseableResource, report *buildProgress) []resource.ReadSeekCloseableResource {
	report.plan(len(volumes))

	walked := make([]resource.ReadSeekCloseableResource, len(volumes))
	for i, volume := range volumes {
		walked[i] = &headerVolume{ReadSeekCloseableResource: volume, report: report}
	}
	return walked
}

type headerVolume struct {
	resource.ReadSeekCloseableResource
	report *buildProgress
}

func (v *headerVolume) Open() (io.ReadSeekCloser, error) {
	v.report.step()
	return v.ReadSeekCloseableResource.Open()
}

// ArchiveVolumes counts the volumes the archives among these filenames hold. It
// is what a build costs before it has run: a header walk opens every volume of
// every archive once, and everything else is presented as it is. Archives nested
// in these are not in it, since nothing knows they are there until their parent
// is open.
func ArchiveVolumes(filenames []string) int {
	volumes := 0
	for groupFilename, groupFilenames := range filenameops.GroupPartFilenames(filenames) {
		if isArchive(path.Ext(groupFilename)) {
			volumes += len(groupFilenames)
		}
	}
	return volumes
}
