package main

import (
	"net/http"
	"net/http/httptest"
	"regexp"
	"testing"
	"time"

	"git.ruekov.eu/ruakij/nzbStreamer/pkg/diskcache"
)

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

	handler, err := setupMetrics(cache)
	if err != nil {
		t.Fatalf("failed setting up metrics: %v", err)
	}

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/metrics", nil))

	body := recorder.Body.String()
	for _, want := range []string{`cache_items{[^}]*} 1`, `cache_bytes{[^}]*} 7`} {
		if !regexp.MustCompile(want).MatchString(body) {
			t.Errorf("metrics do not report %q:\n%s", want, body)
		}
	}
}
