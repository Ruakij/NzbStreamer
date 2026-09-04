// Package webui is this projects own api and the page that calls it. One poll
// answers the whole page; errors are a real http status, since nothing here is
// imitating anything.
package webui

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"

	"git.ruekov.eu/ruakij/nzbStreamer/internal/service/nzbservice"
	"git.ruekov.eu/ruakij/nzbStreamer/pkg/nzbparser"
)

var logger = slog.With("Module", "WebUI")

const maxUploadBytes = 32 << 20

// Service is the part of nzbservice this api projects.
type Service interface {
	Add(nzbData *nzbparser.NzbData, category string) (string, error)
	Queue() []nzbservice.QueueItem
	History() []nzbservice.QueueItem
	Files() map[string][]string
	Cancel(id string) error
	Delete(id string) error
}

type Handler struct {
	service    Service
	components []Component
	mux        *http.ServeMux
}

func NewHandler(service Service, components ...Component) *Handler {
	h := &Handler{service: service, components: components, mux: http.NewServeMux()}
	h.mux.HandleFunc("GET /{$}", page)
	h.mux.HandleFunc("GET /api/items", h.items)
	h.mux.HandleFunc("POST /api/add", h.add)
	h.mux.HandleFunc("POST /api/remove", h.remove)
	h.mux.HandleFunc("GET /api/health", h.health)
	h.mux.HandleFunc("GET /api/health/live", h.live)
	return h
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.mux.ServeHTTP(w, r)
}

func (h *Handler) items(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, map[string]any{
		"queue":   h.service.Queue(),
		"history": h.service.History(),
		"files":   h.service.Files(),
		"stats":   map[string]any{},
	})
}

func (h *Handler) add(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseMultipartForm(maxUploadBytes); err != nil {
		writeError(w, http.StatusBadRequest, "failed reading upload: "+err.Error())
		return
	}

	file, header, err := r.FormFile("file")
	if err != nil {
		writeError(w, http.StatusBadRequest, "no nzb file in request")
		return
	}
	defer file.Close()

	content, err := io.ReadAll(file)
	if err != nil {
		writeError(w, http.StatusBadRequest, "failed reading upload: "+err.Error())
		return
	}

	nzbData, err := nzbparser.ParseNzb(bytes.NewReader(content), header.Filename)
	if err != nil {
		writeError(w, http.StatusBadRequest, "failed parsing nzb: "+err.Error())
		return
	}
	if _, errs := nzbData.CheckPlausability(); len(errs) > 0 {
		writeError(w, http.StatusBadRequest, "implausible nzb: "+errs[0].Error())
		return
	}

	id, err := h.service.Add(nzbData, r.FormValue("category"))
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	logger.Info("Accepted nzb", "id", id)
	writeJSON(w, map[string]any{"id": id})
}

// remove takes the action from the caller, because the page knows which block
// the row is in: a queued item is cancelled, a finished one is deleted.
func (h *Handler) remove(w http.ResponseWriter, r *http.Request) {
	id := r.FormValue("id")
	if id == "" {
		writeError(w, http.StatusBadRequest, "no id")
		return
	}

	var err error
	switch action := r.FormValue("action"); action {
	case "cancel":
		err = h.service.Cancel(id)
	case "delete":
		err = h.service.Delete(id)
	default:
		writeError(w, http.StatusBadRequest, "unknown action: "+action)
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

func writeJSON(w http.ResponseWriter, body any) {
	w.Header().Set("Content-Type", "application/json")
	encode(w, body)
}

func writeError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	encode(w, map[string]string{"error": message})
}

func encode(w http.ResponseWriter, body any) {
	if err := json.NewEncoder(w).Encode(body); err != nil {
		logger.Error("Failed writing response", "error", err)
	}
}
