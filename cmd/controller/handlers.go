package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"strconv"
	"time"

	"epochd/pkg/api"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func (c *controller) routes() *http.ServeMux {
	mux := http.NewServeMux()
	mux.Handle("GET /metrics", promhttp.HandlerFor(c.met.registry, promhttp.HandlerOpts{}))
	mux.HandleFunc("GET /healthz", c.track("GET", "/healthz", handleHealthz))
	mux.HandleFunc("GET /resolve", c.track("GET", "/resolve", c.handleResolve))
	mux.HandleFunc("GET /timeshifts", c.track("GET", "/timeshifts", c.handleListTimeshifts))
	mux.HandleFunc("POST /timeshifts", c.track("POST", "/timeshifts", c.handleCreateTimeshift))
	mux.HandleFunc("GET /timeshifts/{id}", c.track("GET", "/timeshifts/{id}", c.handleGetTimeshift))
	mux.HandleFunc("GET /timeshifts/{id}/status", c.track("GET", "/timeshifts/{id}/status", c.handleTimeshiftStatus))
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

// track wraps h, recording each request in the apiRequestsTotal counter and
// logging method, path, status, and duration.
func (c *controller) track(method, path string, h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sr := &statusRecorder{ResponseWriter: w}
		h(sr, r)
		code := sr.statusCode()
		c.met.apiRequestsTotal.WithLabelValues(method, path, strconv.Itoa(code)).Inc()
		c.log.Info("http",
			"method", method,
			"path", r.URL.Path,
			"status", code,
			"duration_ms", time.Since(start).Milliseconds())
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

	s, err := c.createTimeshift(r.Context(), req.Namespace, req.LabelSelector, target, ttl, req.Freeze)
	if err != nil {
		if isNotFound(err) {
			writeError(w, http.StatusNotFound, err.Error())
			return
		}
		if isConflict(err) {
			writeError(w, http.StatusConflict, err.Error())
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
	if req.Time != "" && req.Duration != "" {
		writeError(w, http.StatusBadRequest, "provide either time or duration, not both")
		return
	}
	if req.Time == "" && req.Duration == "" {
		writeError(w, http.StatusBadRequest, "time or duration is required")
		return
	}

	var s *timeshift
	var err error
	if req.Duration != "" {
		delta, parseErr := time.ParseDuration(req.Duration)
		if parseErr != nil {
			writeError(w, http.StatusBadRequest, "duration must be a Go duration string (e.g. \"24h\"): "+parseErr.Error())
			return
		}
		s, err = c.advanceTimeshift(r.Context(), id, delta, req.Freeze)
	} else {
		target, parseErr := time.Parse(time.RFC3339, req.Time)
		if parseErr != nil {
			writeError(w, http.StatusBadRequest, "time must be RFC3339: "+parseErr.Error())
			return
		}
		s, err = c.updateTimeshift(r.Context(), id, target, req.Freeze)
	}
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

func (c *controller) handleTimeshiftStatus(w http.ResponseWriter, r *http.Request) {
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

	c.mu.RLock()
	handles := make([]containerHandle, len(s.handles))
	copy(handles, s.handles)
	c.mu.RUnlock()

	entries := make([]api.ContainerStatusEntry, len(handles))
	for i, h := range handles {
		entry := api.ContainerStatusEntry{
			Pod:       h.pod,
			Container: h.container,
			NodeIP:    h.nodeIP,
		}
		hs, err := c.agents.GetStatus(r.Context(), h.nodeIP, h.agentHandle)
		if err != nil {
			entry.Error = err.Error()
		} else {
			entry.Status = hs
		}
		entries[i] = entry
	}

	writeJSON(w, http.StatusOK, api.TimeshiftStatusResponse{
		ID:         s.id,
		Namespace:  s.namespace,
		Containers: entries,
	})
}

func (c *controller) handleResolve(w http.ResponseWriter, r *http.Request) {
	ns := r.URL.Query().Get("namespace")
	sel := r.URL.Query().Get("selector")
	if ns == "" {
		writeError(w, http.StatusBadRequest, "namespace query parameter is required")
		return
	}
	if sel == "" {
		writeError(w, http.StatusBadRequest, "selector query parameter is required")
		return
	}

	pods, err := c.k8s.CoreV1().Pods(ns).List(r.Context(), metav1.ListOptions{LabelSelector: sel})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "list pods: "+err.Error())
		return
	}

	resolved := make([]api.ResolvedPod, 0, len(pods.Items))
	for i := range pods.Items {
		pod := &pods.Items[i]
		var containers []string
		for _, cs := range pod.Status.ContainerStatuses {
			if cs.State.Running != nil {
				containers = append(containers, cs.Name)
			}
		}
		if len(containers) == 0 {
			continue
		}
		sort.Strings(containers)
		resolved = append(resolved, api.ResolvedPod{
			Name:       pod.Name,
			Namespace:  pod.Namespace,
			NodeIP:     pod.Status.HostIP,
			Containers: containers,
		})
	}
	sort.Slice(resolved, func(i, j int) bool { return resolved[i].Name < resolved[j].Name })

	writeJSON(w, http.StatusOK, api.ResolveResponse{Pods: resolved})
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

func isConflict(err error) bool {
	var ce *conflictError
	return errors.As(err, &ce)
}
