package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/bkaznowski/epochd/pkg/api"
)

// ---------------------------------------------------------------------------
// Fake controller server
// ---------------------------------------------------------------------------

type fakeController struct {
	mux    *http.ServeMux
	stored *api.TimeshiftResponse
	status *api.TimeshiftStatusResponse
	pods   []api.ResolvedPod
}

func newFakeController() *fakeController {
	fc := &fakeController{mux: http.NewServeMux()}
	fc.mux.HandleFunc("POST /timeshifts", fc.handleCreate)
	fc.mux.HandleFunc("GET /timeshifts", fc.handleList)
	fc.mux.HandleFunc("GET /timeshifts/{id}", fc.handleGet)
	fc.mux.HandleFunc("PATCH /timeshifts/{id}", fc.handleUpdate)
	fc.mux.HandleFunc("DELETE /timeshifts/{id}", fc.handleDelete)
	fc.mux.HandleFunc("GET /timeshifts/{id}/status", fc.handleStatus)
	fc.mux.HandleFunc("GET /resolve", fc.handleResolve)
	return fc
}

func (fc *fakeController) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	fc.mux.ServeHTTP(w, r)
}

func (fc *fakeController) handleCreate(w http.ResponseWriter, r *http.Request) {
	var req api.CreateTimeshiftRequest
	json.NewDecoder(r.Body).Decode(&req) //nolint:errcheck
	resp := &api.TimeshiftResponse{
		ID:        "abc1234",
		Namespace: req.Namespace,
		Time:      req.Time,
		Frozen:    req.Freeze,
		AppliedTo: []string{"web-abc/app"},
	}
	if req.TTL != "" {
		ttl, _ := time.ParseDuration(req.TTL)
		resp.ExpiresAt = time.Now().UTC().Add(ttl).Format(time.RFC3339)
	}
	fc.stored = resp
	fcWriteJSON(w, http.StatusCreated, resp)
}

func (fc *fakeController) handleList(w http.ResponseWriter, _ *http.Request) {
	resp := api.ListTimeshiftsResponse{Timeshifts: []api.TimeshiftResponse{}}
	if fc.stored != nil {
		resp.Timeshifts = []api.TimeshiftResponse{*fc.stored}
	}
	fcWriteJSON(w, http.StatusOK, resp)
}

func (fc *fakeController) handleGet(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if fc.stored == nil || fc.stored.ID != id {
		fcWriteErr(w, http.StatusNotFound, "not found")
		return
	}
	fcWriteJSON(w, http.StatusOK, fc.stored)
}

func (fc *fakeController) handleUpdate(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if fc.stored == nil || fc.stored.ID != id {
		fcWriteErr(w, http.StatusNotFound, "not found")
		return
	}
	var req api.UpdateTimeshiftRequest
	json.NewDecoder(r.Body).Decode(&req) //nolint:errcheck
	if req.Time != "" {
		fc.stored.Time = req.Time
	}
	fcWriteJSON(w, http.StatusOK, fc.stored)
}

func (fc *fakeController) handleDelete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if fc.stored == nil || fc.stored.ID != id {
		fcWriteErr(w, http.StatusNotFound, "not found")
		return
	}
	fc.stored = nil
	w.WriteHeader(http.StatusNoContent)
}

func (fc *fakeController) handleStatus(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if fc.status == nil || fc.status.ID != id {
		fcWriteErr(w, http.StatusNotFound, "not found")
		return
	}
	fcWriteJSON(w, http.StatusOK, fc.status)
}

func (fc *fakeController) handleResolve(w http.ResponseWriter, _ *http.Request) {
	fcWriteJSON(w, http.StatusOK, api.ResolveResponse{Pods: fc.pods})
}

func fcWriteJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v) //nolint:errcheck
}

func fcWriteErr(w http.ResponseWriter, code int, msg string) {
	fcWriteJSON(w, code, api.ErrorResponse{Error: msg})
}

// ---------------------------------------------------------------------------
// Test helpers
// ---------------------------------------------------------------------------

// startFake starts the fake controller server and returns its URL plus a cleanup func.
func startFake(t *testing.T) (*fakeController, string) {
	t.Helper()
	fc := newFakeController()
	srv := httptest.NewServer(fc)
	t.Cleanup(srv.Close)
	return fc, srv.URL
}

// capture redirects stdout/stderr to buffers for the duration of fn.
func capture(fn func()) (out, errOut string) {
	var outBuf, errBuf bytes.Buffer
	prev, prevErr := stdout, stderr
	stdout, stderr = &outBuf, &errBuf
	defer func() {
		stdout, stderr = prev, prevErr
	}()
	fn()
	return outBuf.String(), errBuf.String()
}

// mustRun asserts run(args) returns nil.
func mustRun(t *testing.T, args []string) string {
	t.Helper()
	out, _ := capture(func() {
		if err := run(args); err != nil {
			t.Fatalf("run(%v): %v", args, err)
		}
	})
	return out
}

// mustFail asserts run(args) returns a non-nil error containing wantSub.
func mustFail(t *testing.T, args []string, wantSub string) {
	t.Helper()
	capture(func() {
		err := run(args)
		if err == nil {
			t.Fatalf("run(%v): expected error, got nil", args)
		}
		if !strings.Contains(err.Error(), wantSub) {
			t.Fatalf("run(%v): error %q does not contain %q", args, err.Error(), wantSub)
		}
	})
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

func TestCmdCreate(t *testing.T) {
	_, url := startFake(t)
	out := mustRun(t, []string{"create",
		"--url=" + url,
		"--namespace=default",
		"--selector=app=web",
		"--time=2030-01-01T00:00:00Z",
	})
	if !strings.Contains(out, "created timeshift") {
		t.Errorf("output missing 'created timeshift': %q", out)
	}
	if !strings.Contains(out, "abc1234") {
		t.Errorf("output missing timeshift ID: %q", out)
	}
	if !strings.Contains(out, "web-abc/app") {
		t.Errorf("output missing applied container: %q", out)
	}
}

func TestCmdCreateMissingURL(t *testing.T) {
	mustFail(t, []string{"create",
		"--namespace=default", "--selector=app=web", "--time=2030-01-01T00:00:00Z",
	}, "EPOCHD_URL")
}

func TestCmdCreateMissingFlags(t *testing.T) {
	_, url := startFake(t)
	mustFail(t, []string{"create", "--url=" + url, "--selector=app=web", "--time=2030-01-01T00:00:00Z"}, "--namespace")
	mustFail(t, []string{"create", "--url=" + url, "--namespace=default", "--time=2030-01-01T00:00:00Z"}, "--selector")
	mustFail(t, []string{"create", "--url=" + url, "--namespace=default", "--selector=app=web"}, "--time")
}

func TestCmdList(t *testing.T) {
	fc, url := startFake(t)
	// Empty list
	out := mustRun(t, []string{"list", "--url=" + url})
	if !strings.Contains(out, "no active timeshifts") {
		t.Errorf("empty list output: %q", out)
	}

	// Create one, then list
	fc.stored = &api.TimeshiftResponse{
		ID:        "abc1234",
		Namespace: "default",
		Time:      "2030-01-01T00:00:00Z",
		AppliedTo: []string{"web-abc/app"},
	}
	out = mustRun(t, []string{"list", "--url=" + url})
	if !strings.Contains(out, "abc1234") {
		t.Errorf("list output missing ID: %q", out)
	}
	if !strings.Contains(out, "2030-01-01T00:00:00Z") {
		t.Errorf("list output missing target time: %q", out)
	}
	if !strings.Contains(out, "web-abc/app") {
		t.Errorf("list output missing applied to: %q", out)
	}
}

func TestCmdGet(t *testing.T) {
	fc, url := startFake(t)
	fc.stored = &api.TimeshiftResponse{
		ID:        "abc1234",
		Namespace: "staging",
		Time:      "2030-06-01T12:00:00Z",
		AppliedTo: []string{"api-pod/main"},
	}
	out := mustRun(t, []string{"get", "--url=" + url, "abc1234"})
	if !strings.Contains(out, "abc1234") {
		t.Errorf("get output missing ID: %q", out)
	}
	if !strings.Contains(out, "staging") {
		t.Errorf("get output missing namespace: %q", out)
	}
	if !strings.Contains(out, "api-pod/main") {
		t.Errorf("get output missing applied to: %q", out)
	}
}

func TestCmdGetNotFound(t *testing.T) {
	_, url := startFake(t)
	mustFail(t, []string{"get", "--url=" + url, "does-not-exist"}, "not found")
}

func TestCmdGetMissingID(t *testing.T) {
	_, url := startFake(t)
	mustFail(t, []string{"get", "--url=" + url}, "ID required")
}

func TestCmdUpdate(t *testing.T) {
	fc, url := startFake(t)
	fc.stored = &api.TimeshiftResponse{
		ID:        "abc1234",
		Namespace: "default",
		Time:      "2030-01-01T00:00:00Z",
		AppliedTo: []string{"web-abc/app"},
	}
	out := mustRun(t, []string{"update", "--url=" + url, "--time=2035-06-15T08:00:00Z", "abc1234"})
	if !strings.Contains(out, "updated timeshift") {
		t.Errorf("update output missing 'updated timeshift': %q", out)
	}
	if !strings.Contains(out, "2035-06-15T08:00:00Z") {
		t.Errorf("update output missing new time: %q", out)
	}
}

func TestCmdUpdateMissingTime(t *testing.T) {
	fc, url := startFake(t)
	fc.stored = &api.TimeshiftResponse{ID: "abc1234"}
	mustFail(t, []string{"update", "--url=" + url, "abc1234"}, "--time")
}

func TestCmdUpdateNotFound(t *testing.T) {
	_, url := startFake(t)
	mustFail(t, []string{"update", "--url=" + url, "--time=2030-01-01T00:00:00Z", "no-such-id"}, "not found")
}

func TestCmdDelete(t *testing.T) {
	fc, url := startFake(t)
	fc.stored = &api.TimeshiftResponse{ID: "abc1234"}
	out := mustRun(t, []string{"delete", "--url=" + url, "abc1234"})
	if !strings.Contains(out, "deleted timeshift abc1234") {
		t.Errorf("delete output: %q", out)
	}
	if fc.stored != nil {
		t.Error("stored timeshift was not removed")
	}
}

func TestCmdDeleteNotFound(t *testing.T) {
	_, url := startFake(t)
	mustFail(t, []string{"delete", "--url=" + url, "no-such-id"}, "not found")
}

func TestCmdStatus(t *testing.T) {
	fc, url := startFake(t)
	fc.status = &api.TimeshiftStatusResponse{
		ID:        "abc1234",
		Namespace: "default",
		Containers: []api.ContainerStatusEntry{
			{
				Pod:       "web-abc",
				Container: "app",
				NodeIP:    "10.0.0.1",
				Status: &api.HandleStatus{
					Generation: 3,
					LastTarget: "2030-01-01T00:00:00Z",
					PID:        12345,
				},
			},
			{
				Pod:       "web-def",
				Container: "app",
				NodeIP:    "10.0.0.2",
				Error:     "rpc timeout",
			},
		},
	}
	out := mustRun(t, []string{"status", "--url=" + url, "abc1234"})
	if !strings.Contains(out, "abc1234") {
		t.Errorf("status output missing ID: %q", out)
	}
	if !strings.Contains(out, "web-abc") {
		t.Errorf("status output missing pod: %q", out)
	}
	if !strings.Contains(out, "12345") {
		t.Errorf("status output missing PID: %q", out)
	}
	if !strings.Contains(out, "rpc timeout") {
		t.Errorf("status output missing error: %q", out)
	}
	// Error row should show dashes for missing fields
	if !strings.Contains(out, "web-def") {
		t.Errorf("status output missing error pod: %q", out)
	}
}

func TestCmdStatusNotFound(t *testing.T) {
	_, url := startFake(t)
	mustFail(t, []string{"status", "--url=" + url, "no-such-id"}, "not found")
}

func TestCmdResolve(t *testing.T) {
	fc, url := startFake(t)
	fc.pods = []api.ResolvedPod{
		{Name: "web-abc", Namespace: "default", NodeIP: "10.0.0.1", Containers: []string{"app", "sidecar"}},
	}
	out := mustRun(t, []string{"resolve", "--url=" + url, "--namespace=default", "--selector=app=web"})
	if !strings.Contains(out, "web-abc") {
		t.Errorf("resolve output missing pod: %q", out)
	}
	if !strings.Contains(out, "10.0.0.1") {
		t.Errorf("resolve output missing node IP: %q", out)
	}
	if !strings.Contains(out, "app, sidecar") {
		t.Errorf("resolve output missing containers: %q", out)
	}
}

func TestCmdResolveEmpty(t *testing.T) {
	_, url := startFake(t)
	out := mustRun(t, []string{"resolve", "--url=" + url, "--namespace=default", "--selector=app=web"})
	if !strings.Contains(out, "no matching pods") {
		t.Errorf("empty resolve output: %q", out)
	}
}

func TestUnknownCommand(t *testing.T) {
	mustFail(t, []string{"frobnicate"}, "unknown command")
}

func TestHelpShowsUsage(t *testing.T) {
	out, _ := capture(func() {
		if err := run([]string{"help"}); err != nil {
			t.Fatalf("help: %v", err)
		}
	})
	if !strings.Contains(out, "create") || !strings.Contains(out, "status") {
		t.Errorf("help output missing subcommands: %q", out)
	}
}

func TestCmdAdvance(t *testing.T) {
	fc, url := startFake(t)
	fc.stored = &api.TimeshiftResponse{
		ID:        "abc1234",
		Namespace: "default",
		Time:      "2030-01-01T00:00:00Z",
		AppliedTo: []string{"web-abc/app"},
	}
	out := mustRun(t, []string{"advance", "--url=" + url, "--by=24h", "abc1234"})
	if !strings.Contains(out, "advanced") {
		t.Errorf("advance output missing 'advanced': %q", out)
	}
	if !strings.Contains(out, "abc1234") {
		t.Errorf("advance output missing ID: %q", out)
	}
}

func TestCmdAdvanceMissingBy(t *testing.T) {
	fc, url := startFake(t)
	fc.stored = &api.TimeshiftResponse{ID: "abc1234", Time: "2030-01-01T00:00:00Z"}
	mustFail(t, []string{"advance", "--url=" + url, "abc1234"}, "--by")
}

func TestCmdAdvanceMissingID(t *testing.T) {
	_, url := startFake(t)
	mustFail(t, []string{"advance", "--url=" + url, "--by=24h"}, "ID required")
}

func TestCmdAdvanceNotFound(t *testing.T) {
	_, url := startFake(t)
	mustFail(t, []string{"advance", "--url=" + url, "--by=24h", "no-such-id"}, "not found")
}

func TestCmdCreateFrozen(t *testing.T) {
	_, url := startFake(t)
	out := mustRun(t, []string{"create",
		"--url=" + url,
		"--namespace=default",
		"--selector=app=web",
		"--time=2030-01-01T00:00:00Z",
		"--freeze",
	})
	if !strings.Contains(out, "created timeshift") {
		t.Errorf("output missing 'created timeshift': %q", out)
	}
}
