package nntpclient

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
	meter = otel.Meter("internal/nntpclient")

	fetchDuration, _ = meter.Float64Histogram("nntp.fetch.duration",
		metric.WithDescription("Time one server took to answer an article request"),
		metric.WithUnit("s"))
	fetchedArticles, _ = meter.Int64Counter("nntp.fetch.articles",
		metric.WithDescription("Article requests by the server that answered and how it did"))
	fetchedBytes, _ = meter.Int64Counter("nntp.fetch.bytes",
		metric.WithDescription("Decoded article bytes per server"),
		metric.WithUnit("By"))
	connectionWait, _ = meter.Float64Histogram("nntp.connection.wait",
		metric.WithDescription("Time a request waited for a connection to carry it"),
		metric.WithUnit("s"))
	breakerTrips, _ = meter.Int64Counter("nntp.breaker.trips",
		metric.WithDescription("Times a server was taken out of rotation"))
)

// serverKey labels a measurement with the server it is about. The configured
// name is the host, which is what an operator recognises.
const serverKey = attribute.Key("server")

// outcome of one request against one server, which is what separates a descent
// past a server that missed from one past a server that failed.
const (
	outcomeOK      = "ok"
	outcomeMissing = "missing"
	outcomeError   = "error"
)

var outcomeKey = attribute.Key("outcome")

// recordWait times what a request spent before it had a connection to speak on,
// which is what a pool too small for the read rate shows up as.
func (c *Client) recordWait(started time.Time) {
	connectionWait.Record(context.Background(), time.Since(started).Seconds(),
		metric.WithAttributes(serverKey.String(c.config.Host)))
}
