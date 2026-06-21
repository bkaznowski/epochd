package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"epochd/pkg/api"

	"github.com/prometheus/client_golang/prometheus/promhttp"
)

func (c *controller) routes() *http.ServeMux {
	mux := http.NewServeMux()
	mux.Handle("GET /metrics", promhttp.HandlerFor(c.met.registry, promhttp.HandlerOpts{}))
	mux.HandleFunc("GET /healthz", c.track("GET", "/healthz", handleHealthz))
	mux.HandleFunc("GET /timeshifts", c.track("GET", "/timeshifts", c.handleListTimeshifts))
	mux.HandleFunc("POST /timeshifts", c.track("POST", "/timeshifts", c.handleCreateTimeshift))
	mux.HandleFunc("GET /timeshifts/{id}", c.track("GET", "/timeshifts/{id}", c.handleGetTimeshift))
	mux.HandleFunc("PATCH /timeshifts/{id}", c.track("PATCH", "/timeshifts/{id}", c.handleUpdateTimeshift))
	mux.HandleFunc("DELETE /timeshifts/{id}", c.track("DELETE", "/timeshifts/{id}", c.handleDeleteTimeshift))
	return mux
}

// statusRecorder captures the HTTP status code written by a handler.
type statusRecorder struct {
	http.ResponseWriter
	code int
}

func (sr *statusRecorder) WriteHeader(code int) {
	if sr.code == 0 {
		sr.code = code
	}
	sr.ResponseWriter.WriteHeader(code)
}

func (sr *statusRecorder) statusCode() int {
	if sr.code == 0 {
		return http.StatusOK
	}
	return sr.code
}

// track wraps h, recording each request in the apiRequestsTotal counter.
func (c *controller) track(method, path string, h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sr := &statusRecorder{ResponseWriter: w}
		h(sr, r)
		c.met.apiRequestsTotal.WithLabelValues(method, path, strconv.Itoa(sr.statusCode())).Inc()
	}
}

func handleHealthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (c *controller) handleListTimeshifts(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, api.ListTimeshiftsResponse{
		Timeshifts: c.listTimeshifts(),
	})
}

func (c *controller) handleCreateTimeshift(w http.ResponseWriter, r *http.Request) {
	var req api.CreateTimeshiftRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if req.Namespace == "" {
		writeError(w, http.StatusBadRequest, "namespace is required")
		return
	}
	if req.LabelSelector == "" {
		writeError(w, http.StatusBadRequest, "labelSelector is required")
		return
	}
	target, err := time.Parse(time.RFC3339, req.Time)
	if err != nil {
		writeError(w, http.StatusBadRequest, "time must be RFC3339: "+err.Error())
		return
	}
	var ttl time.Duration
	if req.TTL != "" {
		var err error
		ttl, err = time.ParseDuration(req.TTL)
		if err != nil || ttl <= 0 {
			writeError(w, http.StatusBadRequest, "ttl must be a positive Go duration (e.g. \"1h\") or omitted for no expiry")
			return
		}
	}

	s, err := c.createTimeshift(r.Context(), req.Namespace, req.LabelSelector, target, ttl)
	if err != nil {
		if isNotFound(err) {
			writeError(w, http.StatusNotFound, err.Error())
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, s.toResponse())
}

func (c *controller) handleGetTimeshift(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	s, err := c.getTimeshift(id)
	if err != nil {
		if isNotFound(err) {
			writeError(w, http.StatusNotFound, err.Error())
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, s.toResponse())
}

func (c *controller) handleUpdateTimeshift(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	var req api.UpdateTimeshiftRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	target, err := time.Parse(time.RFC3339, req.Time)
	if err != nil {
		writeError(w, http.StatusBadRequest, "time must be RFC3339: "+err.Error())
		return
	}

	s, err := c.updateTimeshift(r.Context(), id, target)
	if err != nil {
		if isNotFound(err) {
			writeError(w, http.StatusNotFound, err.Error())
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, s.toResponse())
}

func (c *controller) handleDeleteTimeshift(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := c.deleteTimeshift(r.Context(), id); err != nil {
		if isNotFound(err) {
			writeError(w, http.StatusNotFound, err.Error())
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---------------------------------------------------------------------------

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v) //nolint:errcheck
}

func writeError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, api.ErrorResponse{Error: msg})
}

func isNotFound(err error) bool {
	var nf *notFoundError
	return errors.As(err, &nf)
}
