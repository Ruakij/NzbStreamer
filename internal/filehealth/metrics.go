package filehealth

import (
	"context"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// The instruments of this package. OTel hands back a working no-op instrument
// along with any error, and the only error a fixed name and unit can produce is
// a malformed one, so the error is dropped.
var (
	meter = otel.Meter("internal/filehealth")

	probedSegments, _ = meter.Int64Counter("filehealth.segments.probed",
		metric.WithDescription("Segments a health check asked the server about, by what it found"))
	sampledSegments, _ = meter.Int64Counter("filehealth.segments.sampled",
		metric.WithDescription("Segments the checked files hold, which the probed ones are a sample of"))
	checkDuration, _ = meter.Float64Histogram("filehealth.check.duration",
		metric.WithDescription("Time a whole nzbs health check took"),
		metric.WithUnit("s"))
)

var resultKey = attribute.Key("result")

// recordProbe counts one probed segment. The check is parallel, so this is called
// from every probe goroutine.
func recordProbe(ctx context.Context, result string) {
	probedSegments.Add(ctx, 1, metric.WithAttributes(resultKey.String(result)))
}

func recordCheck(ctx context.Context, started time.Time, segments int) {
	checkDuration.Record(ctx, time.Since(started).Seconds())
	sampledSegments.Add(ctx, int64(segments))
}
