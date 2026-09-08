package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/prometheus"
	"go.opentelemetry.io/otel/metric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"

	"git.ruekov.eu/ruakij/nzbStreamer/pkg/diskcache"
)

// setupMetrics installs the meter provider every package records into and
// returns the handler that serves what it holds. This is the only place that
// knows an exporter exists; until it runs, the instruments the packages declare
// are the no-op the global provider hands out.
func setupMetrics(cache *diskcache.Cache, library *libraryMeter) (http.Handler, error) {
	exporter, err := prometheus.New()
	if err != nil {
		return nil, fmt.Errorf("failed creating the prometheus exporter: %w", err)
	}

	otel.SetMeterProvider(sdkmetric.NewMeterProvider(sdkmetric.WithReader(exporter)))

	if err := observeCache(cache); err != nil {
		return nil, err
	}
	if err := observeLibrary(library); err != nil {
		return nil, err
	}

	return promhttp.Handler(), nil
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

	nzbs := gauge("library.nzbs", metric.WithDescription("Nzbs presented"))
	bytes := gauge("library.bytes",
		metric.WithDescription("Bytes the presented nzbs describe, cached or not"),
		metric.WithUnit("By"))
	maxBytes := gauge("library.max_bytes",
		metric.WithDescription("Bytes the library may describe before adds are refused; 0 is unlimited"),
		metric.WithUnit("By"))
	activeBytes := gauge("library.active_bytes",
		metric.WithDescription("Bytes of the distinct segments read within LIBRARY_ACTIVE_WINDOW, which is what the cache would have to hold to serve them without a refetch"),
		metric.WithUnit("By"))
	refetchedBytes := gauge("cache.refetched_bytes",
		metric.WithDescription("Bytes of the active library that had to be downloaded again within the window, which a cache with room for all of it would have served"),
		metric.WithUnit("By"))

	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("failed creating the library instruments: %w", err)
	}

	_, err := meter.RegisterCallback(func(_ context.Context, observer metric.Observer) error {
		stats := library.read()

		observer.ObserveInt64(nzbs, int64(stats.Nzbs))
		observer.ObserveInt64(bytes, stats.Bytes)
		observer.ObserveInt64(maxBytes, stats.MaxBytes)
		observer.ObserveInt64(activeBytes, stats.WorkingSet)
		observer.ObserveInt64(refetchedBytes, stats.Refetched)
		return nil
	}, nzbs, bytes, maxBytes, activeBytes, refetchedBytes)
	if err != nil {
		return fmt.Errorf("failed registering the library metrics callback: %w", err)
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
	maxBytes := gauge("cache.max_bytes",
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
