package nzbservice

import (
	"testing"
	"time"
)

func TestRemainingOpsCoversTheStagesAnItemHasNotReached(t *testing.T) {
	queued := &QueueItem{Stage: StageQueued, probeOps: 100, buildOps: 40}
	if left, total := remainingOps(queued, 10); left != 140 || total != 140 {
		t.Errorf("a queued add has %v of %v left, want the check and the build", left, total)
	}

	checking := &QueueItem{
		Stage:      StageChecking,
		probeOps:   100,
		buildOps:   40,
		checkDone:  50,
		checkTotal: 100,
	}
	left, total := remainingOps(checking, 10)
	if left != 90 || total != 140 {
		t.Errorf("a half-done check has %v of %v left, want 90 of 140", left, total)
	}

	// Escalation: the same probes against a check that turned out to be twice
	// the work leaves the add further from done than it looked
	before := progressOf(checking, left, total)
	checking.checkTotal = 200
	after, afterTotal := remainingOps(checking, 10)
	if progressOf(checking, after, afterTotal) >= before {
		t.Errorf("progress did not fall when the check found more work: %v then %v",
			before, progressOf(checking, after, afterTotal))
	}
}

// A build reports nothing about itself, so it is counted down against the rate
// the servers are answering at, and an overrun sits at nothing left
func TestABuildCountsDownAgainstTheRate(t *testing.T) {
	building := &QueueItem{
		Stage:        StageBuilding,
		probeOps:     100,
		buildOps:     40,
		stageStarted: time.Now().Add(-2 * time.Second),
	}
	if left, _ := remainingOps(building, 10); left < 19 || left > 21 {
		t.Errorf("2s of building at 10 ops/s has %v left, want about 20", left)
	}

	building.stageStarted = time.Now().Add(-time.Minute)
	if left, _ := remainingOps(building, 10); left != 0 {
		t.Errorf("a build past its estimate has %v left, want 0", left)
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
