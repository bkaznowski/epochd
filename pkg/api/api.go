// Package api holds the HTTP+JSON request and response types for the epochd
// controller's REST API. These types are shared between the controller
// implementation and any Go client (e.g. the e2e test SDK in phase 10).
// No build tag — the types are pure structs with no OS-specific dependencies.
package api

// CreateTimeshiftRequest is the body of POST /timeshifts.
type CreateTimeshiftRequest struct {
	Namespace     string `json:"namespace"`
	LabelSelector string `json:"labelSelector"`
	Time          string `json:"time"`            // RFC3339 absolute timestamp
	TTL           string `json:"ttl,omitempty"`  // Go duration string, e.g. "1h30m"; omit for no expiry
	Freeze        bool   `json:"freeze,omitempty"` // if true, clock does not advance past target
}

// UpdateTimeshiftRequest is the body of PATCH /timeshifts/{id}.
// Provide either Time (absolute) or Duration (relative advance); not both.
type UpdateTimeshiftRequest struct {
	Time     string `json:"time,omitempty"`     // RFC3339 absolute timestamp
	Duration string `json:"duration,omitempty"` // Go duration string to advance by, e.g. "24h" or "-1h"
	Freeze   bool   `json:"freeze,omitempty"`   // if true, switch to (or stay in) freeze mode
}

// TimeshiftResponse is returned by POST /timeshifts, GET /timeshifts/{id}, and PATCH /timeshifts/{id}.
type TimeshiftResponse struct {
	ID        string   `json:"id"`
	Namespace string   `json:"namespace"`
	Time      string   `json:"time"`                // RFC3339
	Frozen    bool     `json:"frozen,omitempty"`    // true when clock is frozen at Time
	ExpiresAt string   `json:"expiresAt,omitempty"` // RFC3339; absent when no TTL was set
	AppliedTo []string `json:"appliedTo"`           // "pod-name/container-name", sorted
}

// ListTimeshiftsResponse is returned by GET /timeshifts.
type ListTimeshiftsResponse struct {
	Timeshifts []TimeshiftResponse `json:"timeshifts"`
}

// ResolveResponse is returned by GET /resolve.
type ResolveResponse struct {
	Pods []ResolvedPod `json:"pods"`
}

// ResolvedPod describes one pod that would be affected by a timeshift with the
// given namespace and label selector. Only running containers are included.
type ResolvedPod struct {
	Name       string   `json:"name"`
	Namespace  string   `json:"namespace"`
	NodeIP     string   `json:"nodeIP"`
	Containers []string `json:"containers"` // running containers, sorted
}

// HandleStatus is the live injection state for one container, as read directly
// from the trampoline's state struct by the agent.
type HandleStatus struct {
	Generation uint32 `json:"generation"` // bumped on each SetTime/Freeze; 0 after initial Inject
	LastTarget string `json:"lastTarget"` // RFC3339, last time written by Inject or SetTime
	StateAddr  string `json:"stateAddr"`  // hex address of the state struct, for debugging
	PID        int32  `json:"pid"`        // host PID of the injected process
}

// ContainerStatusEntry is the live injection state for one container within a
// timeshift. Error is set (and Status is nil) when the agent GetStatus call fails.
type ContainerStatusEntry struct {
	Pod       string        `json:"pod"`
	Container string        `json:"container"`
	NodeIP    string        `json:"nodeIP"`
	Status    *HandleStatus `json:"status,omitempty"`
	Error     string        `json:"error,omitempty"`
}

// TimeshiftStatusResponse is returned by GET /timeshifts/{id}/status.
type TimeshiftStatusResponse struct {
	ID         string                 `json:"id"`
	Namespace  string                 `json:"namespace"`
	Containers []ContainerStatusEntry `json:"containers"`
}

// ErrorResponse is the JSON body of all 4xx/5xx responses.
type ErrorResponse struct {
	Error string `json:"error"`
}
