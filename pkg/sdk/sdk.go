// Package sdk is the Go client library for the epochd controller API.
// It is safe to use from any OS and has no Linux-specific dependencies.
//
// Typical e2e test usage:
//
//	client := sdk.NewClient("http://epochd-controller.epochd.svc")
//	err := sdk.WithTime(ctx, client, "default", "app=my-svc", target, time.Hour,
//	    func(ctx context.Context, ts *sdk.Timeshift) error {
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

	"github.com/bkaznowski/epochd/pkg/api"
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
	Frozen    bool      // true when the clock does not advance past Time
	ExpiresAt time.Time // zero value when no TTL was set
	AppliedTo []string  // "pod/container", sorted
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
		Frozen:    r.Frozen,
		ExpiresAt: exp,
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
// that match labelSelector, setting their clock to target with time advancing
// at the real rate. Pass a positive ttl for automatic expiry; pass 0 for no expiry.
func (c *Client) CreateTimeshift(ctx context.Context, ns, labelSelector string, target time.Time, ttl time.Duration) (*Timeshift, error) {
	return c.createTimeshift(ctx, ns, labelSelector, target, ttl, false)
}

// CreateFrozenTimeshift creates a new timeshift with the clock frozen at target.
// Unlike CreateTimeshift, the containers see exactly target on every call to
// clock_gettime -- time never advances.
func (c *Client) CreateFrozenTimeshift(ctx context.Context, ns, labelSelector string, target time.Time, ttl time.Duration) (*Timeshift, error) {
	return c.createTimeshift(ctx, ns, labelSelector, target, ttl, true)
}

func (c *Client) createTimeshift(ctx context.Context, ns, labelSelector string, target time.Time, ttl time.Duration, freeze bool) (*Timeshift, error) {
	body := api.CreateTimeshiftRequest{
		Namespace:     ns,
		LabelSelector: labelSelector,
		Time:          target.UTC().Format(time.RFC3339),
		Freeze:        freeze,
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

// UpdateTimeshift moves the target clock for an existing timeshift to target
// in advancing mode. If the timeshift was frozen, it becomes advancing.
func (c *Client) UpdateTimeshift(ctx context.Context, id string, target time.Time) (*Timeshift, error) {
	return c.updateTimeshift(ctx, id, target, false)
}

// FreezeTimeshift updates the target clock to target and switches the timeshift
// to freeze mode. Every call to clock_gettime in the targeted containers returns
// exactly target until the next UpdateTimeshift or FreezeTimeshift call.
func (c *Client) FreezeTimeshift(ctx context.Context, id string, target time.Time) (*Timeshift, error) {
	return c.updateTimeshift(ctx, id, target, true)
}

// AdvanceTimeshift advances the timeshift's clock by d (may be negative to
// rewind). For advancing timeshifts the offset grows by d; for frozen ones the
// frozen point shifts by d. The mode (frozen or advancing) is preserved.
func (c *Client) AdvanceTimeshift(ctx context.Context, id string, d time.Duration) (*Timeshift, error) {
	body := api.UpdateTimeshiftRequest{Duration: d.String()}
	var resp api.TimeshiftResponse
	if err := c.do(ctx, http.MethodPatch, "/timeshifts/"+id, body, &resp); err != nil {
		return nil, err
	}
	return timeshiftFromResponse(resp)
}

func (c *Client) updateTimeshift(ctx context.Context, id string, target time.Time, freeze bool) (*Timeshift, error) {
	body := api.UpdateTimeshiftRequest{
		Time:   target.UTC().Format(time.RFC3339),
		Freeze: freeze,
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
// WaitForActive
// ---------------------------------------------------------------------------

// WaitForActive polls /timeshifts/{id}/status every 250 ms until every
// container in the timeshift reports a successful injection (Status non-nil,
// no Error field) or until ctx is cancelled.
//
// A per-container injection error is returned immediately rather than retried
// — it indicates the agent rejected the injection and retrying is not useful.
//
// WaitForActive is called automatically by WithTime and WithFrozenTime before
// the user callback is invoked. Callers using CreateTimeshift or
// CreateFrozenTimeshift directly can call this method to confirm injection.
func (c *Client) WaitForActive(ctx context.Context, id string) error {
	for {
		st, err := c.TimeshiftStatus(ctx, id)
		if err != nil {
			return fmt.Errorf("sdk: WaitForActive: %w", err)
		}

		allReady := true
		for _, cs := range st.Containers {
			if cs.Error != "" {
				return fmt.Errorf("sdk: WaitForActive: container %s/%s: %s",
					cs.Pod, cs.Container, cs.Error)
			}
			if cs.Status == nil {
				allReady = false
			}
		}
		if allReady {
			return nil
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
}

// ---------------------------------------------------------------------------
// WithTime / WithFrozenTime helpers
// ---------------------------------------------------------------------------

// WithTime creates a timeshift, waits for every targeted container to confirm
// injection via WaitForActive, calls fn, then deletes the timeshift — even if
// fn returns an error. The timeshift id is available as ts.ID if fn needs to
// call UpdateTimeshift mid-test.
//
// ttl must be positive. WithTime is a scoped helper that always cleans up
// after itself; use CreateTimeshift directly if you need a timeshift that
// outlives a single function call.
//
// If both fn and the delete fail, both errors are combined into the returned
// error. See WithFrozenTime for freeze mode.
func WithTime(
	ctx context.Context,
	c *Client,
	ns, labelSelector string,
	target time.Time,
	ttl time.Duration,
	fn func(ctx context.Context, ts *Timeshift) error,
) error {
	return withTimeMode(ctx, c, ns, labelSelector, target, ttl, false, fn)
}

// WithFrozenTime creates a timeshift with the clock frozen at target, waits
// for injection, calls fn, then deletes the timeshift.
// Every call to clock_gettime in the targeted containers returns exactly target
// for the duration of the callback.
func WithFrozenTime(
	ctx context.Context,
	c *Client,
	ns, labelSelector string,
	target time.Time,
	ttl time.Duration,
	fn func(ctx context.Context, ts *Timeshift) error,
) error {
	return withTimeMode(ctx, c, ns, labelSelector, target, ttl, true, fn)
}

func withTimeMode(
	ctx context.Context,
	c *Client,
	ns, labelSelector string,
	target time.Time,
	ttl time.Duration,
	freeze bool,
	fn func(ctx context.Context, ts *Timeshift) error,
) error {
	if ttl <= 0 {
		return fmt.Errorf("sdk: WithTime/WithFrozenTime requires a positive ttl; use CreateTimeshift for no-expiry timeshifts")
	}
	ts, err := c.createTimeshift(ctx, ns, labelSelector, target, ttl, freeze)
	if err != nil {
		return fmt.Errorf("sdk: WithTime create: %w", err)
	}

	if err := c.WaitForActive(ctx, ts.ID); err != nil {
		_ = c.DeleteTimeshift(ctx, ts.ID)
		return fmt.Errorf("sdk: WithTime: %w", err)
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
