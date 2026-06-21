package sdk_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"epochd/pkg/api"
	"epochd/pkg/sdk"
)

// ---------------------------------------------------------------------------
// Fake controller server
// ---------------------------------------------------------------------------

// fakeServer is a minimal in-process HTTP server that mimics the controller.
// It stores one timeshift at a time and is sufficient to test the SDK client.
type fakeServer struct {
	mux    *http.ServeMux
	stored *api.TimeshiftResponse // nil = not found
}

func newFakeServer() *fakeServer {
	fs := &fakeServer{mux: http.NewServeMux()}
	fs.mux.HandleFunc("GET /timeshifts", fs.handleList)
	fs.mux.HandleFunc("POST /timeshifts", fs.handleCreate)
	fs.mux.HandleFunc("GET /timeshifts/{id}", fs.handleGet)
	fs.mux.HandleFunc("PATCH /timeshifts/{id}", fs.handleUpdate)
	fs.mux.HandleFunc("DELETE /timeshifts/{id}", fs.handleDelete)
	return fs
}

func (fs *fakeServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	fs.mux.ServeHTTP(w, r)
}

func (fs *fakeServer) handleList(w http.ResponseWriter, r *http.Request) {
	resp := api.ListTimeshiftsResponse{}
	if fs.stored != nil {
		resp.Timeshifts = []api.TimeshiftResponse{*fs.stored}
	} else {
		resp.Timeshifts = []api.TimeshiftResponse{}
	}
	writeJSON(w, http.StatusOK, resp)
}

func (fs *fakeServer) handleCreate(w http.ResponseWriter, r *http.Request) {
	var req api.CreateTimeshiftRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad json")
		return
	}
	if req.Namespace == "" || req.LabelSelector == "" {
		writeErr(w, http.StatusBadRequest, "namespace and labelSelector required")
		return
	}
	resp := &api.TimeshiftResponse{
		ID:        "test-id-1234",
		Namespace: req.Namespace,
		Time:      req.Time,
		AppliedTo: []string{"pod-a/main"},
	}
	if req.TTL != "" {
		ttl, _ := time.ParseDuration(req.TTL)
		resp.ExpiresAt = time.Now().UTC().Add(ttl).Format(time.RFC3339)
	}
	fs.stored = resp
	writeJSON(w, http.StatusCreated, fs.stored)
}

func (fs *fakeServer) handleGet(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if fs.stored == nil || fs.stored.ID != id {
		writeErr(w, http.StatusNotFound, "not found")
		return
	}
	writeJSON(w, http.StatusOK, fs.stored)
}

func (fs *fakeServer) handleUpdate(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if fs.stored == nil || fs.stored.ID != id {
		writeErr(w, http.StatusNotFound, "not found")
		return
	}
	var req api.UpdateTimeshiftRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad json")
		return
	}
	fs.stored.Time = req.Time
	writeJSON(w, http.StatusOK, fs.stored)
}

func (fs *fakeServer) handleDelete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if fs.stored == nil || fs.stored.ID != id {
		writeErr(w, http.StatusNotFound, "not found")
		return
	}
	fs.stored = nil
	w.WriteHeader(http.StatusNoContent)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v) //nolint:errcheck
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, api.ErrorResponse{Error: msg})
}

// startFake starts the fake server and returns the SDK client pointing at it.
func startFake(t *testing.T) (*sdk.Client, *fakeServer) {
	t.Helper()
	fs := newFakeServer()
	srv := httptest.NewServer(fs)
	t.Cleanup(srv.Close)
	return sdk.NewClient(srv.URL), fs
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

func TestCreateTimeshift(t *testing.T) {
	client, _ := startFake(t)
	target := time.Now().Add(24 * time.Hour).UTC().Truncate(time.Second)

	ts, err := client.CreateTimeshift(context.Background(), "default", "app=svc", target, time.Hour)
	if err != nil {
		t.Fatalf("CreateTimeshift: %v", err)
	}
	if ts.ID == "" {
		t.Error("expected non-empty ID")
	}
	if !ts.Time.Equal(target) {
		t.Errorf("Time: got %v want %v", ts.Time, target)
	}
	if ts.Namespace != "default" {
		t.Errorf("Namespace: got %q want %q", ts.Namespace, "default")
	}
	if len(ts.AppliedTo) == 0 {
		t.Error("expected AppliedTo to be non-empty")
	}
}

func TestGetTimeshift(t *testing.T) {
	client, _ := startFake(t)
	target := time.Now().Add(24 * time.Hour).UTC().Truncate(time.Second)

	created, err := client.CreateTimeshift(context.Background(), "default", "app=svc", target, time.Hour)
	if err != nil {
		t.Fatalf("CreateTimeshift: %v", err)
	}

	got, err := client.GetTimeshift(context.Background(), created.ID)
	if err != nil {
		t.Fatalf("GetTimeshift: %v", err)
	}
	if got.ID != created.ID {
		t.Errorf("ID mismatch: got %q want %q", got.ID, created.ID)
	}
}

func TestGetTimeshiftNotFound(t *testing.T) {
	client, _ := startFake(t)

	_, err := client.GetTimeshift(context.Background(), "no-such-id")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !sdk.IsNotFound(err) {
		t.Errorf("expected IsNotFound, got %v", err)
	}
}

func TestUpdateTimeshift(t *testing.T) {
	client, _ := startFake(t)
	target := time.Now().Add(24 * time.Hour).UTC().Truncate(time.Second)

	created, err := client.CreateTimeshift(context.Background(), "default", "app=svc", target, time.Hour)
	if err != nil {
		t.Fatalf("CreateTimeshift: %v", err)
	}

	newTarget := time.Now().Add(48 * time.Hour).UTC().Truncate(time.Second)
	updated, err := client.UpdateTimeshift(context.Background(), created.ID, newTarget)
	if err != nil {
		t.Fatalf("UpdateTimeshift: %v", err)
	}
	if !updated.Time.Equal(newTarget) {
		t.Errorf("Time after update: got %v want %v", updated.Time, newTarget)
	}
}

func TestDeleteTimeshift(t *testing.T) {
	client, _ := startFake(t)
	target := time.Now().Add(24 * time.Hour).UTC().Truncate(time.Second)

	created, err := client.CreateTimeshift(context.Background(), "default", "app=svc", target, time.Hour)
	if err != nil {
		t.Fatalf("CreateTimeshift: %v", err)
	}

	if err := client.DeleteTimeshift(context.Background(), created.ID); err != nil {
		t.Fatalf("DeleteTimeshift: %v", err)
	}

	_, err = client.GetTimeshift(context.Background(), created.ID)
	if !sdk.IsNotFound(err) {
		t.Errorf("expected NotFound after delete, got %v", err)
	}
}

func TestDeleteTimeshiftNotFound(t *testing.T) {
	client, _ := startFake(t)

	err := client.DeleteTimeshift(context.Background(), "ghost")
	if !sdk.IsNotFound(err) {
		t.Errorf("expected IsNotFound, got %v", err)
	}
}

func TestAPIError(t *testing.T) {
	client, _ := startFake(t)

	_, err := client.GetTimeshift(context.Background(), "missing")
	if err == nil {
		t.Fatal("expected APIError, got nil")
	}
	ae, ok := err.(*sdk.APIError)
	if !ok {
		t.Fatalf("expected *sdk.APIError, got %T: %v", err, err)
	}
	if ae.StatusCode != http.StatusNotFound {
		t.Errorf("StatusCode: got %d want 404", ae.StatusCode)
	}
	if ae.Message == "" {
		t.Error("expected non-empty Message")
	}
}

func TestAPIErrorString(t *testing.T) {
	ae := &sdk.APIError{StatusCode: 404, Message: "not found"}
	if !strings.Contains(ae.Error(), "404") {
		t.Errorf("Error() should contain status code: %q", ae.Error())
	}
}

func TestWithTime(t *testing.T) {
	client, fs := startFake(t)
	target := time.Now().Add(24 * time.Hour).UTC().Truncate(time.Second)

	var capturedID string
	err := sdk.WithTime(context.Background(), client, "default", "app=svc", target, time.Hour,
		func(ctx context.Context, ts *sdk.Timeshift) error {
			capturedID = ts.ID
			if !ts.Time.Equal(target) {
				t.Errorf("inside WithTime: Time %v != %v", ts.Time, target)
			}
			return nil
		},
	)
	if err != nil {
		t.Fatalf("WithTime: %v", err)
	}
	// timeshift must be deleted after fn returns
	if fs.stored != nil && fs.stored.ID == capturedID {
		t.Error("timeshift was not deleted after WithTime")
	}
}

func TestWithTimeCleansUpOnFnError(t *testing.T) {
	client, fs := startFake(t)
	target := time.Now().Add(24 * time.Hour).UTC().Truncate(time.Second)

	sentinelErr := fmt.Errorf("test assertion failed")
	err := sdk.WithTime(context.Background(), client, "default", "app=svc", target, time.Hour,
		func(ctx context.Context, ts *sdk.Timeshift) error {
			return sentinelErr
		},
	)
	if err == nil {
		t.Fatal("expected error from WithTime, got nil")
	}
	if !strings.Contains(err.Error(), sentinelErr.Error()) {
		t.Errorf("expected fn error in WithTime error: %v", err)
	}
	// timeshift must still be deleted even though fn failed
	if fs.stored != nil {
		t.Error("timeshift was not deleted after fn error")
	}
}

func TestListTimeshiftsEmpty(t *testing.T) {
	client, _ := startFake(t)

	list, err := client.ListTimeshifts(context.Background())
	if err != nil {
		t.Fatalf("ListTimeshifts: %v", err)
	}
	if len(list) != 0 {
		t.Errorf("expected empty list, got %d entries", len(list))
	}
}

func TestListTimeshiftsAfterCreate(t *testing.T) {
	client, _ := startFake(t)
	target := time.Now().Add(24 * time.Hour).UTC().Truncate(time.Second)

	created, err := client.CreateTimeshift(context.Background(), "default", "app=svc", target, time.Hour)
	if err != nil {
		t.Fatalf("CreateTimeshift: %v", err)
	}

	list, err := client.ListTimeshifts(context.Background())
	if err != nil {
		t.Fatalf("ListTimeshifts: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(list))
	}
	if list[0].ID != created.ID {
		t.Errorf("ID mismatch: got %q want %q", list[0].ID, created.ID)
	}
	if !list[0].Time.Equal(target) {
		t.Errorf("Time mismatch: got %v want %v", list[0].Time, target)
	}
}

func TestListTimeshiftsAfterDelete(t *testing.T) {
	client, _ := startFake(t)
	target := time.Now().Add(24 * time.Hour).UTC().Truncate(time.Second)

	created, err := client.CreateTimeshift(context.Background(), "default", "app=svc", target, time.Hour)
	if err != nil {
		t.Fatalf("CreateTimeshift: %v", err)
	}
	if err := client.DeleteTimeshift(context.Background(), created.ID); err != nil {
		t.Fatalf("DeleteTimeshift: %v", err)
	}

	list, err := client.ListTimeshifts(context.Background())
	if err != nil {
		t.Fatalf("ListTimeshifts after delete: %v", err)
	}
	if len(list) != 0 {
		t.Errorf("expected empty list after delete, got %d entries", len(list))
	}
}

func TestCreateTimeshiftNoTTL(t *testing.T) {
	client, _ := startFake(t)
	target := time.Now().Add(24 * time.Hour).UTC().Truncate(time.Second)

	ts, err := client.CreateTimeshift(context.Background(), "default", "app=svc", target, 0)
	if err != nil {
		t.Fatalf("CreateTimeshift with no TTL: %v", err)
	}
	if !ts.ExpiresAt.IsZero() {
		t.Errorf("ExpiresAt should be zero for no-TTL timeshift, got %v", ts.ExpiresAt)
	}
}

func TestWithTimeRejectsZeroTTL(t *testing.T) {
	client, _ := startFake(t)
	target := time.Now().Add(24 * time.Hour).UTC().Truncate(time.Second)

	err := sdk.WithTime(context.Background(), client, "default", "app=svc", target, 0,
		func(ctx context.Context, ts *sdk.Timeshift) error { return nil },
	)
	if err == nil {
		t.Fatal("expected error for zero TTL in WithTime, got nil")
	}
}

func TestWithTimeTSkipsWithoutEnvVar(t *testing.T) {
	t.Setenv("EPOCHD_URL", "") // ensure unset
	client, _ := startFake(t)
	target := time.Now().Add(24 * time.Hour).UTC().Truncate(time.Second)

	ran := false
	// WithTimeT calls t.Skip on the sub-test's T, so the fn body must not run.
	t.Run("inner", func(t *testing.T) {
		sdk.WithTimeT(t, client, "default", "app=svc", target, time.Hour,
			func(t *testing.T, ts *sdk.Timeshift) {
				ran = true
			},
		)
	})
	if ran {
		t.Error("WithTimeT fn body should not run when EPOCHD_URL is unset")
	}
}
