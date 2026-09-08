package webui_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"git.ruekov.eu/ruakij/nzbStreamer/internal/clientapi/webui"
	"git.ruekov.eu/ruakij/nzbStreamer/internal/service/nzbservice"
	"git.ruekov.eu/ruakij/nzbStreamer/pkg/nzbparser"
)

type fakeService struct {
	queue   []nzbservice.QueueItem
	history []nzbservice.QueueItem
	files   map[string][]string

	cancelled []string
	deleted   []string
	archived  []string
}

func (s *fakeService) Add(nzbData *nzbparser.NzbData, _ string) (string, error) {
	return nzbData.MetaName, nil
}

func (s *fakeService) Queue() []nzbservice.QueueItem   { return s.queue }
func (s *fakeService) History() []nzbservice.QueueItem { return s.history }
func (s *fakeService) Files() map[string][]string      { return s.files }

func (s *fakeService) Cancel(id string) error {
	s.cancelled = append(s.cancelled, id)
	return nil
}

func (s *fakeService) Delete(id string) error {
	s.deleted = append(s.deleted, id)
	return nil
}

func (s *fakeService) Archive(id string, archived bool) error {
	s.archived = append(s.archived, fmt.Sprintf("%s/%t", id, archived))
	return nil
}

func TestItems(t *testing.T) {
	service := &fakeService{
		queue:   []nzbservice.QueueItem{{ID: "a", Stage: nzbservice.StageChecking, Bytes: 42, Added: time.Now()}},
		history: []nzbservice.QueueItem{{ID: "b", Stage: nzbservice.StageFailed, Err: "boom"}},
		files:   map[string][]string{"b": {"b/video.mkv"}},
	}

	recorder := httptest.NewRecorder()
	webui.NewHandler(service).ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/items", nil))

	var body struct {
		Queue   []map[string]any    `json:"queue"`
		History []map[string]any    `json:"history"`
		Files   map[string][]string `json:"files"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("response was not json: %v (%s)", err, recorder.Body)
	}

	if got := body.Queue[0]["stage"]; got != "checking" {
		t.Errorf("queue stage is %v, want checking", got)
	}
	if got := body.Queue[0]["bytes"]; got != float64(42) {
		t.Errorf("queue bytes is %v, want 42", got)
	}
	if got := body.History[0]["error"]; got != "boom" {
		t.Errorf("history error is %v, want boom", got)
	}
	if got := body.Files["b"]; len(got) != 1 || got[0] != "b/video.mkv" {
		t.Errorf("files are %v, want [b/video.mkv]", got)
	}
}

func TestStatsAndNzbDetail(t *testing.T) {
	handler := webui.NewHandler(&fakeService{})
	handler.Stats = func() any { return map[string]any{"cache": map[string]any{"items": 3}} }
	handler.NzbStats = func(id string) any {
		if id != "b" {
			return nil
		}
		return map[string]any{"id": id, "cached_bytes": 7}
	}

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/items", nil))
	if !strings.Contains(recorder.Body.String(), `"items":3`) {
		t.Errorf("items answered %s, want the stats provider's numbers", recorder.Body)
	}

	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/nzb?id=b", nil))
	if !strings.Contains(recorder.Body.String(), `"cached_bytes":7`) {
		t.Errorf("nzb answered %s, want the detail", recorder.Body)
	}

	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/nzb?id=gone", nil))
	if recorder.Code != http.StatusNotFound {
		t.Errorf("unknown nzb answered %d, want 404", recorder.Code)
	}
}

func TestPageRevalidates(t *testing.T) {
	handler := webui.NewHandler(&fakeService{})

	for _, path := range []string{"/", "/static/app.js"} {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))
		etag := recorder.Header().Get("ETag")
		if recorder.Code != http.StatusOK || etag == "" {
			t.Fatalf("%s answered %d with etag %q", path, recorder.Code, etag)
		}

		request := httptest.NewRequest(http.MethodGet, path, nil)
		request.Header.Set("If-None-Match", etag)
		recorder = httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusNotModified {
			t.Errorf("%s answered %d to its own etag, want 304", path, recorder.Code)
		}
	}
}

func TestRemoveRoutesAction(t *testing.T) {
	service := &fakeService{}
	handler := webui.NewHandler(service)

	for _, action := range []string{"cancel", "delete", "archive", "restore"} {
		request := httptest.NewRequest(http.MethodPost, "/api/remove", strings.NewReader("id=x&action="+action))
		request.Header.Set("Content-Type", "application/x-www-form-urlencoded")

		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusNoContent {
			t.Fatalf("%s answered %d: %s", action, recorder.Code, recorder.Body)
		}
	}

	if len(service.cancelled) != 1 || service.cancelled[0] != "x" {
		t.Errorf("cancel got %v, want [x]", service.cancelled)
	}
	if len(service.deleted) != 1 || service.deleted[0] != "x" {
		t.Errorf("delete got %v, want [x]", service.deleted)
	}
	if fmt.Sprint(service.archived) != "[x/true x/false]" {
		t.Errorf("archive and restore got %v", service.archived)
	}
}
