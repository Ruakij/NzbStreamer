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
		metric.WithDescription("Adds in flight"))
	historyDepth, _ = meter.Int64ObservableGauge("nzbservice.history.depth",
		metric.WithDescription("Finished adds the service still holds a record of"))
)

var outcomeKey = attribute.Key("outcome")

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

		var queued, done int64
		for _, item := range s.queue {
			if item.Done() {
				done++
			} else {
				queued++
			}
		}

		observer.ObserveInt64(queueDepth, queued)
		observer.ObserveInt64(historyDepth, done)
		return nil
	}, queueDepth, historyDepth)
	if err != nil {
		slog.Error("Failed registering queue metrics", "error", err)
	}
}
