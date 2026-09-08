package nzbservice

import "testing"

func TestRemainingOpsCoversTheStagesAnItemHasNotReached(t *testing.T) {
	queued := &QueueItem{Stage: StageQueued, probeOps: 100, buildOps: 40}
	if left, total := remainingOps(queued); left != 140 || total != 140 {
		t.Errorf("a queued add has %v of %v left, want the check and the build", left, total)
	}

	checking := &QueueItem{
		Stage:      StageChecking,
		probeOps:   100,
		buildOps:   40,
		stageDone:  50,
		stageTotal: 100,
	}
	left, total := remainingOps(checking)
	if left != 90 || total != 140 {
		t.Errorf("a half-done check has %v of %v left, want 90 of 140", left, total)
	}

	// Escalation: the same probes against a check that turned out to be twice
	// the work leaves the add further from done than it looked
	before := progressOf(checking, left, total)
	checking.stageTotal = 200
	after, afterTotal := remainingOps(checking)
	if progressOf(checking, after, afterTotal) >= before {
		t.Errorf("progress did not fall when the check found more work: %v then %v",
			before, progressOf(checking, after, afterTotal))
	}
}

// A build reports the volumes it has opened, and one that finds an archive
// nested in the set grows past the plan rather than reporting itself done
func TestABuildCountsTheVolumesItHasOpened(t *testing.T) {
	building := &QueueItem{
		Stage:     StageBuilding,
		probeOps:  100,
		buildOps:  40,
		stageDone: 10,
	}
	if left, total := remainingOps(building); left != 30 || total != 140 {
		t.Errorf("10 of 40 volumes walked has %v of %v left, want 30 of 140", left, total)
	}

	building.stageDone, building.stageTotal = 40, 60
	left, total := remainingOps(building)
	if left != 20 || total != 160 {
		t.Errorf("a build that found more volumes has %v of %v left, want 20 of 160", left, total)
	}
}

func TestAQueuedAddWaitsForTheOnesAheadOfIt(t *testing.T) {
	service := &Service{rate: func() float64 { return 10 }}
	service.slots.setLimit(1)

	service.queue = []*QueueItem{
		{ID: "first", Stage: StageQueued, probeOps: 100},
		{ID: "second", Stage: StageQueued, probeOps: 100},
	}

	items := service.items(false)
	if len(items) != 2 {
		t.Fatalf("got %d items, want 2", len(items))
	}
	if items[0].Eta != 10 {
		t.Errorf("the first add reported %vs, want its own work", items[0].Eta)
	}
	if items[1].Eta != 20 {
		t.Errorf("the second add reported %vs, want its own work behind the first", items[1].Eta)
	}
}

// Nothing is known about how fast the servers answer until they have answered
// something, and an add that cannot be estimated says so with a zero
func TestWithoutARateThereIsNoEta(t *testing.T) {
	service := &Service{}
	service.queue = []*QueueItem{{ID: "first", Stage: StageQueued, probeOps: 100}}

	if eta := service.items(false)[0].Eta; eta != 0 {
		t.Errorf("estimated %vs without a measured rate, want 0", eta)
	}
}
