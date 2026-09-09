package webui_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
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
	files   map[string][]nzbservice.PresentedFile
	addErr  error
	raw     map[string][]byte

	cancelErr  error
	deleteErr  error
	archiveErr error
	added      []string
	cancelled  []string
	deleted    []string
	archived   []string
}

func (s *fakeService) Add(nzbData *nzbparser.NzbData, _ string) (string, error) {
	if s.addErr != nil {
		return "", s.addErr
	}
	s.added = append(s.added, nzbData.MetaName)
	return nzbData.MetaName, nil
}

func (s *fakeService) NzbRaw(id string) ([]byte, error) {
	raw, ok := s.raw[id]
	if !ok {
		return nil, fmt.Errorf("%w: %s", nzbservice.ErrNzbNotFound, id)
	}
	return raw, nil
}

func (s *fakeService) Queue() []nzbservice.QueueItem                { return s.queue }
func (s *fakeService) History() []nzbservice.QueueItem              { return s.history }
func (s *fakeService) Files() map[string][]nzbservice.PresentedFile { return s.files }

func (s *fakeService) Cancel(id string) error {
	s.cancelled = append(s.cancelled, id)
	return s.cancelErr
}

func (s *fakeService) Delete(id string) error {
	s.deleted = append(s.deleted, id)
	return s.deleteErr
}

func (s *fakeService) Archive(id string, archived bool) error {
	s.archived = append(s.archived, fmt.Sprintf("%s/%t", id, archived))
	return s.archiveErr
}

const nzbXML = `<?xml version="1.0" encoding="utf-8" ?>
<nzb>
	<file poster="p@example.com" date="1700000000" subject="Release &#34;file.rar&#34; yEnc (1/1)">
		<groups><group>alt.binaries.test</group></groups>
		<segments><segment bytes="100" number="1">a@example.com</segment></segments>
	</file>
</nzb>`

func postAdd(t *testing.T, service *fakeService) *httptest.ResponseRecorder {
	t.Helper()

	var body bytes.Buffer
	form := multipart.NewWriter(&body)
	file, err := form.CreateFormFile("file", "Some.Release.nzb")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.Write([]byte(nzbXML)); err != nil {
		t.Fatal(err)
	}
	if err := form.Close(); err != nil {
		t.Fatal(err)
	}

	request := httptest.NewRequest(http.MethodPost, "/api/add", &body)
	request.Header.Set("Content-Type", form.FormDataContentType())
	recorder := httptest.NewRecorder()
	webui.NewHandler(service).ServeHTTP(recorder, request)
	return recorder
}

func TestAddAlreadyExistsIsBadRequest(t *testing.T) {
	if recorder := postAdd(t, &fakeService{addErr: nzbservice.ErrNzbAlreadyExists}); recorder.Code != http.StatusBadRequest {
		t.Fatalf("duplicate add answered %d, want 400", recorder.Code)
	}
}

func TestAddLibraryFullIsInsufficientStorage(t *testing.T) {
	if recorder := postAdd(t, &fakeService{addErr: fmt.Errorf("%w: Some.Release", nzbservice.ErrLibraryFull)}); recorder.Code != http.StatusInsufficientStorage {
		t.Fatalf("full library answered %d, want 507", recorder.Code)
	}
}

// The download is the submitted document, offered under a name a browser saves
// it as, and an unknown id is a 404 rather than an empty file.
func TestTheNzbOfARecordIsDownloadable(t *testing.T) {
	service := &fakeService{raw: map[string][]byte{`Some "Release"`: []byte(nzbXML)}}

	request := httptest.NewRequest(http.MethodGet, "/api/nzb/file?id="+url.QueryEscape(`Some "Release"`), nil)
	recorder := httptest.NewRecorder()
	webui.NewHandler(service).ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("download answered %d, want 200", recorder.Code)
	}
	if recorder.Body.String() != nzbXML {
		t.Errorf("body: got %q, want %q", recorder.Body.String(), nzbXML)
	}
	if got := recorder.Header().Get("Content-Type"); got != "application/x-nzb" {
		t.Errorf("content-type: got %q", got)
	}

	disposition, params, err := mime.ParseMediaType(recorder.Header().Get("Content-Disposition"))
	if err != nil {
		t.Fatalf("Content-Disposition %q: %v", recorder.Header().Get("Content-Disposition"), err)
	}
	if disposition != "attachment" || params["filename"] != `Some "Release".nzb` {
		t.Errorf("disposition: got %q %v", disposition, params)
	}

	recorder = httptest.NewRecorder()
	webui.NewHandler(service).ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/nzb/file?id=nothing", nil))
	if recorder.Code != http.StatusNotFound {
		t.Errorf("unknown id answered %d, want 404", recorder.Code)
	}
}

func TestRemoveMapsServiceErrors(t *testing.T) {
	tests := []struct {
		action string
		setup  func(*fakeService)
		want   int
	}{
		{"cancel", func(s *fakeService) { s.cancelErr = fmt.Errorf("%w: x", nzbservice.ErrNzbNotFound) }, http.StatusNotFound},
		{"delete", func(s *fakeService) { s.deleteErr = fmt.Errorf("%w: x", nzbservice.ErrNzbStillRunning) }, http.StatusConflict},
		{"archive", func(s *fakeService) { s.archiveErr = fmt.Errorf("%w: x", nzbservice.ErrNzbNotFound) }, http.StatusNotFound},
		{"restore", func(s *fakeService) { s.archiveErr = fmt.Errorf("%w: x", nzbservice.ErrNzbStillRunning) }, http.StatusConflict},
	}

	for _, test := range tests {
		service := &fakeService{}
		test.setup(service)
		request := httptest.NewRequest(http.MethodPost, "/api/remove", strings.NewReader("id=x&action="+test.action))
		request.Header.Set("Content-Type", "application/x-www-form-urlencoded")

		recorder := httptest.NewRecorder()
		webui.NewHandler(service).ServeHTTP(recorder, request)
		if recorder.Code != test.want {
			t.Errorf("%s answered %d, want %d", test.action, recorder.Code, test.want)
		}
	}
}

func TestItems(t *testing.T) {
	service := &fakeService{
		queue:   []nzbservice.QueueItem{{ID: "a", Stage: nzbservice.StageChecking, Bytes: 42, Added: time.Now()}},
		history: []nzbservice.QueueItem{{ID: "b", Stage: nzbservice.StageFailed, Err: "boom"}},
		files:   map[string][]nzbservice.PresentedFile{"b": {{Path: "b/video.mkv", Bytes: 7, Exact: true}}},
	}

	recorder := httptest.NewRecorder()
	webui.NewHandler(service).ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/items", nil))

	var body struct {
		Queue   []map[string]any            `json:"queue"`
		History []map[string]any            `json:"history"`
		Files   map[string][]map[string]any `json:"files"`
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
	if got := body.Files["b"]; len(got) != 1 || got[0]["path"] != "b/video.mkv" || got[0]["bytes"] != float64(7) {
		t.Errorf("files are %v, want the one presented file sized 7", got)
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

const testNzb = `<?xml version="1.0"?>
<nzb><head><meta type="name">Some.Release</meta></head>
<file poster="p@example.com" date="1700000000" subject="[1/1] - &#34;file.rar&#34; yEnc (1/2)">
<groups><group>alt.binaries.test</group></groups>
<segments><segment bytes="10" number="1">a@n</segment><segment bytes="20" number="2">b@n</segment></segments>
</file></nzb>`

// contentNzb posts its sizes decoded: hints of exactly a segment size are what
// identifies that, since a wire-counted one carries the yEnc overhead on top.
const contentNzb = `<?xml version="1.0"?>
<nzb><head><meta type="name">Some.Release</meta></head>
<file poster="p@example.com" date="1700000000" subject="[1/1] - &#34;file.rar&#34; yEnc (1/3)">
<groups><group>alt.binaries.test</group></groups>
<segments><segment bytes="716800" number="1">a@n</segment><segment bytes="716800" number="2">b@n</segment><segment bytes="1024" number="3">c@n</segment></segments>
</file></nzb>`

func inspect(t *testing.T, service *fakeService, nzb string) *httptest.ResponseRecorder {
	t.Helper()

	var upload bytes.Buffer
	form := multipart.NewWriter(&upload)
	part, err := form.CreateFormFile("file", "Some.Release.nzb")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write([]byte(nzb)); err != nil {
		t.Fatal(err)
	}
	form.Close()

	request := httptest.NewRequest(http.MethodPost, "/api/inspect", &upload)
	request.Header.Set("Content-Type", form.FormDataContentType())
	recorder := httptest.NewRecorder()
	webui.NewHandler(service).ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("inspect answered %d: %s", recorder.Code, recorder.Body)
	}

	return recorder
}

// An nzb counting its bytes decoded says nothing about what went over the wire,
// so there is no wire size to report rather than the decoded one twice.
func TestInspectLeavesOutTheWireSizeItCannotKnow(t *testing.T) {
	recorder := inspect(t, &fakeService{}, contentNzb)

	var body struct {
		Convention string           `json:"convention"`
		Wire       *int             `json:"wire"`
		Bytes      int              `json:"bytes"`
		Exact      bool             `json:"exact"`
		Files      []map[string]any `json:"files"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("response was not json: %v (%s)", err, recorder.Body)
	}

	if body.Convention != "content" || !body.Exact || body.Bytes != 716800*2+1024 {
		t.Fatalf("read it as %s, %d bytes exact=%t; want the exact content sizes",
			body.Convention, body.Bytes, body.Exact)
	}
	if body.Wire != nil {
		t.Errorf("reported %d bytes on the wire", *body.Wire)
	}
	if _, ok := body.Files[0]["wire"]; ok {
		t.Errorf("posted file reports %v on the wire", body.Files[0]["wire"])
	}
}

func TestInspectReportsTheParseWithoutAdding(t *testing.T) {
	service := &fakeService{}
	recorder := inspect(t, service, testNzb)

	var body struct {
		Name     string           `json:"name"`
		Wire     int              `json:"wire"`
		Bytes    int              `json:"bytes"`
		Exact    bool             `json:"exact"`
		Segments int              `json:"segments"`
		Files    []map[string]any `json:"files"`
		Errors   []string         `json:"errors"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("response was not json: %v (%s)", err, recorder.Body)
	}

	if body.Name != "Some.Release" {
		t.Errorf("name is %q, want Some.Release", body.Name)
	}
	if body.Wire != 30 || body.Segments != 2 {
		t.Errorf("got %d wire bytes in %d segments, want 30 in 2", body.Wire, body.Segments)
	}
	// Nothing identifies the convention of this one, so the content size is an
	// estimate below what the wire size says
	if body.Exact || body.Bytes >= body.Wire {
		t.Errorf("got %d bytes exact=%t, want an estimate under %d", body.Bytes, body.Exact, body.Wire)
	}
	if len(body.Files) != 1 || body.Files[0]["filename"] != "file.rar" {
		t.Errorf("files are %v, want the one posted file", body.Files)
	}
	if len(body.Errors) != 0 {
		t.Errorf("plausible nzb reported %v", body.Errors)
	}
	if len(service.added) != 0 {
		t.Errorf("inspect added %v, want nothing added", service.added)
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
