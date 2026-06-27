package api_test

import (
	"encoding/json"
	"testing"

	"github.com/bkaznowski/epochd/pkg/api"
)

func TestCreateTimeshiftRequestRoundTrip(t *testing.T) {
	req := api.CreateTimeshiftRequest{
		Namespace:     "default",
		LabelSelector: "app=web",
		Time:          "2030-01-01T00:00:00Z",
		TTL:           "1h",
		Freeze:        true,
	}
	b, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var got api.CreateTimeshiftRequest
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if got.Namespace != req.Namespace {
		t.Errorf("Namespace: got %q want %q", got.Namespace, req.Namespace)
	}
	if got.LabelSelector != req.LabelSelector {
		t.Errorf("LabelSelector: got %q want %q", got.LabelSelector, req.LabelSelector)
	}
	if got.Time != req.Time {
		t.Errorf("Time: got %q want %q", got.Time, req.Time)
	}
	if got.TTL != req.TTL {
		t.Errorf("TTL: got %q want %q", got.TTL, req.TTL)
	}
	if got.Freeze != req.Freeze {
		t.Errorf("Freeze: got %v want %v", got.Freeze, req.Freeze)
	}
}

func TestCreateTimeshiftRequestOmitEmpty(t *testing.T) {
	req := api.CreateTimeshiftRequest{
		Namespace:     "ns",
		LabelSelector: "app=x",
		Time:          "2030-01-01T00:00:00Z",
	}
	b, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	// TTL and Freeze should be omitted when zero.
	var raw map[string]any
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatalf("Unmarshal raw: %v", err)
	}
	if _, ok := raw["ttl"]; ok {
		t.Error("ttl should be omitted when empty")
	}
	if _, ok := raw["freeze"]; ok {
		t.Error("freeze should be omitted when false")
	}
}

func TestUpdateTimeshiftRequestRoundTrip(t *testing.T) {
	req := api.UpdateTimeshiftRequest{
		Time:     "2030-06-01T12:00:00Z",
		Duration: "24h",
		Freeze:   true,
	}
	b, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var got api.UpdateTimeshiftRequest
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if got.Time != req.Time {
		t.Errorf("Time: got %q want %q", got.Time, req.Time)
	}
	if got.Duration != req.Duration {
		t.Errorf("Duration: got %q want %q", got.Duration, req.Duration)
	}
	if got.Freeze != req.Freeze {
		t.Errorf("Freeze: got %v want %v", got.Freeze, req.Freeze)
	}
}

func TestTimeshiftResponseRoundTrip(t *testing.T) {
	resp := api.TimeshiftResponse{
		ID:        "abc1234",
		Namespace: "default",
		Time:      "2030-01-01T00:00:00Z",
		Frozen:    true,
		ExpiresAt: "2030-01-02T00:00:00Z",
		AppliedTo: []string{"pod-a/app", "pod-b/app"},
	}
	b, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var got api.TimeshiftResponse
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if got.ID != resp.ID {
		t.Errorf("ID: got %q want %q", got.ID, resp.ID)
	}
	if got.Frozen != resp.Frozen {
		t.Errorf("Frozen: got %v want %v", got.Frozen, resp.Frozen)
	}
	if len(got.AppliedTo) != len(resp.AppliedTo) {
		t.Errorf("AppliedTo len: got %d want %d", len(got.AppliedTo), len(resp.AppliedTo))
	}
}

func TestErrorResponseRoundTrip(t *testing.T) {
	e := api.ErrorResponse{Error: "something went wrong"}
	b, err := json.Marshal(e)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var got api.ErrorResponse
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if got.Error != e.Error {
		t.Errorf("Error: got %q want %q", got.Error, e.Error)
	}
}

func TestTimeshiftStatusResponseRoundTrip(t *testing.T) {
	resp := api.TimeshiftStatusResponse{
		ID:        "abc1234",
		Namespace: "staging",
		Containers: []api.ContainerStatusEntry{
			{
				Pod:       "web-abc",
				Container: "app",
				NodeIP:    "10.0.0.1",
				Status: &api.HandleStatus{
					Generation: 2,
					LastTarget: "2030-01-01T00:00:00Z",
					StateAddr:  "0x7ffe1234",
					PID:        99,
				},
			},
			{
				Pod:       "web-def",
				Container: "sidecar",
				NodeIP:    "10.0.0.2",
				Error:     "inject failed",
			},
		},
	}
	b, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var got api.TimeshiftStatusResponse
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if got.ID != resp.ID {
		t.Errorf("ID: got %q want %q", got.ID, resp.ID)
	}
	if len(got.Containers) != 2 {
		t.Fatalf("Containers len: got %d want 2", len(got.Containers))
	}
	if got.Containers[0].Status == nil {
		t.Error("first container Status should be non-nil")
	} else if got.Containers[0].Status.PID != 99 {
		t.Errorf("PID: got %d want 99", got.Containers[0].Status.PID)
	}
	if got.Containers[1].Error != "inject failed" {
		t.Errorf("Error: got %q want %q", got.Containers[1].Error, "inject failed")
	}
}

func TestResolvedPodRoundTrip(t *testing.T) {
	pod := api.ResolvedPod{
		Name:       "web-abc",
		Namespace:  "default",
		NodeIP:     "10.0.0.1",
		Containers: []string{"app", "sidecar"},
	}
	b, err := json.Marshal(pod)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var got api.ResolvedPod
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if got.Name != pod.Name {
		t.Errorf("Name: got %q want %q", got.Name, pod.Name)
	}
	if len(got.Containers) != len(pod.Containers) {
		t.Errorf("Containers len: got %d want %d", len(got.Containers), len(pod.Containers))
	}
}
