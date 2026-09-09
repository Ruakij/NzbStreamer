package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/exporters/prometheus"
	"go.opentelemetry.io/otel/metric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	semconv "go.opentelemetry.io/otel/semconv/v1.38.0"

	"git.ruekov.eu/ruakij/nzbStreamer/internal/presentation/fusemount"
	"git.ruekov.eu/ruakij/nzbStreamer/internal/presentation/webdav"
	"git.ruekov.eu/ruakij/nzbStreamer/pkg/diskcache"
	"git.ruekov.eu/ruakij/nzbStreamer/pkg/resource/readaheadresource"
)

// interfaceKey names which way in a file was read, which health.go calls the
// interfaces too.
var interfaceKey = attribute.Key("interface")

// windowKey names how far back a measurement looked, one value per configured
// window.
var windowKey = attribute.Key("window")

// windowLabel writes a duration the way it is configured rather than the way Go
// prints it, since "168h0m0s" on a legend is noise around the one part that says
// anything.
func windowLabel(window time.Duration) string {
	switch {
	case window%(24*time.Hour) == 0:
		return strconv.FormatInt(int64(window/(24*time.Hour)), 10) + "d"
	case window%time.Hour == 0:
		return strconv.FormatInt(int64(window/time.Hour), 10) + "h"
	default:
		return window.String()
	}
}

// durationViews gives the histograms that record seconds a bucket ladder in
// seconds. The sdk's default ladder runs 0 to 10000 and is meant for
// milliseconds, under which every measurement here falls into the first bucket
// and no quantile can be read back out of it.
func durationViews() []sdkmetric.View {
	// A round trip to a news server and everything built out of one
	fast := []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30}
	// A whole nzb going through a stage, which is minutes on a bad day
	slow := []float64{0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60, 120, 300, 600}
	boundaries := map[string][]float64{
		"nntp.socket.rtt":              {0.001, 0.0025, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1},
		"nntp.socket.connect.latency":  fast,
		"nntp.response.latency":        fast,
		"nntp.queue.latency":           fast,
		"nntp.fetch.duration":          fast,
		"http.server.request.duration": fast,
		"filehealth.check.duration":    slow,
		"nzbservice.add.duration":      slow,
	}

	views := make([]sdkmetric.View, 0, len(boundaries))
	for name, bounds := range boundaries {
		views = append(views, sdkmetric.NewView(
			sdkmetric.Instrument{Name: name},
			sdkmetric.Stream{Aggregation: sdkmetric.AggregationExplicitBucketHistogram{Boundaries: bounds}},
		))
	}
	return views
}

// setupMetrics installs the meter provider every package records into and
// returns the handler that serves what it holds, plus the flush that gets the
// last collection out on the way down. This is the only place that knows an
// exporter exists; until it runs, the instruments the packages declare are the
// no-op the global provider hands out.
//
// Scraping is always served. A configured collector is a second reader on the
// same provider rather than a replacement, so a push setup can still be curled.
func setupMetrics(ctx context.Context, c MetricsConfig, cache *diskcache.Cache, library *libraryMeter) (http.Handler, func(context.Context) error, error) {
	exporter, err := prometheus.New()
	if err != nil {
		return nil, nil, fmt.Errorf("failed creating the prometheus exporter: %w", err)
	}

	options := []sdkmetric.Option{sdkmetric.WithReader(exporter)}
	for _, view := range durationViews() {
		options = append(options, sdkmetric.WithView(view))
	}
	if c.OTLPEndpoint != "" {
		pusher, err := otlpReader(ctx, c)
		if err != nil {
			return nil, nil, err
		}
		options = append(options,
			sdkmetric.WithReader(pusher),
			sdkmetric.WithResource(resource.NewSchemaless(semconv.ServiceName(c.ServiceName))))
	}

	provider := sdkmetric.NewMeterProvider(options...)
	otel.SetMeterProvider(provider)

	if err := observeCache(cache); err != nil {
		return nil, nil, err
	}
	if err := observeLibrary(library); err != nil {
		return nil, nil, err
	}
	if err := observePresenters(); err != nil {
		return nil, nil, err
	}
	if err := observeReadahead(); err != nil {
		return nil, nil, err
	}

	return promhttp.Handler(), provider.Shutdown, nil
}

// otlpReader builds the reader that pushes to a collector on an interval. The
// endpoint is written as a url so that what is encrypted is read off it rather
// than configured separately: anything but https goes out in the clear, which
// is what a collector on the same host wants.
func otlpReader(ctx context.Context, c MetricsConfig) (sdkmetric.Reader, error) {
	endpoint, err := url.Parse(c.OTLPEndpoint)
	if err != nil || endpoint.Host == "" {
		return nil, fmt.Errorf("%q is not a usable otlp endpoint, expected scheme://host:port", c.OTLPEndpoint)
	}
	insecure := endpoint.Scheme != "https"

	var exporter sdkmetric.Exporter
	switch c.OTLPProtocol {
	case "grpc":
		options := []otlpmetricgrpc.Option{otlpmetricgrpc.WithEndpoint(endpoint.Host)}
		if insecure {
			options = append(options, otlpmetricgrpc.WithInsecure())
		}
		exporter, err = otlpmetricgrpc.New(ctx, options...)
	case "http":
		options := []otlpmetrichttp.Option{otlpmetrichttp.WithEndpoint(endpoint.Host)}
		if endpoint.Path != "" {
			options = append(options, otlpmetrichttp.WithURLPath(endpoint.Path))
		}
		if insecure {
			options = append(options, otlpmetrichttp.WithInsecure())
		}
		exporter, err = otlpmetrichttp.New(ctx, options...)
	default:
		return nil, fmt.Errorf("%q is not an otlp protocol, expected grpc or http", c.OTLPProtocol)
	}
	if err != nil {
		return nil, fmt.Errorf("failed creating the otlp exporter: %w", err)
	}

	return sdkmetric.NewPeriodicReader(exporter, sdkmetric.WithInterval(c.OTLPInterval)), nil
}

// observeLibrary reports what has been added against what of it is read. Both
// are gauges: the active bytes are the distinct segments of a window and do not
// add up over time, and the nominal size falls when an nzb is deleted.
func observeLibrary(library *libraryMeter) error {
	meter := otel.Meter("cmd/nzbstreamer")

	var errs []error
	gauge := func(name string, opts ...metric.Int64ObservableGaugeOption) metric.Int64ObservableGauge {
		instrument, err := meter.Int64ObservableGauge(name, opts...)
		errs = append(errs, err)
		return instrument
	}
	counter := func(name string, opts ...metric.Int64ObservableCounterOption) metric.Int64ObservableCounter {
		instrument, err := meter.Int64ObservableCounter(name, opts...)
		errs = append(errs, err)
		return instrument
	}

	nzbs := gauge("library.nzbs", metric.WithDescription("Nzbs presented"))
	bytes := gauge("library.bytes",
		metric.WithDescription("Bytes the presented nzbs describe, cached or not"),
		metric.WithUnit("By"))
	maxBytes := gauge("library.bytes.limit",
		metric.WithDescription("Bytes the library may describe before adds are refused; 0 is unlimited"),
		metric.WithUnit("By"))
	// One series per configured window, which is the data that was in
	// circulation over each of them: what a day needs against what a month does
	// is what a larger cache would buy
	activeBytes := gauge("library.active.bytes",
		metric.WithDescription("Bytes of the distinct segments read within the window, which is what the cache would have to hold to serve them without a refetch"),
		metric.WithUnit("By"))
	activeSegments := gauge("library.active.segments",
		metric.WithDescription("Distinct segments read within the window, which against the active bytes is the mean size of what is being read"))
	// Counters rather than a window like the active library: a refetch is an
	// event with a size, so the window belongs to whatever reads them, where a
	// working set is a set and cannot be summed back out of per-scrape values
	refetchedBytes := counter("cache.refetched.bytes",
		metric.WithDescription("Bytes that had to be downloaded a second time since the process started, which a cache with room for the whole working set would have served instead; a restart resets it to 0, which is what makes rate and increase read it correctly"),
		metric.WithUnit("By"))
	refetchedSegments := counter("cache.refetched.segments",
		metric.WithDescription("Segments among them, which against the refetched bytes says whether the evictions hit the large segments or all of them alike"))

	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("failed creating the library instruments: %w", err)
	}

	_, err := meter.RegisterCallback(func(_ context.Context, observer metric.Observer) error {
		stats := library.read()

		observer.ObserveInt64(nzbs, int64(stats.Nzbs))
		observer.ObserveInt64(bytes, stats.Bytes)
		observer.ObserveInt64(maxBytes, stats.MaxBytes)
		for _, active := range stats.Active {
			window := metric.WithAttributes(windowKey.String(windowLabel(active.Window)))
			observer.ObserveInt64(activeBytes, active.WorkingSetBytes, window)
			observer.ObserveInt64(activeSegments, active.WorkingSetSegments, window)
		}
		observer.ObserveInt64(refetchedBytes, stats.RefetchedBytes)
		observer.ObserveInt64(refetchedSegments, stats.RefetchedSegments)
		return nil
	}, nzbs, bytes, maxBytes, activeBytes, refetchedBytes, activeSegments, refetchedSegments)
	if err != nil {
		return fmt.Errorf("failed registering the library metrics callback: %w", err)
	}

	return nil
}

// observePresenters reports what the mount and the webdav tree are serving. The
// bytes are counters rather than the rate they are usually looked at as, which
// is the scrapers to derive.
func observePresenters() error {
	meter := otel.Meter("cmd/nzbstreamer")

	var errs []error
	gauge := func(name string, opts ...metric.Int64ObservableGaugeOption) metric.Int64ObservableGauge {
		instrument, err := meter.Int64ObservableGauge(name, opts...)
		errs = append(errs, err)
		return instrument
	}
	counter := func(name string, opts ...metric.Int64ObservableCounterOption) metric.Int64ObservableCounter {
		instrument, err := meter.Int64ObservableCounter(name, opts...)
		errs = append(errs, err)
		return instrument
	}

	// One name per measurement with the interface as a label, so what is served
	// altogether is one series to sum rather than one name per presenter to
	// know about
	open := gauge("presentation.open.handles",
		metric.WithDescription("Handles an interface holds open; the mount counts what a client has open, the webdav tree what it keeps pooled, which outlives the request that opened it"))
	served := counter("presentation.served.bytes",
		metric.WithDescription("Bytes handed to readers of an interface"),
		metric.WithUnit("By"))

	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("failed creating the presenter instruments: %w", err)
	}

	mount := metric.WithAttributes(interfaceKey.String("mount"))
	dav := metric.WithAttributes(interfaceKey.String("webdav"))

	_, err := meter.RegisterCallback(func(_ context.Context, observer metric.Observer) error {
		handles, bytes := fusemount.Stats()
		observer.ObserveInt64(open, handles, mount)
		observer.ObserveInt64(served, bytes, mount)

		handles, bytes = webdav.Stats()
		observer.ObserveInt64(open, handles, dav)
		observer.ObserveInt64(served, bytes, dav)
		return nil
	}, open, served)
	if err != nil {
		return fmt.Errorf("failed registering the presenter metrics callback: %w", err)
	}

	return nil
}

// observeReadahead reports what the windows pulled in against what of it was
// thrown away unread, which is the overshoot of the readahead itself: the layers
// underneath it fetch whole segments of a container, so nothing further down can
// tell a wasted byte from a par2 block nobody was ever going to read.
func observeReadahead() error {
	meter := otel.Meter("cmd/nzbstreamer")

	var errs []error
	gauge := func(name string, opts ...metric.Int64ObservableGaugeOption) metric.Int64ObservableGauge {
		instrument, err := meter.Int64ObservableGauge(name, opts...)
		errs = append(errs, err)
		return instrument
	}
	counter := func(name string, opts ...metric.Int64ObservableCounterOption) metric.Int64ObservableCounter {
		instrument, err := meter.Int64ObservableCounter(name, opts...)
		errs = append(errs, err)
		return instrument
	}

	fetched := counter("readahead.fetched.bytes",
		metric.WithDescription("Bytes the read windows pulled from underneath them, the chunk a read asked for and the ones warmed ahead of it alike"),
		metric.WithUnit("By"))
	discarded := counter("readahead.discarded.bytes",
		metric.WithDescription("Bytes of those the window dropped without a single read touching them, from a seek past them or a file closed mid-stream; a chunk one byte was read from counts as read whole"),
		metric.WithUnit("By"))
	// Chunks rather than bytes on both of these: the chunk size is one setting
	// for the process, so the bytes are the same number multiplied by it, and
	// what is being asked here is how many reads run at once
	inflight := gauge("readahead.inflight.chunks",
		metric.WithDescription("Chunk reads the windows have outstanding underneath them, which is the parallelism the layers below are actually asked for rather than what a window is allowed"))
	warm := counter("readahead.warm.chunks",
		metric.WithDescription("Warm window summed over the reads it served; over readahead.warm.reads it is the mean window a read ran under, which against READAHEAD_MAX_SIZE over READAHEAD_CHUNK is whether the ramp reaches the configured width at all"))
	warmReads := counter("readahead.warm.reads",
		metric.WithDescription("Reads the warm window was summed over, which is the divisor that turns readahead.warm.chunks into the mean"))

	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("failed creating the readahead instruments: %w", err)
	}

	_, err := meter.RegisterCallback(func(_ context.Context, observer metric.Observer) error {
		counts := readaheadresource.Stats()

		observer.ObserveInt64(fetched, counts.FetchedBytes)
		observer.ObserveInt64(discarded, counts.DiscardedBytes)
		observer.ObserveInt64(inflight, counts.InflightChunks)
		observer.ObserveInt64(warm, counts.WarmChunks)
		observer.ObserveInt64(warmReads, counts.WarmReads)
		return nil
	}, fetched, discarded, inflight, warm, warmReads)
	if err != nil {
		return fmt.Errorf("failed registering the readahead metrics callback: %w", err)
	}

	return nil
}

// observeCache reads the numbers the cache keeps for its own eviction, at
// collection time. Nothing in the cache knows about metrics.
func observeCache(cache *diskcache.Cache) error {
	meter := otel.Meter("cmd/nzbstreamer")

	var errs []error
	gauge := func(name string, opts ...metric.Int64ObservableGaugeOption) metric.Int64ObservableGauge {
		instrument, err := meter.Int64ObservableGauge(name, opts...)
		errs = append(errs, err)
		return instrument
	}
	counter := func(name string, opts ...metric.Int64ObservableCounterOption) metric.Int64ObservableCounter {
		instrument, err := meter.Int64ObservableCounter(name, opts...)
		errs = append(errs, err)
		return instrument
	}

	items := gauge("cache.items", metric.WithDescription("Segments the disk cache holds"))
	bytes := gauge("cache.bytes",
		metric.WithDescription("Bytes the disk cache holds"),
		metric.WithUnit("By"))
	maxBytes := gauge("cache.bytes.limit",
		metric.WithDescription("Bytes the disk cache may hold; 0 is unlimited"),
		metric.WithUnit("By"))
	hits := counter("cache.hits", metric.WithDescription("Reads served from the disk cache"))
	misses := counter("cache.misses", metric.WithDescription("Reads the disk cache did not hold"))
	evictions := counter("cache.evictions", metric.WithDescription("Items dropped to make room"))

	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("failed creating the cache instruments: %w", err)
	}

	_, err := meter.RegisterCallback(func(_ context.Context, observer metric.Observer) error {
		stats := cache.Stats()

		observer.ObserveInt64(items, int64(stats.Items))
		observer.ObserveInt64(bytes, stats.Bytes)
		observer.ObserveInt64(maxBytes, stats.MaxBytes)
		observer.ObserveInt64(hits, stats.Hits)
		observer.ObserveInt64(misses, stats.Misses)
		observer.ObserveInt64(evictions, stats.Evictions)
		return nil
	}, items, bytes, maxBytes, hits, misses, evictions)
	if err != nil {
		return fmt.Errorf("failed registering the cache metrics callback: %w", err)
	}

	return nil
}
