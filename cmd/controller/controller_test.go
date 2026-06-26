package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"epochd/pkg/api"
	applog "epochd/pkg/log"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/tools/record"
	"google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"
)

// ---------------------------------------------------------------------------
// Mock AgentPool
// ---------------------------------------------------------------------------

type mockAgentPool struct {
	injectFn    func(ctx context.Context, nodeIP, containerID string, target time.Time, freeze bool) (string, error)
	setTimeFn   func(ctx context.Context, nodeIP, handleID string, target time.Time, freeze bool) error
	resetFn     func(ctx context.Context, nodeIP, handleID string) error
	getStatusFn func(ctx context.Context, nodeIP, handleID string) (*api.HandleStatus, error)
}

func (m *mockAgentPool) Inject(ctx context.Context, nodeIP, containerID string, target time.Time, freeze bool) (string, error) {
	if m.injectFn != nil {
		return m.injectFn(ctx, nodeIP, containerID, target, freeze)
	}
	return "handle-" + containerID[:8], nil
}

func (m *mockAgentPool) SetTime(ctx context.Context, nodeIP, handleID string, target time.Time, freeze bool) error {
	if m.setTimeFn != nil {
		return m.setTimeFn(ctx, nodeIP, handleID, target, freeze)
	}
	return nil
}

func (m *mockAgentPool) Reset(ctx context.Context, nodeIP, handleID string) error {
	if m.resetFn != nil {
		return m.resetFn(ctx, nodeIP, handleID)
	}
	return nil
}

func (m *mockAgentPool) GetStatus(ctx context.Context, nodeIP, handleID string) (*api.HandleStatus, error) {
	if m.getStatusFn != nil {
		return m.getStatusFn(ctx, nodeIP, handleID)
	}
	return &api.HandleStatus{
		Generation: 0,
		LastTarget: time.Now().UTC().Format(time.RFC3339),
		StateAddr:  "0x0",
		PID:        1,
	}, nil
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func makePod(name, ns, nodeIP, containerID string) corev1.Pod {
	return corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: ns,
			Labels:    map[string]string{"app": name},
		},
		Status: corev1.PodStatus{
			HostIP: nodeIP,
			ContainerStatuses: []corev1.ContainerStatus{
				{
					Name:        "main",
					ContainerID: containerID,
					State:       corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
				},
			},
		},
	}
}

func newTestController(t *testing.T, pods ...corev1.Pod) (*controller, *mockAgentPool) {
	t.Helper()
	k8s := fake.NewClientset()
	for i := range pods {
		if _, err := k8s.CoreV1().Pods(pods[i].Namespace).Create(context.Background(), &pods[i], metav1.CreateOptions{}); err != nil {
			t.Fatalf("create pod: %v", err)
		}
	}
	pool := &mockAgentPool{}
	return newController(k8s, pool, nil, applog.Discard()), pool
}

func doRequest(t *testing.T, handler http.Handler, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			t.Fatalf("encode body: %v", err)
		}
	}
	r := httptest.NewRequest(method, path, &buf)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	return w
}

func decodeResponse(t *testing.T, w *httptest.ResponseRecorder, v any) {
	t.Helper()
	if err := json.NewDecoder(w.Body).Decode(v); err != nil {
		t.Fatalf("decode response: %v (body: %s)", err, w.Body.String())
	}
}

// ---------------------------------------------------------------------------
// Controller unit tests
// ---------------------------------------------------------------------------

func TestCreateTimeshift(t *testing.T) {
	pod := makePod("web-1", "default", "10.0.0.1", "containerd://aabbcc112233")
	ctrl, _ := newTestController(t, pod)

	target := time.Now().Add(24 * time.Hour).UTC().Truncate(time.Second)
	s, err := ctrl.createTimeshift(context.Background(), "default", "app=web-1", target, time.Hour, false)
	if err != nil {
		t.Fatalf("createTimeshift: %v", err)
	}
	if s.id == "" {
		t.Error("expected non-empty ID")
	}
	if len(s.handles) == 0 {
		t.Error("expected at least one handle")
	}
	if !s.targetTime.Equal(target) {
		t.Errorf("targetTime: got %v want %v", s.targetTime, target)
	}
}

func TestCreateTimeshiftNoMatchingPods(t *testing.T) {
	ctrl, _ := newTestController(t)
	_, err := ctrl.createTimeshift(context.Background(), "default", "app=ghost", time.Now().Add(time.Hour), time.Hour, false)
	if err == nil {
		t.Fatal("expected error for no matching pods")
	}
	if !isNotFound(err) {
		t.Errorf("expected notFoundError, got %T: %v", err, err)
	}
}

func TestCreateTimeshiftConflictSamePod(t *testing.T) {
	pod := makePod("web-1", "default", "10.0.0.1", "containerd://aabbcc112233")
	ctrl, _ := newTestController(t, pod)

	target := time.Now().Add(24 * time.Hour)
	if _, err := ctrl.createTimeshift(context.Background(), "default", "app=web-1", target, 0, false); err != nil {
		t.Fatalf("first createTimeshift: %v", err)
	}

	// Second timeshift targeting the same pod must be rejected.
	_, err := ctrl.createTimeshift(context.Background(), "default", "app=web-1", target.Add(time.Hour), 0, false)
	if err == nil {
		t.Fatal("expected conflict error, got nil")
	}
	var ce *conflictError
	if !errors.As(err, &ce) {
		t.Errorf("expected conflictError, got %T: %v", err, err)
	}
	if len(ce.entries) != 1 {
		t.Errorf("expected 1 conflict entry, got %d", len(ce.entries))
	}
	if ce.entries[0].pod != "web-1" || ce.entries[0].container != "main" {
		t.Errorf("unexpected conflict entry: %+v", ce.entries[0])
	}
}

func TestCreateTimeshiftConflictPartialOverlap(t *testing.T) {
	podA := makePod("web-a", "default", "10.0.0.1", "containerd://aaaa00000000")
	podB := makePod("web-b", "default", "10.0.0.2", "containerd://bbbb00000000")
	ctrl, _ := newTestController(t, podA, podB)

	target := time.Now().Add(24 * time.Hour)

	// First timeshift covers web-a only.
	if _, err := ctrl.createTimeshift(context.Background(), "default", "app=web-a", target, 0, false); err != nil {
		t.Fatalf("first createTimeshift: %v", err)
	}

	// Second timeshift uses a broader selector that overlaps web-a â€” must conflict.
	_, err := ctrl.createTimeshift(context.Background(), "default", "app in (web-a,web-b)", target, 0, false)
	if err == nil {
		t.Fatal("expected conflict error, got nil")
	}
	if !isConflict(err) {
		t.Errorf("expected conflictError, got %T: %v", err, err)
	}
}

func TestCreateTimeshiftNoConflictAfterDelete(t *testing.T) {
	pod := makePod("web-1", "default", "10.0.0.1", "containerd://aabbcc112233")
	ctrl, _ := newTestController(t, pod)

	target := time.Now().Add(24 * time.Hour)
	s, err := ctrl.createTimeshift(context.Background(), "default", "app=web-1", target, 0, false)
	if err != nil {
		t.Fatalf("first createTimeshift: %v", err)
	}

	if err := ctrl.deleteTimeshift(context.Background(), s.id); err != nil {
		t.Fatalf("deleteTimeshift: %v", err)
	}

	// After deletion, the same pod must be targetable again.
	if _, err := ctrl.createTimeshift(context.Background(), "default", "app=web-1", target, 0, false); err != nil {
		t.Fatalf("second createTimeshift after delete: %v", err)
	}
}

func TestHTTPCreateTimeshiftConflict(t *testing.T) {
	pod := makePod("web-1", "default", "10.0.0.1", "containerd://aabbcc112233")
	ctrl, _ := newTestController(t, pod)
	mux := ctrl.routes()

	target := time.Now().Add(24 * time.Hour).UTC().Truncate(time.Second)
	body := api.CreateTimeshiftRequest{
		Namespace:     "default",
		LabelSelector: "app=web-1",
		Time:          target.Format(time.RFC3339),
	}

	// First request succeeds.
	w := doRequest(t, mux, http.MethodPost, "/timeshifts", body)
	if w.Code != http.StatusCreated {
		t.Fatalf("first POST /timeshifts: got %d, want 201 (body: %s)", w.Code, w.Body.String())
	}

	// Second request for the same pod returns 409.
	w = doRequest(t, mux, http.MethodPost, "/timeshifts", body)
	if w.Code != http.StatusConflict {
		t.Fatalf("second POST /timeshifts: got %d, want 409 (body: %s)", w.Code, w.Body.String())
	}
	var errResp api.ErrorResponse
	decodeResponse(t, w, &errResp)
	if !strings.Contains(errResp.Error, "already have an active timeshift") {
		t.Errorf("unexpected error message: %q", errResp.Error)
	}
}

func TestGetTimeshiftNotFound(t *testing.T) {
	ctrl, _ := newTestController(t)
	_, err := ctrl.getTimeshift("deadbeef")
	if !isNotFound(err) {
		t.Errorf("expected notFoundError, got %v", err)
	}
}

func TestUpdateTimeshift(t *testing.T) {
	pod := makePod("web-1", "default", "10.0.0.1", "containerd://aabbcc112233")
	ctrl, pool := newTestController(t, pod)

	called := 0
	pool.setTimeFn = func(_ context.Context, _, _ string, _ time.Time, _ bool) error {
		called++
		return nil
	}

	s, err := ctrl.createTimeshift(context.Background(), "default", "app=web-1", time.Now().Add(time.Hour), time.Hour, false)
	if err != nil {
		t.Fatalf("createTimeshift: %v", err)
	}
	newTarget := time.Now().Add(48 * time.Hour)
	s2, err := ctrl.updateTimeshift(context.Background(), s.id, newTarget, false)
	if err != nil {
		t.Fatalf("updateTimeshift: %v", err)
	}
	if called != len(s.handles) {
		t.Errorf("SetTime called %d times, want %d", called, len(s.handles))
	}
	if !s2.targetTime.Equal(newTarget) {
		t.Errorf("targetTime not updated: got %v", s2.targetTime)
	}
}

func TestDeleteTimeshift(t *testing.T) {
	pod := makePod("web-1", "default", "10.0.0.1", "containerd://aabbcc112233")
	ctrl, pool := newTestController(t, pod)

	resetCalled := 0
	pool.resetFn = func(_ context.Context, _, _ string) error {
		resetCalled++
		return nil
	}

	s, err := ctrl.createTimeshift(context.Background(), "default", "app=web-1", time.Now().Add(time.Hour), time.Hour, false)
	if err != nil {
		t.Fatalf("createTimeshift: %v", err)
	}

	if err := ctrl.deleteTimeshift(context.Background(), s.id); err != nil {
		t.Fatalf("deleteTimeshift: %v", err)
	}
	if resetCalled != len(s.handles) {
		t.Errorf("Reset called %d times, want %d", resetCalled, len(s.handles))
	}
	if _, err := ctrl.getTimeshift(s.id); !isNotFound(err) {
		t.Error("expected skew to be gone after delete")
	}
}

func TestDeleteTimeshiftNotFound(t *testing.T) {
	ctrl, _ := newTestController(t)
	err := ctrl.deleteTimeshift(context.Background(), "nope")
	if !isNotFound(err) {
		t.Errorf("expected notFoundError, got %v", err)
	}
}

func TestSweepExpired(t *testing.T) {
	pod := makePod("web-1", "default", "10.0.0.1", "containerd://aabbcc112233")
	ctrl, pool := newTestController(t, pod)

	resetCalled := 0
	pool.resetFn = func(_ context.Context, _, _ string) error {
		resetCalled++
		return nil
	}

	s, err := ctrl.createTimeshift(context.Background(), "default", "app=web-1", time.Now().Add(time.Hour), time.Millisecond, false)
	if err != nil {
		t.Fatalf("createTimeshift: %v", err)
	}
	// backdate expiresAt so it looks expired
	ctrl.mu.Lock()
	ctrl.timeshifts[s.id].expiresAt = time.Now().Add(-time.Second)
	ctrl.mu.Unlock()

	ctrl.sweepExpired(context.Background())

	if resetCalled == 0 {
		t.Error("expected Reset to be called for expired skew")
	}
	if _, err := ctrl.getTimeshift(s.id); !isNotFound(err) {
		t.Error("expected expired skew to be removed")
	}
}

// ---------------------------------------------------------------------------
// HTTP handler tests
// ---------------------------------------------------------------------------

func TestHTTPCreateTimeshift(t *testing.T) {
	pod := makePod("web-1", "default", "10.0.0.1", "containerd://aabbcc112233")
	ctrl, _ := newTestController(t, pod)
	mux := ctrl.routes()

	target := time.Now().Add(24 * time.Hour).UTC().Truncate(time.Second)
	w := doRequest(t, mux, http.MethodPost, "/timeshifts", api.CreateTimeshiftRequest{
		Namespace:     "default",
		LabelSelector: "app=web-1",
		Time:          target.Format(time.RFC3339),
		TTL:           "1h",
	})

	if w.Code != http.StatusCreated {
		t.Fatalf("got %d want 201; body: %s", w.Code, w.Body.String())
	}
	var resp api.TimeshiftResponse
	decodeResponse(t, w, &resp)
	if resp.ID == "" {
		t.Error("expected non-empty ID in response")
	}
	if len(resp.AppliedTo) == 0 {
		t.Error("expected AppliedTo to be non-empty")
	}
}

func TestHTTPCreateTimeshiftBadJSON(t *testing.T) {
	ctrl, _ := newTestController(t)
	r := httptest.NewRequest(http.MethodPost, "/timeshifts", bytes.NewBufferString("not-json"))
	w := httptest.NewRecorder()
	ctrl.routes().ServeHTTP(w, r)
	if w.Code != http.StatusBadRequest {
		t.Errorf("got %d want 400", w.Code)
	}
}

func TestHTTPCreateTimeshiftMissingNamespace(t *testing.T) {
	ctrl, _ := newTestController(t)
	w := doRequest(t, ctrl.routes(), http.MethodPost, "/timeshifts", api.CreateTimeshiftRequest{
		LabelSelector: "app=x",
		Time:          time.Now().Add(time.Hour).Format(time.RFC3339),
		TTL:           "1h",
	})
	if w.Code != http.StatusBadRequest {
		t.Errorf("got %d want 400", w.Code)
	}
}

func TestHTTPGetTimeshiftNotFound(t *testing.T) {
	ctrl, _ := newTestController(t)
	w := doRequest(t, ctrl.routes(), http.MethodGet, "/timeshifts/doesnotexist", nil)
	if w.Code != http.StatusNotFound {
		t.Errorf("got %d want 404", w.Code)
	}
}

func TestHTTPUpdateAndGetTimeshift(t *testing.T) {
	pod := makePod("web-1", "default", "10.0.0.1", "containerd://aabbcc112233")
	ctrl, _ := newTestController(t, pod)
	mux := ctrl.routes()

	// create
	target := time.Now().Add(24 * time.Hour).UTC().Truncate(time.Second)
	w := doRequest(t, mux, http.MethodPost, "/timeshifts", api.CreateTimeshiftRequest{
		Namespace:     "default",
		LabelSelector: "app=web-1",
		Time:          target.Format(time.RFC3339),
		TTL:           "1h",
	})
	if w.Code != http.StatusCreated {
		t.Fatalf("create: got %d; body: %s", w.Code, w.Body.String())
	}
	var created api.TimeshiftResponse
	decodeResponse(t, w, &created)

	// update
	newTarget := time.Now().Add(48 * time.Hour).UTC().Truncate(time.Second)
	w = doRequest(t, mux, http.MethodPatch, "/timeshifts/"+created.ID, api.UpdateTimeshiftRequest{
		Time: newTarget.Format(time.RFC3339),
	})
	if w.Code != http.StatusOK {
		t.Fatalf("update: got %d; body: %s", w.Code, w.Body.String())
	}
	var updated api.TimeshiftResponse
	decodeResponse(t, w, &updated)
	if updated.Time != newTarget.Format(time.RFC3339) {
		t.Errorf("time not updated: got %s want %s", updated.Time, newTarget.Format(time.RFC3339))
	}

	// get
	w = doRequest(t, mux, http.MethodGet, "/timeshifts/"+created.ID, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("get: got %d; body: %s", w.Code, w.Body.String())
	}
}

func TestHTTPDeleteTimeshift(t *testing.T) {
	pod := makePod("web-1", "default", "10.0.0.1", "containerd://aabbcc112233")
	ctrl, _ := newTestController(t, pod)
	mux := ctrl.routes()

	w := doRequest(t, mux, http.MethodPost, "/timeshifts", api.CreateTimeshiftRequest{
		Namespace:     "default",
		LabelSelector: "app=web-1",
		Time:          time.Now().Add(24 * time.Hour).Format(time.RFC3339),
		TTL:           "1h",
	})
	var created api.TimeshiftResponse
	decodeResponse(t, w, &created)

	w = doRequest(t, mux, http.MethodDelete, "/timeshifts/"+created.ID, nil)
	if w.Code != http.StatusNoContent {
		t.Errorf("delete: got %d want 204; body: %s", w.Code, w.Body.String())
	}

	w = doRequest(t, mux, http.MethodGet, "/timeshifts/"+created.ID, nil)
	if w.Code != http.StatusNotFound {
		t.Errorf("get after delete: got %d want 404", w.Code)
	}
}

// TestCreateTimeshiftNoTTL verifies that a timeshift created without a TTL is
// not swept by the expiry goroutine and has no expiresAt in its response.
func TestCreateTimeshiftNoTTL(t *testing.T) {
	pod := makePod("web-1", "default", "10.0.0.1", "containerd://aabbcc112233")
	ctrl, _ := newTestController(t, pod)

	s, err := ctrl.createTimeshift(context.Background(), "default", "app=web-1", time.Now().Add(time.Hour), 0, false)
	if err != nil {
		t.Fatalf("createTimeshift: %v", err)
	}
	if !s.expiresAt.IsZero() {
		t.Errorf("expiresAt should be zero for no-TTL timeshift, got %v", s.expiresAt)
	}

	resp := s.toResponse()
	if resp.ExpiresAt != "" {
		t.Errorf("ExpiresAt should be absent in response, got %q", resp.ExpiresAt)
	}
}

// TestSweepDoesNotExpireNoTTL verifies the sweeper ignores timeshifts with no TTL.
func TestSweepDoesNotExpireNoTTL(t *testing.T) {
	pod := makePod("web-1", "default", "10.0.0.1", "containerd://aabbcc112233")
	ctrl, pool := newTestController(t, pod)

	resetCalled := 0
	pool.resetFn = func(_ context.Context, _, _ string) error {
		resetCalled++
		return nil
	}

	s, err := ctrl.createTimeshift(context.Background(), "default", "app=web-1", time.Now().Add(time.Hour), 0, false)
	if err != nil {
		t.Fatalf("createTimeshift: %v", err)
	}

	ctrl.sweepExpired(context.Background())

	if resetCalled != 0 {
		t.Errorf("Reset called %d times; no-TTL timeshift should not be swept", resetCalled)
	}
	if _, err := ctrl.getTimeshift(s.id); err != nil {
		t.Errorf("no-TTL timeshift should still exist after sweep: %v", err)
	}
}

// TestHTTPCreateTimeshiftNoTTL verifies the HTTP handler accepts a missing TTL
// and the response omits expiresAt.
func TestHTTPCreateTimeshiftNoTTL(t *testing.T) {
	pod := makePod("web-1", "default", "10.0.0.1", "containerd://aabbcc112233")
	ctrl, _ := newTestController(t, pod)

	w := doRequest(t, ctrl.routes(), http.MethodPost, "/timeshifts", api.CreateTimeshiftRequest{
		Namespace:     "default",
		LabelSelector: "app=web-1",
		Time:          time.Now().Add(24 * time.Hour).Format(time.RFC3339),
	})
	if w.Code != http.StatusCreated {
		t.Fatalf("got %d want 201; body: %s", w.Code, w.Body.String())
	}

	// Decode into a raw map to check the key is absent, not just empty.
	var raw map[string]any
	if err := json.NewDecoder(w.Body).Decode(&raw); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if _, ok := raw["expiresAt"]; ok {
		t.Errorf("expiresAt key should be absent from response when no TTL set, got %v", raw["expiresAt"])
	}
}

// TestHTTPCreateTimeshiftInvalidTTL verifies that an explicitly invalid TTL
// string (not empty) is still rejected.
func TestHTTPCreateTimeshiftInvalidTTL(t *testing.T) {
	ctrl, _ := newTestController(t)
	w := doRequest(t, ctrl.routes(), http.MethodPost, "/timeshifts", api.CreateTimeshiftRequest{
		Namespace:     "default",
		LabelSelector: "app=web-1",
		Time:          time.Now().Add(time.Hour).Format(time.RFC3339),
		TTL:           "not-a-duration",
	})
	if w.Code != http.StatusBadRequest {
		t.Errorf("got %d want 400", w.Code)
	}
}

// TestListTimeshifts verifies that listTimeshifts returns all active entries
// sorted oldest-first.
func TestListTimeshifts(t *testing.T) {
	pod1 := makePod("web-1", "default", "10.0.0.1", "containerd://aabbcc112233")
	pod2 := makePod("web-2", "default", "10.0.0.2", "containerd://ddeeff445566")
	ctrl, _ := newTestController(t, pod1, pod2)

	// Empty to start.
	if got := ctrl.listTimeshifts(); len(got) != 0 {
		t.Fatalf("expected empty list, got %d entries", len(got))
	}

	target := time.Now().Add(24 * time.Hour).UTC().Truncate(time.Second)
	s1, err := ctrl.createTimeshift(context.Background(), "default", "app=web-1", target, time.Hour, false)
	if err != nil {
		t.Fatalf("createTimeshift 1: %v", err)
	}
	// Ensure distinct createdAt values.
	time.Sleep(time.Millisecond)
	s2, err := ctrl.createTimeshift(context.Background(), "default", "app=web-2", target.Add(time.Hour), 0, false)
	if err != nil {
		t.Fatalf("createTimeshift 2: %v", err)
	}

	list := ctrl.listTimeshifts()
	if len(list) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(list))
	}
	// Oldest first.
	if list[0].ID != s1.id {
		t.Errorf("expected first entry to be s1 (%s), got %s", s1.id[:8], list[0].ID[:8])
	}
	if list[1].ID != s2.id {
		t.Errorf("expected second entry to be s2 (%s), got %s", s2.id[:8], list[1].ID[:8])
	}
	// s1 has TTL â†’ ExpiresAt present; s2 has no TTL â†’ ExpiresAt absent.
	if list[0].ExpiresAt == "" {
		t.Error("s1 should have ExpiresAt set")
	}
	if list[1].ExpiresAt != "" {
		t.Errorf("s2 should have no ExpiresAt, got %q", list[1].ExpiresAt)
	}
}

func TestHTTPListTimeshifts(t *testing.T) {
	pod1 := makePod("web-1", "default", "10.0.0.1", "containerd://aabbcc112233")
	pod2 := makePod("web-2", "default", "10.0.0.2", "containerd://ddeeff445566")
	ctrl, _ := newTestController(t, pod1, pod2)
	mux := ctrl.routes()

	// Empty list before any timeshifts.
	w := doRequest(t, mux, http.MethodGet, "/timeshifts", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("empty list: got %d want 200", w.Code)
	}
	var empty api.ListTimeshiftsResponse
	decodeResponse(t, w, &empty)
	if len(empty.Timeshifts) != 0 {
		t.Fatalf("expected empty slice, got %d", len(empty.Timeshifts))
	}

	// Create two timeshifts â€” one per pod so they don't conflict.
	target := time.Now().Add(24 * time.Hour).UTC().Format(time.RFC3339)
	doRequest(t, mux, http.MethodPost, "/timeshifts", api.CreateTimeshiftRequest{
		Namespace: "default", LabelSelector: "app=web-1", Time: target, TTL: "1h",
	})
	doRequest(t, mux, http.MethodPost, "/timeshifts", api.CreateTimeshiftRequest{
		Namespace: "default", LabelSelector: "app=web-2", Time: target,
	})

	w = doRequest(t, mux, http.MethodGet, "/timeshifts", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("list: got %d want 200; body: %s", w.Code, w.Body.String())
	}
	var resp api.ListTimeshiftsResponse
	decodeResponse(t, w, &resp)
	if len(resp.Timeshifts) != 2 {
		t.Fatalf("expected 2 timeshifts, got %d", len(resp.Timeshifts))
	}
}

func TestHTTPHealthz(t *testing.T) {
	ctrl, _ := newTestController(t)
	w := doRequest(t, ctrl.routes(), http.MethodGet, "/healthz", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("got %d want 200; body: %s", w.Code, w.Body.String())
	}
	var body map[string]string
	decodeResponse(t, w, &body)
	if body["status"] != "ok" {
		t.Errorf("status: got %q want \"ok\"", body["status"])
	}
}

// TestUpdateTimeshiftReinjection verifies that when SetTime returns a gRPC
// NOT_FOUND (agent restarted), updateTimeshift re-injects the container via
// Inject and stores the new handle ID.
func TestUpdateTimeshiftReinjection(t *testing.T) {
	pod := makePod("web-1", "default", "10.0.0.1", "containerd://aabbcc112233")
	ctrl, pool := newTestController(t, pod)

	s, err := ctrl.createTimeshift(context.Background(), "default", "app=web-1", time.Now().Add(time.Hour), time.Hour, false)
	if err != nil {
		t.Fatalf("createTimeshift: %v", err)
	}
	originalHandle := s.handles[0].agentHandle

	// SetTime returns NOT_FOUND to simulate agent restart.
	pool.setTimeFn = func(_ context.Context, _, _ string, _ time.Time, _ bool) error {
		return grpcstatus.Error(codes.NotFound, "handle not found")
	}
	newAgentHandle := "handle-reinjected"
	injectCalled := 0
	pool.injectFn = func(_ context.Context, _, _ string, _ time.Time, _ bool) (string, error) {
		injectCalled++
		return newAgentHandle, nil
	}

	newTarget := time.Now().Add(48 * time.Hour)
	s2, err := ctrl.updateTimeshift(context.Background(), s.id, newTarget, false)
	if err != nil {
		t.Fatalf("updateTimeshift: %v", err)
	}
	if injectCalled != 1 {
		t.Errorf("Inject called %d times, want 1", injectCalled)
	}
	if s2.handles[0].agentHandle == originalHandle {
		t.Errorf("handle not updated: still %q", originalHandle)
	}
	if s2.handles[0].agentHandle != newAgentHandle {
		t.Errorf("handle = %q, want %q", s2.handles[0].agentHandle, newAgentHandle)
	}
}

// TestPodWatcherReinjection verifies that handlePodEvent re-injects a restarted
// pod (new containerID) and replaces the stale handle in the timeshift.
func TestPodWatcherReinjection(t *testing.T) {
	oldCID := "containerd://aabbcc112233"
	pod := makePod("web-1", "default", "10.0.0.1", oldCID)
	pod.Status.Phase = corev1.PodRunning
	ctrl, pool := newTestController(t, pod)

	s, err := ctrl.createTimeshift(context.Background(), "default", "app=web-1", time.Now().Add(time.Hour), time.Hour, false)
	if err != nil {
		t.Fatalf("createTimeshift: %v", err)
	}
	if len(s.handles) != 1 {
		t.Fatalf("expected 1 handle after create, got %d", len(s.handles))
	}

	// Simulate pod restart: same pod name, new containerID.
	newCID := "containerd://ddeeff445566"
	restarted := makePod("web-1", "default", "10.0.0.1", newCID)
	restarted.Status.Phase = corev1.PodRunning

	newAgentHandle := "handle-new"
	pool.injectFn = func(_ context.Context, _, _ string, _ time.Time, _ bool) (string, error) {
		return newAgentHandle, nil
	}

	ctrl.handlePodEvent(context.Background(), &restarted)

	ctrl.mu.RLock()
	ts := ctrl.timeshifts[s.id]
	handles := make([]containerHandle, len(ts.handles))
	copy(handles, ts.handles)
	ctrl.mu.RUnlock()

	if len(handles) != 1 {
		t.Fatalf("expected 1 handle after re-injection, got %d", len(handles))
	}
	if handles[0].containerID != newCID {
		t.Errorf("containerID = %q, want %q", handles[0].containerID, newCID)
	}
	if handles[0].agentHandle != newAgentHandle {
		t.Errorf("agentHandle = %q, want %q", handles[0].agentHandle, newAgentHandle)
	}
}

// TestPodWatcherSkipsAlreadyHandled verifies that handlePodEvent does not
// re-inject a pod whose containers are already in the handles list.
func TestPodWatcherSkipsAlreadyHandled(t *testing.T) {
	cid := "containerd://aabbcc112233"
	pod := makePod("web-1", "default", "10.0.0.1", cid)
	pod.Status.Phase = corev1.PodRunning
	ctrl, pool := newTestController(t, pod)

	s, err := ctrl.createTimeshift(context.Background(), "default", "app=web-1", time.Now().Add(time.Hour), time.Hour, false)
	if err != nil {
		t.Fatalf("createTimeshift: %v", err)
	}

	injectCalled := 0
	pool.injectFn = func(_ context.Context, _, _ string, _ time.Time, _ bool) (string, error) {
		injectCalled++
		return "handle-extra", nil
	}

	// Same pod, same containerID â€” already handled.
	ctrl.handlePodEvent(context.Background(), &pod)

	if injectCalled != 0 {
		t.Errorf("Inject called %d times for already-handled pod, want 0", injectCalled)
	}
	ctrl.mu.RLock()
	handleCount := len(ctrl.timeshifts[s.id].handles)
	ctrl.mu.RUnlock()
	if handleCount != 1 {
		t.Errorf("handle count = %d, want 1 (no duplicates)", handleCount)
	}
}

// TestMetrics verifies that the /metrics endpoint is wired up and reflects
// real controller activity: inject counters, the active-timeshifts gauge, and
// the HTTP request counter are all exercised by a create+delete cycle.
func TestMetrics(t *testing.T) {
	pod := makePod("web-1", "default", "10.0.0.1", "containerd://aabbcc112233")
	ctrl, _ := newTestController(t, pod)
	mux := ctrl.routes()

	// Create a timeshift â€” increments inject counter and active gauge.
	w := doRequest(t, mux, http.MethodPost, "/timeshifts", api.CreateTimeshiftRequest{
		Namespace:     "default",
		LabelSelector: "app=web-1",
		Time:          time.Now().Add(24 * time.Hour).Format(time.RFC3339),
		TTL:           "1h",
	})
	if w.Code != http.StatusCreated {
		t.Fatalf("create: got %d; body: %s", w.Code, w.Body.String())
	}
	var created api.TimeshiftResponse
	decodeResponse(t, w, &created)

	// Delete it â€” decrements active gauge.
	w = doRequest(t, mux, http.MethodDelete, "/timeshifts/"+created.ID, nil)
	if w.Code != http.StatusNoContent {
		t.Fatalf("delete: got %d; body: %s", w.Code, w.Body.String())
	}

	// Fetch /metrics and verify key lines are present.
	w = doRequest(t, mux, http.MethodGet, "/metrics", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("metrics: got %d; body: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()

	checks := []string{
		// Active gauge should be 0 after create+delete.
		"epochd_timeshifts_active 0",
		// Inject counter was incremented on create.
		`epochd_inject_total{result="success"}`,
		// API request counter recorded the POST /timeshifts â†’ 201.
		`epochd_api_requests_total{method="POST",path="/timeshifts",status="201"}`,
		// API request counter recorded the DELETE /timeshifts/{id} â†’ 204.
		`epochd_api_requests_total{method="DELETE",path="/timeshifts/{id}",status="204"}`,
	}
	for _, want := range checks {
		if !strings.Contains(body, want) {
			t.Errorf("/metrics missing %q\nbody:\n%s", want, body)
		}
	}
}

// TestSweepMetrics verifies that sweepExpired increments the sweep counter and
// decrements the active gauge.
func TestSweepMetrics(t *testing.T) {
	pod := makePod("web-1", "default", "10.0.0.1", "containerd://aabbcc112233")
	ctrl, _ := newTestController(t, pod)
	mux := ctrl.routes()

	w := doRequest(t, mux, http.MethodPost, "/timeshifts", api.CreateTimeshiftRequest{
		Namespace:     "default",
		LabelSelector: "app=web-1",
		Time:          time.Now().Add(time.Hour).Format(time.RFC3339),
		TTL:           "1ms",
	})
	if w.Code != http.StatusCreated {
		t.Fatalf("create: got %d; body: %s", w.Code, w.Body.String())
	}
	var created api.TimeshiftResponse
	decodeResponse(t, w, &created)

	// Backdate expiresAt so it looks expired, then sweep.
	ctrl.mu.Lock()
	ctrl.timeshifts[created.ID].expiresAt = time.Now().Add(-time.Second)
	ctrl.mu.Unlock()
	ctrl.sweepExpired(context.Background())

	w = doRequest(t, mux, http.MethodGet, "/metrics", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("metrics: got %d", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{
		"epochd_timeshifts_active 0",
		"epochd_sweep_expired_total 1",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("/metrics missing %q\nbody:\n%s", want, body)
		}
	}
}

// TestSweepEmitsEvent verifies that sweepExpired posts a Kubernetes Event for
// each unique pod whose timeshift has expired.
func TestSweepEmitsEvent(t *testing.T) {
	pod := makePod("web-1", "default", "10.0.0.1", "containerd://aabbcc112233")
	ctrl, _ := newTestController(t, pod)

	fake := record.NewFakeRecorder(10)
	ctrl.setRecorder(fake)

	target := time.Now().Add(24 * time.Hour).UTC().Truncate(time.Second)
	s, err := ctrl.createTimeshift(context.Background(), "default", "app=web-1", target, time.Hour, false)
	if err != nil {
		t.Fatalf("createTimeshift: %v", err)
	}

	ctrl.mu.Lock()
	ctrl.timeshifts[s.id].expiresAt = time.Now().Add(-time.Second)
	ctrl.mu.Unlock()
	ctrl.sweepExpired(context.Background())

	select {
	case ev := <-fake.Events:
		if !strings.Contains(ev, "TimeshiftExpired") {
			t.Errorf("event reason missing TimeshiftExpired: %q", ev)
		}
		if !strings.Contains(ev, s.id[:8]) {
			t.Errorf("event missing timeshift ID %s: %q", s.id[:8], ev)
		}
		if !strings.Contains(ev, target.Format(time.RFC3339)) {
			t.Errorf("event missing target time %s: %q", target.Format(time.RFC3339), ev)
		}
		if !strings.Contains(ev, "1h0m0s") {
			t.Errorf("event missing TTL duration: %q", ev)
		}
	default:
		t.Fatal("expected an event to be emitted, but channel was empty")
	}

	// Only one event per pod even if the pod has multiple containers in the timeshift.
	if len(fake.Events) != 0 {
		t.Errorf("expected exactly 1 event, got %d extra", len(fake.Events))
	}
}

// TestSweepEmitsOneEventPerPod verifies that a timeshift with two handles on the
// same pod emits exactly one event, not one per container.
func TestSweepEmitsOneEventPerPod(t *testing.T) {
	pod := makePod("web-1", "default", "10.0.0.1", "containerd://aabbcc112233")
	ctrl, _ := newTestController(t, pod)

	fake := record.NewFakeRecorder(10)
	ctrl.setRecorder(fake)

	target := time.Now().Add(time.Hour)
	s, err := ctrl.createTimeshift(context.Background(), "default", "app=web-1", target, time.Hour, false)
	if err != nil {
		t.Fatalf("createTimeshift: %v", err)
	}
	// Manually add a second handle on the same pod to simulate two containers.
	ctrl.mu.Lock()
	ctrl.timeshifts[s.id].handles = append(ctrl.timeshifts[s.id].handles, containerHandle{
		pod: "web-1", container: "sidecar", nodeIP: "10.0.0.1",
	})
	ctrl.timeshifts[s.id].expiresAt = time.Now().Add(-time.Second)
	ctrl.mu.Unlock()

	ctrl.sweepExpired(context.Background())

	if len(fake.Events) != 1 {
		t.Errorf("expected 1 event for 1 pod, got %d", len(fake.Events))
	}
}

// TestExpiredCounterIncrements verifies the Prometheus sweep counter increments
// once per expired timeshift.
func TestExpiredCounterIncrements(t *testing.T) {
	pod1 := makePod("web-1", "default", "10.0.0.1", "containerd://aabbcc112233")
	pod2 := makePod("web-2", "default", "10.0.0.2", "containerd://ddeeff445566")
	ctrl, _ := newTestController(t, pod1, pod2)
	mux := ctrl.routes()

	for _, sel := range []string{"app=web-1", "app=web-2"} {
		w := doRequest(t, mux, http.MethodPost, "/timeshifts", api.CreateTimeshiftRequest{
			Namespace:     "default",
			LabelSelector: sel,
			Time:          time.Now().Add(time.Hour).Format(time.RFC3339),
			TTL:           "1ms",
		})
		if w.Code != http.StatusCreated {
			t.Fatalf("create %s: got %d; body: %s", sel, w.Code, w.Body.String())
		}
		var created api.TimeshiftResponse
		decodeResponse(t, w, &created)
		ctrl.mu.Lock()
		ctrl.timeshifts[created.ID].expiresAt = time.Now().Add(-time.Second)
		ctrl.mu.Unlock()
	}

	ctrl.sweepExpired(context.Background())

	w := doRequest(t, mux, http.MethodGet, "/metrics", nil)
	body := w.Body.String()
	if !strings.Contains(body, "epochd_sweep_expired_total 2") {
		t.Errorf("expected epochd_sweep_expired_total 2 in metrics; body:\n%s", body)
	}
}

// ---------------------------------------------------------------------------
// Store / restart-recovery tests
// ---------------------------------------------------------------------------

// newTestControllerWithStore creates a controller backed by a real (fake) store
// so persist/restore can be exercised in unit tests.
func newTestControllerWithStore(t *testing.T, pods ...corev1.Pod) (*controller, *mockAgentPool, *fake.Clientset) {
	t.Helper()
	k8s := fake.NewClientset()
	for i := range pods {
		if _, err := k8s.CoreV1().Pods(pods[i].Namespace).Create(context.Background(), &pods[i], metav1.CreateOptions{}); err != nil {
			t.Fatalf("create pod: %v", err)
		}
	}
	pool := &mockAgentPool{}
	st := newStore(k8s, "epochd")
	return newController(k8s, pool, st, applog.Discard()), pool, k8s
}

// TestStoreRoundTrip saves a timeshift to a fake ConfigMap and loads it back.
func TestStoreRoundTrip(t *testing.T) {
	k8s := fake.NewClientset()
	st := newStore(k8s, "epochd")
	ctx := context.Background()

	target := time.Date(2030, 1, 15, 12, 0, 0, 0, time.UTC)
	original := map[string]*timeshift{
		"abc123": {
			id:            "abc123",
			namespace:     "staging",
			labelSelector: "app=svc",
			targetTime:    target,
			ttl:           time.Hour,
			expiresAt:     target.Add(time.Hour),
			createdAt:     target.Add(-time.Minute),
			handles: []containerHandle{
				{
					pod:         "svc-abc",
					container:   "main",
					nodeIP:      "10.0.0.5",
					containerID: "containerd://aabb1122",
					agentHandle: "handle-xyz",
				},
			},
		},
	}

	if err := st.save(ctx, original); err != nil {
		t.Fatalf("save: %v", err)
	}

	loaded, err := st.load(ctx)
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	got, ok := loaded["abc123"]
	if !ok {
		t.Fatal("loaded map missing key abc123")
	}
	if got.id != "abc123" {
		t.Errorf("id: got %q want %q", got.id, "abc123")
	}
	if got.namespace != "staging" {
		t.Errorf("namespace: got %q want %q", got.namespace, "staging")
	}
	if !got.targetTime.Equal(target) {
		t.Errorf("targetTime: got %v want %v", got.targetTime, target)
	}
	if got.ttl != time.Hour {
		t.Errorf("ttl: got %v want %v", got.ttl, time.Hour)
	}
	if len(got.handles) != 1 {
		t.Fatalf("handles: got %d want 1", len(got.handles))
	}
	h := got.handles[0]
	if h.pod != "svc-abc" || h.container != "main" || h.agentHandle != "handle-xyz" {
		t.Errorf("handle fields wrong: %+v", h)
	}
}

// TestRestoreEmptyStore verifies that a missing ConfigMap (first run) returns an
// empty map without error.
func TestRestoreEmptyStore(t *testing.T) {
	k8s := fake.NewClientset()
	st := newStore(k8s, "epochd")
	loaded, err := st.load(context.Background())
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(loaded) != 0 {
		t.Errorf("expected empty map, got %d entries", len(loaded))
	}
}

// TestControllerRestore verifies the main recovery story: a controller creates a
// timeshift (which persists to the ConfigMap), then a second controller using
// the same backing store recovers that timeshift on startup.
func TestControllerRestore(t *testing.T) {
	pod := makePod("web-1", "default", "10.0.0.1", "containerd://aabbcc112233")
	ctrl1, _, k8s := newTestControllerWithStore(t, pod)
	ctx := context.Background()

	target := time.Now().Add(24 * time.Hour).UTC().Truncate(time.Second)
	s, err := ctrl1.createTimeshift(ctx, "default", "app=web-1", target, time.Hour, false)
	if err != nil {
		t.Fatalf("createTimeshift: %v", err)
	}

	// Simulate controller restart: new controller, same backing store.
	ctrl2 := newController(k8s, &mockAgentPool{}, newStore(k8s, "epochd"), applog.Discard())
	ctrl2.restore(ctx)

	ctrl2.mu.RLock()
	restored, ok := ctrl2.timeshifts[s.id]
	ctrl2.mu.RUnlock()

	if !ok {
		t.Fatal("timeshift not present after restore")
	}
	if !restored.targetTime.Equal(target) {
		t.Errorf("targetTime: got %v want %v", restored.targetTime, target)
	}
	if restored.namespace != "default" {
		t.Errorf("namespace: got %q want %q", restored.namespace, "default")
	}
	if len(restored.handles) != len(s.handles) {
		t.Errorf("handles: got %d want %d", len(restored.handles), len(s.handles))
	}
}

// TestControllerRestoreAfterDelete verifies that deleting a timeshift removes it
// from the store so a restarted controller starts with an empty registry.
func TestControllerRestoreAfterDelete(t *testing.T) {
	pod := makePod("web-1", "default", "10.0.0.1", "containerd://aabbcc112233")
	ctrl1, _, k8s := newTestControllerWithStore(t, pod)
	ctx := context.Background()

	s, err := ctrl1.createTimeshift(ctx, "default", "app=web-1", time.Now().Add(time.Hour), time.Hour, false)
	if err != nil {
		t.Fatalf("createTimeshift: %v", err)
	}
	if err := ctrl1.deleteTimeshift(ctx, s.id); err != nil {
		t.Fatalf("deleteTimeshift: %v", err)
	}

	ctrl2 := newController(k8s, &mockAgentPool{}, newStore(k8s, "epochd"), applog.Discard())
	ctrl2.restore(ctx)

	ctrl2.mu.RLock()
	count := len(ctrl2.timeshifts)
	ctrl2.mu.RUnlock()

	if count != 0 {
		t.Errorf("expected empty registry after restore, got %d entry/entries", count)
	}
}

// TestControllerRestoreGauge verifies that restore correctly initialises the
// Prometheus active-timeshifts gauge.
func TestControllerRestoreGauge(t *testing.T) {
	pod1 := makePod("web-1", "default", "10.0.0.1", "containerd://aabbcc112233")
	pod2 := makePod("web-2", "default", "10.0.0.2", "containerd://ddeeff445566")
	ctrl1, _, k8s := newTestControllerWithStore(t, pod1, pod2)
	ctx := context.Background()

	// Create two timeshifts (one per pod) so the gauge must be 2 after restore.
	for _, sel := range []string{"app=web-1", "app=web-2"} {
		if _, err := ctrl1.createTimeshift(ctx, "default", sel, time.Now().Add(time.Hour), 0, false); err != nil {
			t.Fatalf("createTimeshift: %v", err)
		}
	}

	ctrl2 := newController(k8s, &mockAgentPool{}, newStore(k8s, "epochd"), applog.Discard())
	ctrl2.restore(ctx)

	// Read the gauge via /metrics.
	w := doRequest(t, ctrl2.routes(), "GET", "/metrics", nil)
	if w.Code != 200 {
		t.Fatalf("metrics: %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "epochd_timeshifts_active 2") {
		t.Errorf("/metrics: expected epochd_timeshifts_active 2\nbody:\n%s", body)
	}
}

// ---------------------------------------------------------------------------
// Resolve endpoint tests
// ---------------------------------------------------------------------------

// TestHTTPResolve verifies GET /resolve returns matched pods with their running
// containers, makes no agent calls, and creates no timeshifts.
func TestHTTPResolve(t *testing.T) {
	ctx := context.Background()
	k8s := fake.NewClientset()

	for _, name := range []string{"web-1", "web-2"} {
		pod := corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
				Namespace: "default",
				Labels:    map[string]string{"tier": "frontend"},
			},
			Status: corev1.PodStatus{
				HostIP: "10.0.0.1",
				ContainerStatuses: []corev1.ContainerStatus{{
					Name:        "server",
					ContainerID: "containerd://aa" + name,
					State:       corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
				}},
			},
		}
		if _, err := k8s.CoreV1().Pods("default").Create(ctx, &pod, metav1.CreateOptions{}); err != nil {
			t.Fatalf("create pod %s: %v", name, err)
		}
	}

	pool := &mockAgentPool{}
	injectCalled := 0
	pool.injectFn = func(_ context.Context, _, _ string, _ time.Time, _ bool) (string, error) {
		injectCalled++
		return "", nil
	}

	ctrl := newController(k8s, pool, nil, applog.Discard())
	mux := ctrl.routes()

	w := doRequest(t, mux, http.MethodGet, "/resolve?namespace=default&selector=tier=frontend", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("got %d want 200; body: %s", w.Code, w.Body.String())
	}
	var resp api.ResolveResponse
	decodeResponse(t, w, &resp)

	if len(resp.Pods) != 2 {
		t.Fatalf("expected 2 pods, got %d: %+v", len(resp.Pods), resp.Pods)
	}
	// Pods are sorted by name.
	if resp.Pods[0].Name != "web-1" || resp.Pods[1].Name != "web-2" {
		t.Errorf("pod names: got %q and %q", resp.Pods[0].Name, resp.Pods[1].Name)
	}
	if resp.Pods[0].NodeIP != "10.0.0.1" {
		t.Errorf("nodeIP: got %q want %q", resp.Pods[0].NodeIP, "10.0.0.1")
	}
	if len(resp.Pods[0].Containers) != 1 || resp.Pods[0].Containers[0] != "server" {
		t.Errorf("containers: got %v", resp.Pods[0].Containers)
	}
	if injectCalled != 0 {
		t.Errorf("Inject called %d times; resolve must not touch agents", injectCalled)
	}
	ctrl.mu.RLock()
	timeshiftCount := len(ctrl.timeshifts)
	ctrl.mu.RUnlock()
	if timeshiftCount != 0 {
		t.Errorf("expected no timeshifts after resolve, got %d", timeshiftCount)
	}
}

// TestHTTPResolveMissingParams verifies that both query parameters are required.
func TestHTTPResolveMissingParams(t *testing.T) {
	ctrl, _ := newTestController(t)
	mux := ctrl.routes()

	w := doRequest(t, mux, http.MethodGet, "/resolve?selector=app=x", nil)
	if w.Code != http.StatusBadRequest {
		t.Errorf("missing namespace: got %d want 400", w.Code)
	}

	w = doRequest(t, mux, http.MethodGet, "/resolve?namespace=default", nil)
	if w.Code != http.StatusBadRequest {
		t.Errorf("missing selector: got %d want 400", w.Code)
	}
}

// TestHTTPResolveExcludesNonRunning verifies that pods with no running
// containers are omitted from the response.
func TestHTTPResolveExcludesNonRunning(t *testing.T) {
	ctx := context.Background()
	k8s := fake.NewClientset()

	// Pod with a terminated container â€” should be excluded.
	pending := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "terminating",
			Namespace: "default",
			Labels:    map[string]string{"app": "batch"},
		},
		Status: corev1.PodStatus{
			HostIP: "10.0.0.9",
			ContainerStatuses: []corev1.ContainerStatus{{
				Name:        "worker",
				ContainerID: "containerd://deadbeef",
				State:       corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{}},
			}},
		},
	}
	if _, err := k8s.CoreV1().Pods("default").Create(ctx, &pending, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create pod: %v", err)
	}

	ctrl := newController(k8s, &mockAgentPool{}, nil, applog.Discard())
	w := doRequest(t, ctrl.routes(), http.MethodGet, "/resolve?namespace=default&selector=app=batch", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("got %d want 200; body: %s", w.Code, w.Body.String())
	}
	var resp api.ResolveResponse
	decodeResponse(t, w, &resp)
	if len(resp.Pods) != 0 {
		t.Errorf("expected 0 pods (all non-running), got %d: %+v", len(resp.Pods), resp.Pods)
	}
}

// ---------------------------------------------------------------------------
// Timeshift status endpoint tests
// ---------------------------------------------------------------------------

// TestHTTPTimeshiftStatus verifies GET /timeshifts/{id}/status returns live
// injection state from the agent for each container in the timeshift.
func TestHTTPTimeshiftStatus(t *testing.T) {
	pod := makePod("web-1", "default", "10.0.0.1", "containerd://aabbcc112233")
	ctrl, pool := newTestController(t, pod)
	mux := ctrl.routes()

	target := time.Now().Add(24 * time.Hour).UTC().Truncate(time.Second)
	w := doRequest(t, mux, http.MethodPost, "/timeshifts", api.CreateTimeshiftRequest{
		Namespace:     "default",
		LabelSelector: "app=web-1",
		Time:          target.Format(time.RFC3339),
		TTL:           "1h",
	})
	if w.Code != http.StatusCreated {
		t.Fatalf("create: got %d; body: %s", w.Code, w.Body.String())
	}
	var created api.TimeshiftResponse
	decodeResponse(t, w, &created)

	pool.getStatusFn = func(_ context.Context, _, _ string) (*api.HandleStatus, error) {
		return &api.HandleStatus{
			Generation: 3,
			LastTarget: target.Format(time.RFC3339),
			StateAddr:  "0x7ffd1234abcd",
			PID:        1234,
		}, nil
	}

	w = doRequest(t, mux, http.MethodGet, "/timeshifts/"+created.ID+"/status", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status: got %d; body: %s", w.Code, w.Body.String())
	}
	var resp api.TimeshiftStatusResponse
	decodeResponse(t, w, &resp)

	if resp.ID != created.ID {
		t.Errorf("ID: got %q want %q", resp.ID, created.ID)
	}
	if resp.Namespace != "default" {
		t.Errorf("namespace: got %q want %q", resp.Namespace, "default")
	}
	if len(resp.Containers) != 1 {
		t.Fatalf("expected 1 container entry, got %d", len(resp.Containers))
	}
	entry := resp.Containers[0]
	if entry.Pod != "web-1" {
		t.Errorf("pod: got %q want %q", entry.Pod, "web-1")
	}
	if entry.Status == nil {
		t.Fatal("status is nil")
	}
	if entry.Status.Generation != 3 {
		t.Errorf("generation: got %d want 3", entry.Status.Generation)
	}
	if entry.Status.StateAddr != "0x7ffd1234abcd" {
		t.Errorf("stateAddr: got %q want %q", entry.Status.StateAddr, "0x7ffd1234abcd")
	}
	if entry.Error != "" {
		t.Errorf("unexpected error: %q", entry.Error)
	}
}

// TestHTTPTimeshiftStatusNotFound verifies a 404 for an unknown timeshift ID.
func TestHTTPTimeshiftStatusNotFound(t *testing.T) {
	ctrl, _ := newTestController(t)
	w := doRequest(t, ctrl.routes(), http.MethodGet, "/timeshifts/does-not-exist/status", nil)
	if w.Code != http.StatusNotFound {
		t.Errorf("got %d want 404", w.Code)
	}
}

// TestHTTPTimeshiftStatusAgentError verifies that a GetStatus failure from the
// agent is reflected per-container (error field set, status nil) rather than
// causing a non-200 HTTP response â€” partial status is still useful.
func TestHTTPTimeshiftStatusAgentError(t *testing.T) {
	pod := makePod("web-1", "default", "10.0.0.1", "containerd://aabbcc112233")
	ctrl, pool := newTestController(t, pod)
	mux := ctrl.routes()

	w := doRequest(t, mux, http.MethodPost, "/timeshifts", api.CreateTimeshiftRequest{
		Namespace:     "default",
		LabelSelector: "app=web-1",
		Time:          time.Now().Add(time.Hour).Format(time.RFC3339),
		TTL:           "1h",
	})
	var created api.TimeshiftResponse
	decodeResponse(t, w, &created)

	pool.getStatusFn = func(_ context.Context, _, _ string) (*api.HandleStatus, error) {
		return nil, fmt.Errorf("agent unreachable")
	}

	w = doRequest(t, mux, http.MethodGet, "/timeshifts/"+created.ID+"/status", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("got %d want 200 (errors are per-container, not HTTP-level)", w.Code)
	}
	var resp api.TimeshiftStatusResponse
	decodeResponse(t, w, &resp)
	if len(resp.Containers) != 1 {
		t.Fatalf("expected 1 container entry, got %d", len(resp.Containers))
	}
	entry := resp.Containers[0]
	if entry.Error == "" {
		t.Error("expected error field when agent GetStatus fails")
	}
	if entry.Status != nil {
		t.Error("expected status to be nil when agent GetStatus fails")
	}
}

// TestNewIDUniqueness verifies no collisions across many IDs.
func TestNewIDUniqueness(t *testing.T) {
	seen := make(map[string]struct{}, 10000)
	for i := 0; i < 10000; i++ {
		id := newID()
		if _, dup := seen[id]; dup {
			t.Fatalf("duplicate ID after %d calls: %s", i, id)
		}
		seen[id] = struct{}{}
	}
}



