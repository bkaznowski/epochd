package sdk

import (
	"context"
	"os"
	"testing"
	"time"
)

// WithTimeT is a test helper version of WithTime. It registers cleanup via
// t.Cleanup so the timeshift is deleted even if the test panics, and calls
// t.Fatalf on create or delete failure rather than returning an error.
//
// The test is skipped automatically when the EPOCHD_URL environment variable
// is not set, making it safe to include in regular unit test runs where no
// controller is available.
//
// Usage:
//
//	sdk.WithTimeT(t, client, "default", "app=svc", target, time.Hour,
//	    func(t *testing.T, ts *sdk.Timeshift) {
//	        // assertions here
//	    },
//	)
func WithTimeT(
	t *testing.T,
	c *Client,
	ns, labelSelector string,
	target time.Time,
	ttl time.Duration,
	fn func(t *testing.T, ts *Timeshift),
) {
	t.Helper()
	if os.Getenv("EPOCHD_URL") == "" {
		t.Skip("EPOCHD_URL not set; skipping timeshift test")
	}
	if ttl <= 0 {
		t.Fatal("sdk: WithTimeT requires a positive ttl")
	}

	ts, err := c.CreateTimeshift(context.Background(), ns, labelSelector, target, ttl)
	if err != nil {
		t.Fatalf("sdk: WithTimeT create: %v", err)
	}

	t.Cleanup(func() {
		if err := c.DeleteTimeshift(context.Background(), ts.ID); err != nil && !IsNotFound(err) {
			t.Errorf("sdk: WithTimeT cleanup delete %s: %v", ts.ID, err)
		}
	})

	fn(t, ts)
}
