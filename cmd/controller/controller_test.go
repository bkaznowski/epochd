package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"epochd/pkg/api"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

// ---------------------------------------------------------------------------
// Mock AgentPool
// ---------------------------------------------------------------------------

type mockAgentPool struct {
	injectFn  func(ctx context.Context, nodeIP, containerID string, target time.Time) (string, error)
	setTimeFn func(ctx context.Context, nodeIP, handleID string, target time.Time) error
	resetFn   func(ctx context.Context, nodeIP, handleID string) error
}

func (m *mockAgentPool) Inject(ctx context.Context, nodeIP, containerID string, target time.Time) (string, error) {
	if m.injectFn != nil {
		return m.injectFn(ctx, nodeIP, containerID, target)
	}
	return "handle-" + containerID[:8], nil
}

func (m *mockAgentPool) SetTime(ctx context.Context, nodeIP, handleID string, target time.Time) error {
	if m.setTimeFn != nil {
		return m.setTimeFn(ctx, nodeIP, handleID, target)
	}
	return nil
}

func (m *mockAgentPool) Reset(ctx context.Context, nodeIP, handleID string) error {
	if m.resetFn != nil {
		return m.resetFn(ctx, nodeIP, handleID)
	}
	return nil
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
	return newController(k8s, pool), pool
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
	s, err := ctrl.createTimeshift(context.Background(), "default", "app=web-1", target, time.Hour)
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
	_, err := ctrl.createTimeshift(context.Background(), "default", "app=ghost", time.Now().Add(time.Hour), time.Hour)
	if err == nil {
		t.Fatal("expected error for no matching pods")
	}
	if !isNotFound(err) {
		t.Errorf("expected notFoundError, got %T: %v", err, err)
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
	pool.setTimeFn = func(_ context.Context, _, _ string, _ time.Time) error {
		called++
		return nil
	}

	s, err := ctrl.createTimeshift(context.Background(), "default", "app=web-1", time.Now().Add(time.Hour), time.Hour)
	if err != nil {
		t.Fatalf("createTimeshift: %v", err)
	}
	newTarget := time.Now().Add(48 * time.Hour)
	s2, err := ctrl.updateTimeshift(context.Background(), s.id, newTarget)
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

	s, err := ctrl.createTimeshift(context.Background(), "default", "app=web-1", time.Now().Add(time.Hour), time.Hour)
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

	s, err := ctrl.createTimeshift(context.Background(), "default", "app=web-1", time.Now().Add(time.Hour), time.Millisecond)
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

	s, err := ctrl.createTimeshift(context.Background(), "default", "app=web-1", time.Now().Add(time.Hour), 0)
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

	s, err := ctrl.createTimeshift(context.Background(), "default", "app=web-1", time.Now().Add(time.Hour), 0)
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
	pod := makePod("web-1", "default", "10.0.0.1", "containerd://aabbcc112233")
	ctrl, _ := newTestController(t, pod)

	// Empty to start.
	if got := ctrl.listTimeshifts(); len(got) != 0 {
		t.Fatalf("expected empty list, got %d entries", len(got))
	}

	target := time.Now().Add(24 * time.Hour).UTC().Truncate(time.Second)
	s1, err := ctrl.createTimeshift(context.Background(), "default", "app=web-1", target, time.Hour)
	if err != nil {
		t.Fatalf("createTimeshift 1: %v", err)
	}
	// Ensure distinct createdAt values.
	time.Sleep(time.Millisecond)
	s2, err := ctrl.createTimeshift(context.Background(), "default", "app=web-1", target.Add(time.Hour), 0)
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
	// s1 has TTL → ExpiresAt present; s2 has no TTL → ExpiresAt absent.
	if list[0].ExpiresAt == "" {
		t.Error("s1 should have ExpiresAt set")
	}
	if list[1].ExpiresAt != "" {
		t.Errorf("s2 should have no ExpiresAt, got %q", list[1].ExpiresAt)
	}
}

func TestHTTPListTimeshifts(t *testing.T) {
	pod := makePod("web-1", "default", "10.0.0.1", "containerd://aabbcc112233")
	ctrl, _ := newTestController(t, pod)
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

	// Create two timeshifts.
	target := time.Now().Add(24 * time.Hour).UTC().Format(time.RFC3339)
	doRequest(t, mux, http.MethodPost, "/timeshifts", api.CreateTimeshiftRequest{
		Namespace: "default", LabelSelector: "app=web-1", Time: target, TTL: "1h",
	})
	doRequest(t, mux, http.MethodPost, "/timeshifts", api.CreateTimeshiftRequest{
		Namespace: "default", LabelSelector: "app=web-1", Time: target,
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

