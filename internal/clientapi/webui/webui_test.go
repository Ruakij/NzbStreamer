package webui_test

import (
	"encoding/json"
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

	cancelled []string
	deleted   []string
}

func (s *fakeService) Add(nzbData *nzbparser.NzbData, _ string) (string, error) {
	return nzbData.MetaName, nil
}

func (s *fakeService) Queue() []nzbservice.QueueItem   { return s.queue }
func (s *fakeService) History() []nzbservice.QueueItem { return s.history }

func (s *fakeService) Cancel(id string) error {
	s.cancelled = append(s.cancelled, id)
	return nil
}

func (s *fakeService) Delete(id string) error {
	s.deleted = append(s.deleted, id)
	return nil
}

func TestItems(t *testing.T) {
	service := &fakeService{
		queue:   []nzbservice.QueueItem{{ID: "a", Stage: nzbservice.StageChecking, Bytes: 42, Added: time.Now()}},
		history: []nzbservice.QueueItem{{ID: "b", Stage: nzbservice.StageFailed, Err: "boom"}},
	}

	recorder := httptest.NewRecorder()
	webui.NewHandler(service).ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/items", nil))

	var body struct {
		Queue   []map[string]any `json:"queue"`
		History []map[string]any `json:"history"`
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
}

func TestRemoveRoutesAction(t *testing.T) {
	service := &fakeService{}
	handler := webui.NewHandler(service)

	for _, action := range []string{"cancel", "delete"} {
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
}
