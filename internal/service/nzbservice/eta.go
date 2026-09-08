package nzbservice

import "time"

// An add is measured in the operations it asks the news servers for, since that
// is the resource every add is queueing for and the only one that is scarce.
// A check is STATs, one per probed segment; a build is the article reads an
// archive header walk costs. Turning those into a time is one division by the
// rate the pool is answering at, which every add on this installation shares.
//
// headerWalkOps is what opening one archive costs before it has been opened.
// A plain rar set is its volume headers and little else; a solid or encrypted
// one, or one holding another archive, is far more, and nothing sees that
// coming. What it does instead is overrun the estimate, which building answers
// for by counting down against the clock.
//
// ponytail: one constant for every archive, measure per-archive if the
// estimates for nested sets turn out to matter.
const headerWalkOps = 17

// remainingOps is the work the item has left and what the whole add looks like
// it costs, both in server operations. Waiting for a slot is not in it: that is
// not this items work, and only the queue knows how long it lasts.
func remainingOps(item *QueueItem, rate float64) (left, total float64) {
	total = float64(item.probeOps + item.buildOps)

	switch {
	case item.Done() || item.Stage == StageCancelling:
		return 0, total
	case item.Stage == StageQueued:
		return total, total

	case item.Stage == StageChecking:
		// What the check has reported of itself, and the plan until it has
		// reported anything. It only ever reports more, since every file it
		// escalates is probes the plan did not have, and the add grows with it
		probes := max(item.checkTotal, item.probeOps)
		total = float64(probes + item.buildOps)
		return float64(probes - item.checkDone + item.buildOps), total

	default:
		// A build reports nothing about itself, so what is left of it is counted
		// down against the rate the servers are answering at. An overrun sits at 0,
		// which reads as an estimate that ran out rather than a build that stalled
		done := 0.0
		if !item.stageStarted.IsZero() {
			done = time.Since(item.stageStarted).Seconds() * rate
		}
		return max(float64(item.buildOps)-done, 0), total
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
