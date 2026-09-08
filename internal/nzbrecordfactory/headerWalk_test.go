package nzbrecordfactory

import "testing"

// A walk that opens more volumes than were planned - a set nested in another, a
// volume rardecode goes back to - reports more work rather than a fraction past
// one
func TestBuildProgressGrowsPastThePlan(t *testing.T) {
	var done, total int
	report := &buildProgress{report: func(d, tot int) { done, total = d, tot }}

	report.plan(2)
	report.step()
	if done != 1 || total != 2 {
		t.Errorf("one of two volumes walked reported %d of %d", done, total)
	}

	report.step()
	report.step()
	if done != 3 || total != 3 {
		t.Errorf("a third volume of a two-volume plan reported %d of %d", done, total)
	}
}

func TestArchiveVolumesCountsEveryVolumeOfEveryArchive(t *testing.T) {
	filenames := []string{
		"release.part1.rar", "release.part2.rar", "release.part3.rar",
		"release.nfo", "release.par2",
	}
	if volumes := ArchiveVolumes(filenames); volumes != 3 {
		t.Errorf("counted %d volumes, want the three rar parts", volumes)
	}
}
