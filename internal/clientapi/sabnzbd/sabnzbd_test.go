package sabnzbd_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"git.ruekov.eu/ruakij/nzbStreamer/internal/clientapi/sabnzbd"
	"git.ruekov.eu/ruakij/nzbStreamer/internal/service/nzbservice"
	"git.ruekov.eu/ruakij/nzbStreamer/pkg/nzbparser"
)

const nzbXML = `<?xml version="1.0" encoding="utf-8" ?>
<nzb>
	<file poster="p@example.com" date="1700000000" subject="Release &#34;file.rar&#34; yEnc (1/1)">
		<groups><group>alt.binaries.test</group></groups>
		<segments><segment bytes="100" number="1">a@example.com</segment></segments>
	</file>
</nzb>`

type fakeService struct {
	queue   []nzbservice.QueueItem
	history []nzbservice.QueueItem

	added     []string
	cancelled []string
	deleted   []string
}

func (s *fakeService) Add(nzbData *nzbparser.NzbData, category string) (string, error) {
	s.added = append(s.added, nzbData.MetaName+"/"+category)
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

func call(t *testing.T, handler *sabnzbd.Handler, query string) map[string]any {
	t.Helper()

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api?"+query, nil))

	var body map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("response to %q was not json: %v (%s)", query, err, recorder.Body)
	}
	return body
}

// Everything a client reads before it accepts this as a download client: a
// version it can parse, and a config whose category it was configured with is
// present, whose sorting is off and whose retention says nothing is removed
// behind its back.
func TestAClientCanValidateThisAsADownloadClient(t *testing.T) {
	handler := sabnzbd.NewHandler(&fakeService{}, sabnzbd.Config{
		CompleteDir: "/mnt/nzb",
		Categories:  []string{"*", "tv"},
	})

	if version := call(t, handler, "mode=version")["version"]; version != sabnzbd.Version {
		t.Errorf("version: got %v", version)
	}

	config, ok := call(t, handler, "mode=get_config")["config"].(map[string]any)
	if !ok {
		t.Fatalf("get_config has no config object")
	}

	misc, _ := config["misc"].(map[string]any)
	if misc["complete_dir"] != "/mnt/nzb" {
		t.Errorf("complete_dir: got %v", misc["complete_dir"])
	}
	for _, key := range []string{"pre_check", "enable_tv_sorting", "enable_movie_sorting", "enable_date_sorting"} {
		if misc[key] != false {
			t.Errorf("%s: got %v, want false", key, misc[key])
		}
	}
	// Anything but "all" tells a client this removes completed downloads itself,
	// and it then stops expecting them to still be there
	if misc["history_retention_option"] != "all" {
		t.Errorf("history_retention_option: got %v", misc["history_retention_option"])
	}

	categories, _ := config["categories"].([]any)
	if len(categories) != 2 {
		t.Fatalf("categories: got %v", categories)
	}
	for _, entry := range categories {
		category, _ := entry.(map[string]any)
		// A directory ending in * means job folders are off, which a client
		// refuses to import from
		if dir, _ := category["dir"].(string); dir != "" {
			t.Errorf("category %v has dir %q", category["name"], dir)
		}
	}

	if status := call(t, handler, "mode=fullstatus")["status"].(map[string]any)["completedir"]; status != "/mnt/nzb" {
		t.Errorf("fullstatus completedir: got %v", status)
	}
}

func TestAddingAnNzbReturnsTheIdToTrackItUnder(t *testing.T) {
	service := &fakeService{}
	handler := sabnzbd.NewHandler(service, sabnzbd.Config{})

	var body bytes.Buffer
	form := multipart.NewWriter(&body)
	// The field name the *arrs use
	file, err := form.CreateFormFile("name", "Some.Release.nzb")
	if err != nil {
		t.Fatalf("CreateFormFile: %v", err)
	}
	if _, err := file.Write([]byte(nzbXML)); err != nil {
		t.Fatalf("write: %v", err)
	}
	form.Close()

	request := httptest.NewRequest(http.MethodPost, "/api?mode=addfile&cat=tv", &body)
	request.Header.Set("Content-Type", form.FormDataContentType())

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)

	var response struct {
		Status bool     `json:"status"`
		Ids    []string `json:"nzo_ids"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("response was not json: %v (%s)", err, recorder.Body)
	}
	if !response.Status || len(response.Ids) != 1 || response.Ids[0] != "Some.Release" {
		t.Fatalf("addfile answered %+v", response)
	}
	if len(service.added) != 1 || service.added[0] != "Some.Release/tv" {
		t.Errorf("service saw %v", service.added)
	}
}

// The fields the *arrs actually read off a queue and a history item, and the
// category filter that decides whether they look at an item at all.
func TestQueueAndHistoryCarryWhatAClientReads(t *testing.T) {
	added := time.Now().Add(-time.Minute)
	service := &fakeService{
		queue: []nzbservice.QueueItem{
			{ID: "Checking.Release", Category: "tv", Stage: nzbservice.StageChecking, Bytes: 2 * 1024 * 1024, Added: added},
			{ID: "Someone.Elses", Category: "movies", Stage: nzbservice.StageQueued},
		},
		history: []nzbservice.QueueItem{
			{ID: "Done.Release", Category: "tv", Stage: nzbservice.StageCompleted, Bytes: 700, Added: added, Finished: added.Add(30 * time.Second)},
			{ID: "Dead.Release", Category: "tv", Stage: nzbservice.StageFailed, Err: "3 files beyond repair"},
			{ID: "Gone.Release", Category: "tv", Stage: nzbservice.StageCancelled},
		},
	}
	handler := sabnzbd.NewHandler(service, sabnzbd.Config{CompleteDir: "/mnt/nzb"})

	queue, _ := call(t, handler, "mode=queue&category=tv")["queue"].(map[string]any)
	slots, _ := queue["slots"].([]any)
	if len(slots) != 1 {
		t.Fatalf("queue filtered by category was %v", slots)
	}
	slot, _ := slots[0].(map[string]any)
	if slot["nzo_id"] != "Checking.Release" || slot["filename"] != "Checking.Release" {
		t.Errorf("queue item identity: %v", slot)
	}
	if slot["status"] != "Verifying" || slot["cat"] != "tv" {
		t.Errorf("queue item status: %v", slot)
	}
	// Megabytes, as a string, which is what a client parses into a size
	if slot["mb"] != "2.00" || slot["mbleft"] != "2.00" {
		t.Errorf("queue item size: %v", slot)
	}

	history, _ := call(t, handler, "mode=history&category=tv")["history"].(map[string]any)
	items := map[string]map[string]any{}
	for _, entry := range history["slots"].([]any) {
		item, _ := entry.(map[string]any)
		items[item["nzo_id"].(string)] = item
	}
	if len(items) != 3 {
		t.Fatalf("history was %v", items)
	}

	done := items["Done.Release"]
	if done["status"] != "Completed" {
		t.Errorf("a completed add reported %v", done["status"])
	}
	// Bytes here, unlike the queue, and the folder the add presents its files in
	if done["bytes"] != float64(700) || done["storage"] != "/mnt/nzb/Done.Release" {
		t.Errorf("completed item: %v", done)
	}

	dead := items["Dead.Release"]
	if dead["status"] != "Failed" || dead["fail_message"] != "3 files beyond repair" {
		t.Errorf("a failed add reported %v", dead)
	}
	// A cancel was the clients own doing, and Deleted is the status both *arrs
	// skip over rather than blacklisting the release
	if items["Gone.Release"]["status"] != "Deleted" {
		t.Errorf("a cancelled add reported %v", items["Gone.Release"]["status"])
	}
}

// A client deletes from the queue while an add runs and from the history once it
// has imported. They are different calls and they mean different things here.
func TestDeletingFromTheQueueCancelsAndFromTheHistoryRemoves(t *testing.T) {
	service := &fakeService{}
	handler := sabnzbd.NewHandler(service, sabnzbd.Config{})

	if status := call(t, handler, "mode=queue&name=delete&value=Some.Release&del_files=1")["status"]; status != true {
		t.Errorf("queue delete answered %v", status)
	}
	if status := call(t, handler, "mode=history&name=delete&value=Other.Release&del_files=0")["status"]; status != true {
		t.Errorf("history delete answered %v", status)
	}

	if fmt.Sprint(service.cancelled) != "[Some.Release]" {
		t.Errorf("cancelled: %v", service.cancelled)
	}
	// del_files=0 changes nothing: the record and the files are one thing here
	if fmt.Sprint(service.deleted) != "[Other.Release]" {
		t.Errorf("deleted: %v", service.deleted)
	}
}

func TestAFailureIsReportedTheWayAClientChecksFor(t *testing.T) {
	service := &fakeService{}
	handler := sabnzbd.NewHandler(service, sabnzbd.Config{APIKey: "secret"})

	// The exact messages the *arrs match on to tell a wrong key from a missing one
	if got := call(t, handler, "mode=version")["error"]; got != "API Key Required" {
		t.Errorf("without a key: %v", got)
	}
	if got := call(t, handler, "mode=version&apikey=wrong")["error"]; got != "API Key Incorrect" {
		t.Errorf("with a wrong key: %v", got)
	}
	if got := call(t, handler, "mode=version&apikey=secret")["version"]; got != sabnzbd.Version {
		t.Errorf("with the right key: %v", got)
	}

	// A status of false is what a client looks at before anything else
	response := call(t, handler, "mode=warnings&apikey=secret")
	if response["status"] != false || response["error"] == "" {
		t.Errorf("an unimplemented mode answered %v", response)
	}
}

// The delete calls report what went wrong rather than claiming success, since a
// client that believes a delete happened stops asking.
func TestADeleteThatFailsSaysSo(t *testing.T) {
	handler := sabnzbd.NewHandler(&failingService{}, sabnzbd.Config{})

	response := call(t, handler, "mode=history&name=delete&value=Some.Release")
	if response["status"] != false || response["error"] == "" {
		t.Errorf("a failed delete answered %v", response)
	}
}

type failingService struct{ fakeService }

func (s *failingService) Delete(_ string) error { return errors.New("nzb not found") }
