// Package sdk is the Go client library for the epochd controller API.
// It is safe to use from any OS and has no Linux-specific dependencies.
//
// Typical e2e test usage:
//
//	client := sdk.NewClient("http://epochd-controller.epochd.svc")
//	err := sdk.WithTime(ctx, client, "default", "app=my-svc", target, time.Hour,
//	    func(ctx context.Context) error {
//	        // assertions here — containers see target as the current time
//	        return nil
//	    },
//	)
package sdk

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"epochd/pkg/api"
)

// Client talks to the epochd controller over HTTP+JSON.
type Client struct {
	baseURL string
	http    *http.Client
}

// NewClient returns a Client that targets controllerURL (e.g. "http://localhost:8080").
// A zero-value http.Client with default timeouts is used; pass WithHTTPClient to
// override.
func NewClient(controllerURL string) *Client {
	return &Client{
		baseURL: controllerURL,
		http:    &http.Client{Timeout: 30 * time.Second},
	}
}

// WithHTTPClient returns a copy of c that uses the given HTTP client.
func (c *Client) WithHTTPClient(h *http.Client) *Client {
	cp := *c
	cp.http = h
	return &cp
}

// ---------------------------------------------------------------------------
// Timeshift — typed representation of a controller timeshift
// ---------------------------------------------------------------------------

// Timeshift is a parsed representation of api.TimeshiftResponse with typed
// time fields instead of RFC3339 strings.
type Timeshift struct {
	ID        string
	Namespace string
	Time      time.Time
	ExpiresAt time.Time
	AppliedTo []string // "pod/container", sorted
}

func timeshiftFromResponse(r api.TimeshiftResponse) (*Timeshift, error) {
	t, err := time.Parse(time.RFC3339, r.Time)
	if err != nil {
		return nil, fmt.Errorf("sdk: parse Time %q: %w", r.Time, err)
	}
	var exp time.Time
	if r.ExpiresAt != "" {
		exp, err = time.Parse(time.RFC3339, r.ExpiresAt)
		if err != nil {
			return nil, fmt.Errorf("sdk: parse ExpiresAt %q: %w", r.ExpiresAt, err)
		}
	}
	return &Timeshift{
		ID:        r.ID,
		Namespace: r.Namespace,
		Time:      t,
		ExpiresAt: exp, // zero value when no TTL was set
		AppliedTo: r.AppliedTo,
	}, nil
}

// ---------------------------------------------------------------------------
// APIError
// ---------------------------------------------------------------------------

// APIError is returned when the controller responds with a 4xx or 5xx status.
type APIError struct {
	StatusCode int
	Message    string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("epochd: HTTP %d: %s", e.StatusCode, e.Message)
}

// IsNotFound reports whether err is a 404 response from the controller.
func IsNotFound(err error) bool {
	var ae *APIError
	if ok := isAPIError(err, &ae); ok {
		return ae.StatusCode == http.StatusNotFound
	}
	return false
}

// IsConflict reports whether err is a 409 response from the controller,
// meaning one or more containers in the request are already targeted by an
// active timeshift. Delete or update the conflicting timeshift first.
func IsConflict(err error) bool {
	var ae *APIError
	if ok := isAPIError(err, &ae); ok {
		return ae.StatusCode == http.StatusConflict
	}
	return false
}

func isAPIError(err error, out **APIError) bool {
	if err == nil {
		return false
	}
	ae, ok := err.(*APIError)
	if ok {
		*out = ae
	}
	return ok
}

// ---------------------------------------------------------------------------
// CRUD methods
// ---------------------------------------------------------------------------

// CreateTimeshift creates a new timeshift for all running containers in ns
// that match labelSelector, setting their clock to target. Pass a positive ttl
// for automatic expiry; pass 0 for no expiry (the timeshift persists until
// explicitly deleted).
func (c *Client) CreateTimeshift(ctx context.Context, ns, labelSelector string, target time.Time, ttl time.Duration) (*Timeshift, error) {
	body := api.CreateTimeshiftRequest{
		Namespace:     ns,
		LabelSelector: labelSelector,
		Time:          target.UTC().Format(time.RFC3339),
	}
	if ttl > 0 {
		body.TTL = ttl.String()
	}
	var resp api.TimeshiftResponse
	if err := c.do(ctx, http.MethodPost, "/timeshifts", body, &resp); err != nil {
		return nil, err
	}
	return timeshiftFromResponse(resp)
}

// ListTimeshifts returns all active timeshifts, sorted oldest-first.
func (c *Client) ListTimeshifts(ctx context.Context) ([]Timeshift, error) {
	var resp api.ListTimeshiftsResponse
	if err := c.do(ctx, http.MethodGet, "/timeshifts", nil, &resp); err != nil {
		return nil, err
	}
	out := make([]Timeshift, 0, len(resp.Timeshifts))
	for _, r := range resp.Timeshifts {
		ts, err := timeshiftFromResponse(r)
		if err != nil {
			return nil, err
		}
		out = append(out, *ts)
	}
	return out, nil
}

// GetTimeshift returns the timeshift with the given id.
func (c *Client) GetTimeshift(ctx context.Context, id string) (*Timeshift, error) {
	var resp api.TimeshiftResponse
	if err := c.do(ctx, http.MethodGet, "/timeshifts/"+id, nil, &resp); err != nil {
		return nil, err
	}
	return timeshiftFromResponse(resp)
}

// UpdateTimeshift moves the target clock for an existing timeshift to target.
func (c *Client) UpdateTimeshift(ctx context.Context, id string, target time.Time) (*Timeshift, error) {
	body := api.UpdateTimeshiftRequest{
		Time: target.UTC().Format(time.RFC3339),
	}
	var resp api.TimeshiftResponse
	if err := c.do(ctx, http.MethodPatch, "/timeshifts/"+id, body, &resp); err != nil {
		return nil, err
	}
	return timeshiftFromResponse(resp)
}

// DeleteTimeshift resets all injected containers back to the real clock and
// removes the timeshift from the controller registry.
func (c *Client) DeleteTimeshift(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodDelete, "/timeshifts/"+id, nil, nil)
}

// TimeshiftStatus returns the live injection state for every container in the
// timeshift, as read from the trampoline state struct by each node agent.
func (c *Client) TimeshiftStatus(ctx context.Context, id string) (*api.TimeshiftStatusResponse, error) {
	var resp api.TimeshiftStatusResponse
	if err := c.do(ctx, http.MethodGet, "/timeshifts/"+id+"/status", nil, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// Resolve returns the pods and running containers that would be targeted by a
// timeshift with the given namespace and label selector. No injection is
// performed and no controller state is changed.
func (c *Client) Resolve(ctx context.Context, ns, labelSelector string) ([]api.ResolvedPod, error) {
	q := url.Values{}
	q.Set("namespace", ns)
	q.Set("selector", labelSelector)
	var resp api.ResolveResponse
	if err := c.do(ctx, http.MethodGet, "/resolve?"+q.Encode(), nil, &resp); err != nil {
		return nil, err
	}
	return resp.Pods, nil
}

// ---------------------------------------------------------------------------
// WithTime helper
// ---------------------------------------------------------------------------

// WithTime creates a timeshift, calls fn, then deletes the timeshift — even
// if fn returns an error. The timeshift id is available as ts.ID if fn needs
// to call UpdateTimeshift mid-test.
//
// ttl must be positive. WithTime is a scoped helper that always cleans up
// after itself; use CreateTimeshift directly if you need a timeshift that
// outlives a single function call.
//
// If both fn and the delete fail, both errors are combined into the returned
// error.
func WithTime(
	ctx context.Context,
	c *Client,
	ns, labelSelector string,
	target time.Time,
	ttl time.Duration,
	fn func(ctx context.Context, ts *Timeshift) error,
) error {
	if ttl <= 0 {
		return fmt.Errorf("sdk: WithTime requires a positive ttl; use CreateTimeshift for no-expiry timeshifts")
	}
	ts, err := c.CreateTimeshift(ctx, ns, labelSelector, target, ttl)
	if err != nil {
		return fmt.Errorf("sdk: WithTime create: %w", err)
	}

	fnErr := fn(ctx, ts)

	if delErr := c.DeleteTimeshift(ctx, ts.ID); delErr != nil {
		if fnErr != nil {
			return fmt.Errorf("sdk: WithTime fn failed (%w); also failed to delete timeshift %s: %v", fnErr, ts.ID, delErr)
		}
		return fmt.Errorf("sdk: WithTime delete timeshift %s: %w", ts.ID, delErr)
	}
	return fnErr
}

// ---------------------------------------------------------------------------
// HTTP transport
// ---------------------------------------------------------------------------

func (c *Client) do(ctx context.Context, method, path string, reqBody, respBody any) error {
	var bodyReader io.Reader
	if reqBody != nil {
		b, err := json.Marshal(reqBody)
		if err != nil {
			return fmt.Errorf("sdk: marshal request: %w", err)
		}
		bodyReader = bytes.NewReader(b)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, bodyReader)
	if err != nil {
		return fmt.Errorf("sdk: build request: %w", err)
	}
	if reqBody != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("sdk: %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		var errResp api.ErrorResponse
		_ = json.NewDecoder(resp.Body).Decode(&errResp)
		msg := errResp.Error
		if msg == "" {
			msg = resp.Status
		}
		return &APIError{StatusCode: resp.StatusCode, Message: msg}
	}

	if respBody != nil && resp.StatusCode != http.StatusNoContent {
		if err := json.NewDecoder(resp.Body).Decode(respBody); err != nil {
			return fmt.Errorf("sdk: decode response: %w", err)
		}
	}
	return nil
}
