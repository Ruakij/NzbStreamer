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
		func(time.Time) (sqlstore.SegmentActivity, error) {
			return sqlstore.SegmentActivity{WorkingSet: 1024}, nil
		},
		time.Hour,
	)

	handler, err := setupMetrics(cache, library)
	if err != nil {
		t.Fatalf("failed setting up metrics: %v", err)
	}

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/metrics", nil))

	body := recorder.Body.String()
	for _, want := range []string{
		`cache_items{[^}]*} 1`, `cache_bytes{[^}]*} 7`,
		`library_bytes{[^}]*} 4096`, `library_active_bytes{[^}]*} 1024`,
	} {
		if !regexp.MustCompile(want).MatchString(body) {
			t.Errorf("metrics do not report %q:\n%s", want, body)
		}
	}
}
