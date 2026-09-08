package nzbservice

// An add is measured in the operations it asks the news servers for, since that
// is the resource every add is queueing for and the only one that is scarce.
// A check is STATs, one per probed segment; a build is the volume opens an
// archive header walk costs, one per volume. Both are counted off the nzb before
// the add runs and reported against as it runs. Turning what is left into a time
// is one division by the rate the pool is answering at, which every add on this
// installation shares.

// remainingOps is the work the item has left and what the whole add looks like
// it costs, both in server operations. Waiting for a slot is not in it: that is
// not this items work, and only the queue knows how long it lasts.
//
// A stage that has reported more work than was planned for it is believed: a
// check escalates files it could not decide, and a build walks archives nested
// in the ones the nzb names.
func remainingOps(item *QueueItem) (left, total float64) {
	total = float64(item.probeOps + item.buildOps)

	switch {
	case item.Done() || item.Stage == StageCancelling:
		return 0, total
	case item.Stage == StageQueued:
		return total, total

	case item.Stage == StageChecking:
		probes := max(item.stageTotal, item.probeOps)
		total = float64(probes + item.buildOps)
		return float64(probes - item.stageDone + item.buildOps), total

	default:
		// The check is over, so what it cost is done whatever it turned out to be
		volumes := max(item.stageTotal, item.buildOps)
		total = float64(item.probeOps + volumes)
		return float64(volumes - item.stageDone), total
	}
}

// progressOf is what the add has done against what it looks like it will cost.
// It moves backwards where the work grows - a check that escalates a file it
// could not decide has more segments to probe than it started with, and saying
// so is more use than a bar that only ever climbs.
func progressOf(item *QueueItem, left, total float64) float64 {
	switch {
	case item.Stage == StageCompleted:
		return 1
	case item.Done() || item.Stage == StageCancelling:
		return 0
	case total > 0:
		return min(max(1-left/total, 0), 1)
	default:
		return 0
	}
}
