package main

import (
	"net/http"
	"net/http/httptest"
	"regexp"
	"testing"
	"time"

	"git.ruekov.eu/ruakij/nzbStreamer/internal/nzbstore/sqlstore"
	"git.ruekov.eu/ruakij/nzbStreamer/internal/service/nzbservice"
	"git.ruekov.eu/ruakij/nzbStreamer/pkg/diskcache"
)

// A collector endpoint is a url so that the transport is read off it, and a
// protocol that is not one of the two is a startup error rather than metrics
// that silently go nowhere.
func TestOtlpReaderTakesAUrlAndAKnownProtocol(t *testing.T) {
	for _, endpoint := range []string{"grpc://collector:4317", "https://collector:4317", "http://collector:4318"} {
		for _, protocol := range []string{"grpc", "http"} {
			reader, err := otlpReader(t.Context(), MetricsConfig{OTLPEndpoint: endpoint, OTLPProtocol: protocol, OTLPInterval: time.Minute})
			if err != nil {
				t.Errorf("%s over %s: %v", endpoint, protocol, err)
				continue
			}
			if err := reader.Shutdown(t.Context()); err != nil {
				t.Errorf("shutdown: %v", err)
			}
		}
	}

	if _, err := otlpReader(t.Context(), MetricsConfig{OTLPEndpoint: "collector:4317", OTLPProtocol: "grpc"}); err == nil {
		t.Error("an endpoint without a scheme was accepted")
	}
	if _, err := otlpReader(t.Context(), MetricsConfig{OTLPEndpoint: "grpc://collector:4317", OTLPProtocol: "thrift"}); err == nil {
		t.Error("an unknown protocol was accepted")
	}
}

// The exporter reaches the accessors it was given: an instrument declared here
// and never recorded into is still a series, and one whose callback never runs
// is not.
func TestMetricsServeWhatTheCacheHolds(t *testing.T) {
	cache, err := diskcache.NewCache(&diskcache.CacheOptions{CacheDir: t.TempDir()})
	if err != nil {
		t.Fatalf("failed creating cache: %v", err)
	}
	select {
	case <-cache.Indexed():
	case <-time.After(10 * time.Second):
		t.Fatal("cache did not finish indexing")
	}
	if _, err := cache.Set(diskcache.Key{"an-nzb", "segment-a"}, []byte("payload")); err != nil {
		t.Fatalf("failed storing: %v", err)
	}

	library := newLibraryMeter(
		func() nzbservice.Library { return nzbservice.Library{Nzbs: 2, Bytes: 4096} },
		func(cutoffs []time.Time) ([]sqlstore.SegmentActivity, error) {
			activity := make([]sqlstore.SegmentActivity, len(cutoffs))
			for i := range activity {
				activity[i] = sqlstore.SegmentActivity{
					WorkingSetBytes:    int64(1024 * (i + 1)),
					WorkingSetSegments: int64(3 * (i + 1)),
				}
			}
			return activity, nil
		},
		func() (int64, int64) { return 512, 2 },
		[]time.Duration{24 * time.Hour, 168 * time.Hour},
	)

	handler, _, err := setupMetrics(t.Context(), MetricsConfig{}, cache, library)
	if err != nil {
		t.Fatalf("failed setting up metrics: %v", err)
	}

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/metrics", nil))

	body := recorder.Body.String()
	for _, want := range []string{
		`cache_items{[^}]*} 1`, `cache_bytes{[^}]*} 7`,
		`library_bytes{[^}]*} 4096`,
		`library_active_bytes{[^}]*window="1d"[^}]*} 1024`,
		`library_active_bytes{[^}]*window="7d"[^}]*} 2048`,
		`library_active_segments{[^}]*window="7d"[^}]*} 6`,
		`cache_refetched_bytes_total{[^}]*} 512`, `cache_refetched_segments_total{[^}]*} 2`,
		`readahead_fetched_bytes_total{`, `readahead_discarded_bytes_total{`,
	} {
		if !regexp.MustCompile(want).MatchString(body) {
			t.Errorf("metrics do not report %q:\n%s", want, body)
		}
	}
}
