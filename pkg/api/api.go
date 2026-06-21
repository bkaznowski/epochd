// Package api holds the HTTP+JSON request and response types for the epochd
// controller's REST API. These types are shared between the controller
// implementation and any Go client (e.g. the e2e test SDK in phase 10).
// No build tag — the types are pure structs with no OS-specific dependencies.
package api

// CreateTimeshiftRequest is the body of POST /timeshifts.
type CreateTimeshiftRequest struct {
	Namespace     string `json:"namespace"`
	LabelSelector string `json:"labelSelector"`
	Time          string `json:"time"`           // RFC3339 absolute timestamp
	TTL           string `json:"ttl,omitempty"` // Go duration string, e.g. "1h30m"; omit for no expiry
}

// UpdateTimeshiftRequest is the body of PATCH /timeshifts/{id}.
type UpdateTimeshiftRequest struct {
	Time string `json:"time"` // RFC3339 absolute timestamp
}

// TimeshiftResponse is returned by POST /timeshifts, GET /timeshifts/{id}, and PATCH /timeshifts/{id}.
type TimeshiftResponse struct {
	ID        string   `json:"id"`
	Namespace string   `json:"namespace"`
	Time      string   `json:"time"`                // RFC3339
	ExpiresAt string   `json:"expiresAt,omitempty"` // RFC3339; absent when no TTL was set
	AppliedTo []string `json:"appliedTo"`           // "pod-name/container-name", sorted
}

// ListTimeshiftsResponse is returned by GET /timeshifts.
type ListTimeshiftsResponse struct {
	Timeshifts []TimeshiftResponse `json:"timeshifts"`
}

// ErrorResponse is the JSON body of all 4xx/5xx responses.
type ErrorResponse struct {
	Error string `json:"error"`
}
