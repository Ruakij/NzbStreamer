// Package webui is this projects own api and the page that calls it. One poll
// answers the whole page; errors are a real http status, since nothing here is
// imitating anything.
package webui

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"mime"
	"net/http"

	"git.ruekov.eu/ruakij/nzbStreamer/internal/nzbfileanalyzer"
	"git.ruekov.eu/ruakij/nzbStreamer/internal/service/nzbservice"
	"git.ruekov.eu/ruakij/nzbStreamer/pkg/nzbparser"
)

const maxUploadBytes = 32 << 20

// Service is the part of nzbservice this api projects.
type Service interface {
	Add(nzbData *nzbparser.NzbData, category string) (string, error)
	NzbRaw(id string) ([]byte, error)
	Queue() []nzbservice.QueueItem
	History() []nzbservice.QueueItem
	Files() map[string][]nzbservice.PresentedFile
	Cancel(id string) error
	Delete(id string) error
	Archive(id string, archived bool) error
}

type Handler struct {
	service    Service
	components []Component
	mux        *http.ServeMux

	// Stats answers the strip on top of the page, NzbStats the info panel of one
	// row. Both join what the service knows to what the cache and the pool know,
	// which only the composition root sees; unset, the page shows neither.
	Stats    func() any
	NzbStats func(id string) any

	// Live is what the liveness probe reads. Unset, the process answers alive as
	// long as it serves the request at all.
	Live func() bool
}

func NewHandler(service Service, components ...Component) *Handler {
	h := &Handler{service: service, components: components, mux: http.NewServeMux()}
	h.mux.HandleFunc("GET /{$}", page)
	h.mux.Handle("GET /static/", staticFiles())
	h.mux.HandleFunc("GET /api/items", h.items)
	h.mux.HandleFunc("GET /api/nzb", h.nzb)
	h.mux.HandleFunc("GET /api/nzb/file", h.nzbFile)
	h.mux.HandleFunc("POST /api/add", h.add)
	h.mux.HandleFunc("POST /api/inspect", h.inspect)
	h.mux.HandleFunc("POST /api/remove", h.remove)
	h.mux.HandleFunc("GET /api/health", h.health)
	h.mux.HandleFunc("GET /api/health/live", h.live)
	return h
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.mux.ServeHTTP(w, r)
}

func (h *Handler) items(w http.ResponseWriter, _ *http.Request) {
	stats := any(map[string]any{})
	if h.Stats != nil {
		stats = h.Stats()
	}

	writeJSON(w, map[string]any{
		"queue":   h.service.Queue(),
		"history": h.service.History(),
		"files":   h.service.Files(),
		"stats":   stats,
	})
}

// nzb answers the per-file detail of one nzb, which the page asks for when a row
// is opened rather than on every poll: it walks every segment the nzb posts.
func (h *Handler) nzb(w http.ResponseWriter, r *http.Request) {
	id := r.FormValue("id")
	if id == "" {
		writeError(w, http.StatusBadRequest, "no id")
		return
	}
	if h.NzbStats == nil {
		writeError(w, http.StatusNotFound, "no stats available")
		return
	}

	detail := h.NzbStats(id)
	if detail == nil {
		writeError(w, http.StatusNotFound, "unknown nzb: "+id)
		return
	}

	writeJSON(w, detail)
}

// nzbFile hands back the nzb an add was made from, as it was submitted. The
// bytes are what the store kept, so a record whose add failed and one a client
// archived answer as well as a completed one.
func (h *Handler) nzbFile(w http.ResponseWriter, r *http.Request) {
	id := r.FormValue("id")
	if id == "" {
		writeError(w, http.StatusBadRequest, "no id")
		return
	}

	raw, err := h.service.NzbRaw(id)
	if errors.Is(err, nzbservice.ErrNzbNotFound) {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	// The name comes from the nzb, so it is quoted and escaped rather than pasted
	// into the header
	disposition := mime.FormatMediaType("attachment", map[string]string{"filename": id + ".nzb"})
	if disposition == "" {
		disposition = "attachment"
	}

	w.Header().Set("Content-Type", "application/x-nzb")
	w.Header().Set("Content-Disposition", disposition)
	if _, err := w.Write(raw); err != nil {
		slog.Error("Failed writing nzb", "id", id, "error", err)
	}
}

// uploaded parses the nzb of a multipart request, answering the caller itself on
// anything that stops it.
func uploaded(w http.ResponseWriter, r *http.Request) *nzbparser.NzbData {
	if err := r.ParseMultipartForm(maxUploadBytes); err != nil {
		writeError(w, http.StatusBadRequest, "failed reading upload: "+err.Error())
		return nil
	}

	file, header, err := r.FormFile("file")
	if err != nil {
		writeError(w, http.StatusBadRequest, "no nzb file in request")
		return nil
	}
	defer file.Close()

	content, err := io.ReadAll(file)
	if err != nil {
		writeError(w, http.StatusBadRequest, "failed reading upload: "+err.Error())
		return nil
	}

	nzbData, err := nzbparser.ParseNzb(bytes.NewReader(content), header.Filename)
	if err != nil {
		writeError(w, http.StatusBadRequest, "failed parsing nzb: "+err.Error())
		return nil
	}

	return nzbData
}

// inspect answers what the parser makes of an upload without adding it: the
// same parse an add does, reported rather than acted on, implausible ones
// included since seeing why is the point.
func (h *Handler) inspect(w http.ResponseWriter, r *http.Request) {
	nzbData := uploaded(w, r)
	if nzbData == nil {
		return
	}

	warnings, errs := nzbData.CheckPlausability()

	// What the nzb posts, sized the way an add would size it: without the probe
	// and without what the store already measured, so an unknown convention stays
	// unknown here and every size it yields is an estimate.
	sizer := nzbfileanalyzer.NewSegmentSizer(nzbData)

	files := make([]any, 0, len(nzbData.Files))
	totalWire, totalBytes, totalSegments := 0, 0, 0
	totalExact := true
	for i := range nzbData.Files {
		file := &nzbData.Files[i]

		wire, size := 0, 0
		exact := true
		sizes := sizer.FileSizes(file)
		for i, segment := range file.Segments {
			wire += segment.BytesHint
			size += sizes[i].Size
			exact = exact && sizes[i].Exact
		}
		totalWire += wire
		totalBytes += size
		totalExact = totalExact && exact
		totalSegments += len(file.Segments)

		files = append(files, map[string]any{
			"filename":     file.Filename,
			"subject":      file.Subject,
			"poster":       file.Poster,
			"groups":       file.Groups,
			"encoding":     file.Encoding,
			"date":         file.ParsedDate,
			"wire":         wire,
			"bytes":        size,
			"exact":        exact,
			"segments":     len(file.Segments),
			"segment_hint": file.SegmentCountHint,
		})
	}

	writeJSON(w, map[string]any{
		"name":       nzbData.MetaName,
		"meta":       nzbData.Meta,
		"convention": sizer.Convention().String(),
		"wire":       totalWire,
		"bytes":      totalBytes,
		"exact":      totalExact,
		"segments":   totalSegments,
		"files":      files,
		"warnings":   messages(warnings),
		"errors":     messages(errs),
	})
}

func messages(errs []nzbparser.EncapsulatedError) []string {
	out := make([]string, len(errs))
	for i, err := range errs {
		out[i] = err.Error()
	}
	return out
}

func (h *Handler) add(w http.ResponseWriter, r *http.Request) {
	nzbData := uploaded(w, r)
	if nzbData == nil {
		return
	}

	if _, errs := nzbData.CheckPlausability(); len(errs) > 0 {
		writeError(w, http.StatusBadRequest, "implausible nzb: "+errs[0].Error())
		return
	}

	id, err := h.service.Add(nzbData, r.FormValue("category"))
	if errors.Is(err, nzbservice.ErrNzbAlreadyExists) {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if errors.Is(err, nzbservice.ErrLibraryFull) {
		writeError(w, http.StatusInsufficientStorage, err.Error())
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	slog.Info("Accepted nzb", "id", id)
	writeJSON(w, map[string]any{"id": id})
}

// remove takes the action from the caller, because the page knows which block
// the row is in: a queued item is cancelled, a finished one is deleted.
// Archiving only hides a finished one from the default listing; what it built
// stays presented.
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
	case "archive":
		err = h.service.Archive(id, true)
	case "restore":
		err = h.service.Archive(id, false)
	default:
		writeError(w, http.StatusBadRequest, "unknown action: "+action)
		return
	}
	if err != nil {
		status := http.StatusInternalServerError
		switch {
		case errors.Is(err, nzbservice.ErrNzbNotFound):
			status = http.StatusNotFound
		case errors.Is(err, nzbservice.ErrNzbStillRunning):
			status = http.StatusConflict
		}
		writeError(w, status, err.Error())
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

func writeJSON(w http.ResponseWriter, body any) {
	w.Header().Set("Content-Type", "application/json")
	encode(w, body)
}

func writeError(w http.ResponseWriter, status int, message string) {
	slog.Warn("Request failed", "status", status, "error", message)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	encode(w, map[string]string{"error": message})
}

func encode(w http.ResponseWriter, body any) {
	if err := json.NewEncoder(w).Encode(body); err != nil {
		slog.Error("Failed writing response", "error", err)
	}
}
