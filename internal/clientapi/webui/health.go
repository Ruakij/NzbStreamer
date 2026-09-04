package webui

import "net/http"

// The status a component reports. disabled is what an unconfigured component
// answers: leaving it out of the body would make "not there" and "not
// configured" the same absence, and it neither degrades the aggregate nor gates.
const (
	StatusUp       = "up"
	StatusDegraded = "degraded"
	StatusDown     = "down"
	StatusDisabled = "disabled"
)

// Status is one components answer. Details is whatever an operator wants from
// it at 3am and is free-form per component.
type Status struct {
	Status  string `json:"status"`
	Details any    `json:"details,omitempty"`
}

// Component is one separately configured part of the process that can
// separately be wrong. Gates says the process cannot serve its filesystem
// without it, which is what turns a report into a 503; a dependency of the
// content rather than of the service does not gate.
//
// The handler is handed its components rather than packages registering
// themselves, so nothing reports health from the middle of code that is about
// something else.
type Component struct {
	Name   string
	Gates  bool
	Health func() Status
}

// severity orders the statuses for the aggregate. disabled is not a problem, so
// it ranks with up.
func severity(status string) int {
	switch status {
	case StatusDegraded:
		return 1
	case StatusDown:
		return 2
	default:
		return 0
	}
}

// health answers whether this process can serve its filesystem. The status code
// is gated on the components the filesystem needs, while the aggregate word
// covers all of them: a total usenet outage still lists the library and serves
// what the cache holds, so it is degraded and 200, and an alert fires on the
// word rather than on the code.
func (h *Handler) health(w http.ResponseWriter, _ *http.Request) {
	components := make(map[string]Status, len(h.components))
	aggregate, gatingOK := StatusUp, true

	for _, component := range h.components {
		status := component.Health()
		components[component.Name] = status

		if severity(status.Status) > severity(aggregate) {
			aggregate = status.Status
		}
		if component.Gates && severity(status.Status) > 0 {
			gatingOK = false
		}
	}

	code := http.StatusOK
	if !gatingOK {
		code = http.StatusServiceUnavailable
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	encode(w, map[string]any{"status": aggregate, "components": components})
}

// live answers whether this process is alive and looks at nothing else. Anything
// that can be temporarily wrong and heal on its own must never reach here, or a
// probe restarts the process over its own healing.
func (h *Handler) live(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, map[string]any{"status": StatusUp})
}
