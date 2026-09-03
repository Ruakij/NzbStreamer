// Package sabnzbd presents the service's queue as the part of the SABnzbd API
// that Sonarr and Radarr use: one endpoint, `mode` in the query string, json out.
//
// What it answers is what those clients parse, taken from their source rather
// than from the SABnzbd docs - they validate a download client when it is saved
// and read a handful of fields very literally. The rest of the api is not
// implemented and says so.
package sabnzbd

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"

	"git.ruekov.eu/ruakij/nzbStreamer/internal/service/nzbservice"
	"git.ruekov.eu/ruakij/nzbStreamer/pkg/nzbparser"
)

var logger = slog.With("Module", "Sabnzbd")

// Version reported to a client. Sonarr requires 0.7.0 or newer, reads the
// history retention settings introduced in 4.3 to decide whether this client
// removes completed downloads on its own, and treats the literal "develop" as a
// warning.
const Version = "4.3.3"

// megabyte is what the queue reports sizes in, unlike the history, which is bytes
const megabyte = 1024 * 1024

// Service is the part of nzbservice this surface projects.
type Service interface {
	Add(nzbData *nzbparser.NzbData, category string) (string, error)
	Queue() []nzbservice.QueueItem
	History() []nzbservice.QueueItem
	Cancel(id string) error
	Delete(id string) error
}

type Config struct {
	// APIKey required on every request; unauthenticated when empty
	APIKey string
	// CompleteDir is where a client is told finished downloads are, which is the
	// root of the mount: an add presents its files under a folder named after it
	CompleteDir string
	// Categories offered to a client. A client refuses to save if the category it
	// is configured with is not among them.
	Categories []string
}

type Handler struct {
	service Service
	config  Config
}

func NewHandler(service Service, config Config) *Handler {
	return &Handler{service: service, config: config}
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()

	if err := h.authenticate(query); err != "" {
		writeError(w, err)
		return
	}

	mode := query.Get("mode")
	logger.Debug("Request", "mode", mode, "query", query.Encode())

	switch mode {
	case "version":
		writeJSON(w, map[string]string{"version": Version})
	case "get_config":
		writeJSON(w, h.configResponse())
	case "fullstatus":
		writeJSON(w, map[string]any{"status": map[string]string{"completedir": h.config.CompleteDir}})
	case "addfile":
		h.addFile(w, r, query)
	case "queue":
		h.queue(w, query)
	case "history":
		h.history(w, query)
	default:
		writeError(w, fmt.Sprintf("not implemented: mode=%s", mode))
	}
}

// authenticate reports the error message to answer with, or empty to go on. The
// messages are the ones the *arrs look for by substring to tell a wrong key from
// a missing one.
func (h *Handler) authenticate(query map[string][]string) string {
	if h.config.APIKey == "" {
		return ""
	}

	keys := query["apikey"]
	if len(keys) == 0 || keys[0] == "" {
		return "API Key Required"
	}
	if keys[0] != h.config.APIKey {
		return "API Key Incorrect"
	}
	return ""
}

// configResponse is what a client validates itself against on save. Everything it
// checks is reported as off: no pre-check, no sorting, and a history retention of
// "all", which is how it learns this client does not remove completed downloads
// behind its back.
//
// Every category points at the completed directory itself, because that is where
// the files are - an add is a folder named after the nzb at the root of the mount,
// whatever it was categorised as. A category directory ending in `*` would tell
// the client job folders are off, and it refuses that.
func (h *Handler) configResponse() map[string]any {
	categories := make([]map[string]any, 0, len(h.config.Categories))
	for _, name := range h.config.Categories {
		categories = append(categories, map[string]any{
			"name":     name,
			"order":    0,
			"pp":       "",
			"script":   "None",
			"dir":      "",
			"priority": 0,
		})
	}

	return map[string]any{
		"config": map[string]any{
			"misc": map[string]any{
				"complete_dir":             h.config.CompleteDir,
				"pre_check":                false,
				"enable_tv_sorting":        false,
				"tv_categories":            []string{},
				"enable_movie_sorting":     false,
				"movie_categories":         []string{},
				"enable_date_sorting":      false,
				"date_categories":          []string{},
				"history_retention":        "0",
				"history_retention_option": "all",
				"history_retention_number": 0,
			},
			"categories": categories,
			"servers":    []any{},
			"sorters":    []any{},
		},
	}
}

// addFile takes the nzb of an `addfile` post and returns the id to track it under.
// The add itself runs in the background; the id is what the client polls with.
func (h *Handler) addFile(w http.ResponseWriter, r *http.Request, query map[string][]string) {
	if err := r.ParseMultipartForm(32 * megabyte); err != nil {
		writeError(w, fmt.Sprintf("failed reading upload: %v", err))
		return
	}

	// The form field is called `name` by the *arrs and `nzbfile` by other tools,
	// and the request carries exactly one file whatever it is called
	var header *multipart.FileHeader
	for _, headers := range r.MultipartForm.File {
		if len(headers) > 0 {
			header = headers[0]
			break
		}
	}
	if header == nil {
		writeError(w, "no nzb file in request")
		return
	}

	file, err := header.Open()
	if err != nil {
		writeError(w, fmt.Sprintf("failed opening upload: %v", err))
		return
	}
	defer file.Close()

	content, err := io.ReadAll(file)
	if err != nil {
		writeError(w, fmt.Sprintf("failed reading upload: %v", err))
		return
	}

	nzbData, err := nzbparser.ParseNzb(bytes.NewReader(content), header.Filename)
	if err != nil {
		writeError(w, fmt.Sprintf("failed parsing nzb: %v", err))
		return
	}
	if _, errs := nzbData.CheckPlausability(); len(errs) > 0 {
		writeError(w, fmt.Sprintf("implausible nzb: %v", errs[0]))
		return
	}

	id, err := h.service.Add(nzbData, first(query, "cat", "category"))
	if err != nil {
		writeError(w, err.Error())
		return
	}

	logger.Info("Accepted nzb", "id", id, "category", first(query, "cat", "category"))
	writeJSON(w, map[string]any{"status": true, "nzo_ids": []string{id}})
}

func (h *Handler) queue(w http.ResponseWriter, query map[string][]string) {
	if first(query, "name") == "delete" {
		h.delete(w, query, h.service.Cancel)
		return
	}

	items := filter(h.service.Queue(), query)
	slots := make([]map[string]any, 0, len(items))
	for i, item := range items {
		slots = append(slots, map[string]any{
			"status": queueStatus(item.Stage),
			"index":  i,
			// Nothing here knows how long an add will take: the probe is a sample
			// whose size depends on what it finds, and the archive walk is a
			// handful of segments
			"timeleft":   "0:00:00",
			"percentage": 0,
			"mb":         megabytes(item.Bytes),
			"mbleft":     megabytes(item.Bytes),
			"filename":   item.ID,
			"priority":   "Normal",
			"cat":        item.Category,
			"nzo_id":     item.ID,
		})
	}

	writeJSON(w, map[string]any{"queue": map[string]any{
		"paused": false,
		"slots":  slots,
		// Where a client older than SABnzbd 2.0 reads the completed directory
		"my_home": h.config.CompleteDir,
	}})
}

func (h *Handler) history(w http.ResponseWriter, query map[string][]string) {
	if first(query, "name") == "delete" {
		h.delete(w, query, h.service.Delete)
		return
	}

	items := filter(h.service.History(), query)
	slots := make([]map[string]any, 0, len(items))
	for _, item := range items {
		slot := map[string]any{
			"status":        historyStatus(item.Stage),
			"nzo_id":        item.ID,
			"name":          item.ID,
			"nzb_name":      item.ID + ".nzb",
			"category":      item.Category,
			"bytes":         item.Bytes,
			"fail_message":  item.Err,
			"download_time": int64(item.Finished.Sub(item.Added).Seconds()),
			"storage":       "",
		}
		// The path the client imports from. Only a completed add has one, and it
		// is the folder every file of the nzb is presented under.
		if item.Stage == nzbservice.StageCompleted {
			slot["storage"] = filepath.Join(h.config.CompleteDir, item.ID)
		}
		slots = append(slots, slot)
	}

	writeJSON(w, map[string]any{"history": map[string]any{
		"paused": false,
		"slots":  slots,
	}})
}

// delete answers `name=delete`, which the queue and the history both use with
// their own meaning of removing an item. `del_files` is not read: the record of
// an add and the files it built are one thing here, and deleting one without the
// other leaves either files nothing can report on or a report on files that are
// gone.
func (h *Handler) delete(w http.ResponseWriter, query map[string][]string, remove func(string) error) {
	for _, id := range strings.Split(first(query, "value"), ",") {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		if err := remove(id); err != nil {
			writeError(w, err.Error())
			return
		}
		logger.Info("Removed nzb on client request", "id", id)
	}

	writeJSON(w, map[string]any{"status": true})
}

// queueStatus and historyStatus name a stage the way a client reads it. A
// cancelled add is reported as `Deleted`, which both *arrs skip over: the cancel
// was their own doing or the user's, and it is not a release to blacklist.
func queueStatus(stage nzbservice.Stage) string {
	switch stage {
	case nzbservice.StageQueued:
		return "Queued"
	case nzbservice.StageChecking:
		return "Verifying"
	default:
		return "Downloading"
	}
}

func historyStatus(stage nzbservice.Stage) string {
	switch stage {
	case nzbservice.StageCompleted:
		return "Completed"
	case nzbservice.StageFailed:
		return "Failed"
	default:
		return "Deleted"
	}
}

// filter applies the `category`, `start` and `limit` parameters a client sends.
// The category matters: a client only looks at the downloads carrying the
// category it added them with, so anything else is somebody elses.
func filter(items []nzbservice.QueueItem, query map[string][]string) []nzbservice.QueueItem {
	if category := first(query, "category"); category != "" {
		kept := make([]nzbservice.QueueItem, 0, len(items))
		for _, item := range items {
			if item.Category == category {
				kept = append(kept, item)
			}
		}
		items = kept
	}

	start, _ := strconv.Atoi(first(query, "start"))
	if start > 0 && start < len(items) {
		items = items[start:]
	}
	if limit, _ := strconv.Atoi(first(query, "limit")); limit > 0 && limit < len(items) {
		items = items[:limit]
	}

	return items
}

func megabytes(bytes int64) string {
	return strconv.FormatFloat(float64(bytes)/megabyte, 'f', 2, 64)
}

func first(query map[string][]string, keys ...string) string {
	for _, key := range keys {
		if values := query[key]; len(values) > 0 {
			return values[0]
		}
	}
	return ""
}

func writeJSON(w http.ResponseWriter, body any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(body); err != nil {
		logger.Error("Failed writing response", "error", err)
	}
}

// writeError answers the way SABnzbd does: 200 with a status of false, which is
// what a client checks before it looks at anything else.
func writeError(w http.ResponseWriter, message string) {
	writeJSON(w, map[string]any{"status": false, "error": message})
}
