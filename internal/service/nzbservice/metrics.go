package nzbservice

import (
	"context"
	"log/slog"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// The instruments of this package. OTel hands back a working no-op instrument
// along with any error, and the only error a fixed name and unit can produce is
// a malformed one, so the error is dropped.
var (
	meter = otel.Meter("internal/service/nzbservice")

	addDuration, _ = meter.Float64Histogram("nzbservice.add.duration",
		metric.WithDescription("Time an add took, from accepted to presented"),
		metric.WithUnit("s"))
	queueDepth, _ = meter.Int64ObservableGauge("nzbservice.queue.depth",
		metric.WithDescription("Unfinished adds by how far they have got; the queued ones are the only ones waiting, the rest are being worked on"))
	historyDepth, _ = meter.Int64ObservableGauge("nzbservice.history.depth",
		metric.WithDescription("Finished adds the service still holds a record of"))
	buildsRunning, _ = meter.Int64ObservableGauge("nzbservice.builds.running",
		metric.WithDescription("Adds building a tree right now; the rest of the queue is waiting for one of their slots"))
	buildsLimit, _ = meter.Int64ObservableGauge("nzbservice.builds.limit",
		metric.WithDescription("Adds allowed to build at once; a queue deep while the running ones sit at this is the one raising it shortens, and 0 is no limit"))
)

var outcomeKey = attribute.Key("outcome")

// stageKey splits the unfinished adds by the stage they sit in, which is what
// separates one waiting for a build slot from one already building. The stages
// are a fixed set and the finished ones are counted apart, so it stays a few
// series.
var stageKey = attribute.Key("stage")

func recordAdd(started time.Time, err error) {
	outcome := "ok"
	if err != nil {
		outcome = "error"
	}

	addDuration.Record(context.Background(), time.Since(started).Seconds(),
		metric.WithAttributes(outcomeKey.String(outcome)))
}

// observeQueue reports the depths at collection time. The queue is state the
// service keeps anyway, so nothing counts it as it changes.
func (s *Service) observeQueue() {
	_, err := meter.RegisterCallback(func(_ context.Context, observer metric.Observer) error {
		s.queueMutex.Lock()
		defer s.queueMutex.Unlock()

		var done int64
		// every unfinished stage is reported, a zero included, so a stage
		// emptying reads as none left rather than as a series that stopped
		unfinished := map[Stage]int64{
			StageQueued: 0, StageChecking: 0, StageBuilding: 0, StageCancelling: 0,
		}
		for _, item := range s.queue {
			if item.Done() {
				done++
			} else {
				unfinished[item.Stage]++
			}
		}

		running, limit := s.slots.state()

		for stage, depth := range unfinished {
			observer.ObserveInt64(queueDepth, depth, metric.WithAttributes(stageKey.String(string(stage))))
		}
		observer.ObserveInt64(historyDepth, done)
		observer.ObserveInt64(buildsRunning, int64(running))
		observer.ObserveInt64(buildsLimit, int64(limit))
		return nil
	}, queueDepth, historyDepth, buildsRunning, buildsLimit)
	if err != nil {
		slog.Error("Failed registering queue metrics", "error", err)
	}
}
