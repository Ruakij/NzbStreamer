package webui_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"git.ruekov.eu/ruakij/nzbStreamer/internal/clientapi/webui"
)

func component(name, status string, gates bool) webui.Component {
	return webui.Component{Name: name, Gates: gates, Health: func() webui.Status {
		return webui.Status{Status: status}
	}}
}

func health(t *testing.T, components ...webui.Component) (int, string) {
	t.Helper()

	recorder := httptest.NewRecorder()
	webui.NewHandler(&fakeService{}, components...).
		ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/health", nil))

	var body struct {
		Status     string                  `json:"status"`
		Components map[string]webui.Status `json:"components"`
	}
	if err := json.NewDecoder(recorder.Body).Decode(&body); err != nil {
		t.Fatalf("decoding body: %v", err)
	}
	if len(body.Components) != len(components) {
		t.Fatalf("got %d components, want %d", len(body.Components), len(components))
	}

	return recorder.Code, body.Status
}

func TestTheCodeGatesOnSomeComponentsAndTheWordOnAll(t *testing.T) {
	tests := []struct {
		name       string
		components []webui.Component
		code       int
		status     string
	}{{
		name:       "everything up",
		components: []webui.Component{component("cache", webui.StatusUp, true), component("usenet", webui.StatusUp, false)},
		code:       http.StatusOK,
		status:     webui.StatusUp,
	}, {
		// a provider outage is not a stopped service, so it is said and not acted on
		name:       "a non-gating component is degraded",
		components: []webui.Component{component("cache", webui.StatusUp, true), component("usenet", webui.StatusDegraded, false)},
		code:       http.StatusOK,
		status:     webui.StatusDegraded,
	}, {
		name:       "a gating component is down",
		components: []webui.Component{component("cache", webui.StatusDown, true), component("usenet", webui.StatusUp, false)},
		code:       http.StatusServiceUnavailable,
		status:     webui.StatusDown,
	}, {
		// the restore, which is a library that cannot be listed in full yet
		name:       "a gating component is degraded",
		components: []webui.Component{component("metadata-db", webui.StatusDegraded, true)},
		code:       http.StatusServiceUnavailable,
		status:     webui.StatusDegraded,
	}, {
		name:       "an unconfigured component is neither",
		components: []webui.Component{component("mount", webui.StatusDisabled, true)},
		code:       http.StatusOK,
		status:     webui.StatusUp,
	}}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			code, status := health(t, test.components...)
			if code != test.code || status != test.status {
				t.Errorf("got %d %q, want %d %q", code, status, test.code, test.status)
			}
		})
	}
}

func TestLivenessLooksAtNothing(t *testing.T) {
	down := webui.Component{Name: "cache", Gates: true, Health: func() webui.Status {
		t.Error("liveness asked a component")
		return webui.Status{Status: webui.StatusDown}
	}}

	recorder := httptest.NewRecorder()
	webui.NewHandler(&fakeService{}, down).
		ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/health/live", nil))

	if recorder.Code != http.StatusOK {
		t.Errorf("got %d, want %d", recorder.Code, http.StatusOK)
	}
}
