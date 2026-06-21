//go:build e2e

// Package e2e_test contains end-to-end tests that run against a real
// Kubernetes cluster with epochd deployed. Run via 'make e2e', which sets up
// a kind cluster, deploys epochd, and passes EPOCHD_URL to this suite.
//
// The tests are excluded from 'go test ./...' by the e2e build tag.
package e2e_test

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"epochd/pkg/sdk"
)

// TestTimeshiftDate deploys a busybox pod, shifts its clock one year forward
// via epochd, execs `date +%Y` into the container, and asserts the reported
// year matches the shifted time.
func TestTimeshiftDate(t *testing.T) {
	controllerURL := os.Getenv("EPOCHD_URL")
	if controllerURL == "" {
		t.Skip("EPOCHD_URL not set — run via 'make e2e'")
	}

	ctx := context.Background()
	client := sdk.NewClient(controllerURL)

	const (
		ns  = "epochd-e2e"
		pod = "clocktest"
	)

	// Namespace — create idempotently; delete on cleanup.
	exec.Command("kubectl", "create", "namespace", ns).Run() //nolint:errcheck
	t.Cleanup(func() {
		runCmd(t, "kubectl", "delete", "namespace", ns, "--ignore-not-found=true")
	})

	// Deploy a long-lived busybox pod. The `date` binary is what we probe.
	runCmd(t, "kubectl", "run", pod,
		"-n", ns,
		"--image=busybox:latest",
		"--restart=Never",
		"--labels=app=clocktest",
		"--", "sleep", "3600",
	)
	runCmd(t, "kubectl", "wait",
		"-n", ns,
		"pod/"+pod,
		"--for=condition=Ready",
		"--timeout=60s",
	)

	// Shift the pod's clock exactly one year forward.
	target := time.Now().UTC().AddDate(1, 0, 0).Truncate(time.Second)
	wantYear := fmt.Sprintf("%d", target.Year())

	err := sdk.WithTime(ctx, client, ns, "app=clocktest", target, 10*time.Minute,
		func(ctx context.Context, ts *sdk.Timeshift) error {
			// Give the vDSO trampoline a moment to be active before sampling.
			time.Sleep(500 * time.Millisecond)

			got := strings.TrimSpace(outputCmd(t,
				"kubectl", "exec", "-n", ns, pod, "--", "date", "+%Y",
			))
			if got != wantYear {
				return fmt.Errorf("date +%%Y = %q, want %q (target %v)", got, wantYear, target)
			}
			t.Logf("clock injection verified: date +%%Y = %s", got)
			return nil
		},
	)
	if err != nil {
		t.Fatalf("WithTime: %v", err)
	}
}

// TestTimeshiftUpdate verifies that UpdateTimeshift changes the time the
// container observes mid-test.
func TestTimeshiftUpdate(t *testing.T) {
	controllerURL := os.Getenv("EPOCHD_URL")
	if controllerURL == "" {
		t.Skip("EPOCHD_URL not set — run via 'make e2e'")
	}

	ctx := context.Background()
	client := sdk.NewClient(controllerURL)

	const (
		ns  = "epochd-e2e"
		pod = "updatetest"
	)

	exec.Command("kubectl", "create", "namespace", ns).Run() //nolint:errcheck
	t.Cleanup(func() {
		runCmd(t, "kubectl", "delete", "pod", pod, "-n", ns, "--ignore-not-found=true")
	})

	runCmd(t, "kubectl", "run", pod,
		"-n", ns,
		"--image=busybox:latest",
		"--restart=Never",
		"--labels=app=updatetest",
		"--", "sleep", "3600",
	)
	runCmd(t, "kubectl", "wait",
		"-n", ns,
		"pod/"+pod,
		"--for=condition=Ready",
		"--timeout=60s",
	)

	firstTarget := time.Now().UTC().AddDate(1, 0, 0).Truncate(time.Second)
	secondTarget := time.Now().UTC().AddDate(5, 0, 0).Truncate(time.Second)

	err := sdk.WithTime(ctx, client, ns, "app=updatetest", firstTarget, 10*time.Minute,
		func(ctx context.Context, ts *sdk.Timeshift) error {
			time.Sleep(500 * time.Millisecond)

			got := strings.TrimSpace(outputCmd(t,
				"kubectl", "exec", "-n", ns, pod, "--", "date", "+%Y",
			))
			if got != fmt.Sprintf("%d", firstTarget.Year()) {
				return fmt.Errorf("before update: date +%%Y = %q, want %d", got, firstTarget.Year())
			}

			// Update to 5 years ahead.
			if _, err := client.UpdateTimeshift(ctx, ts.ID, secondTarget); err != nil {
				return fmt.Errorf("UpdateTimeshift: %w", err)
			}
			time.Sleep(500 * time.Millisecond)

			got = strings.TrimSpace(outputCmd(t,
				"kubectl", "exec", "-n", ns, pod, "--", "date", "+%Y",
			))
			if got != fmt.Sprintf("%d", secondTarget.Year()) {
				return fmt.Errorf("after update: date +%%Y = %q, want %d", got, secondTarget.Year())
			}
			t.Logf("UpdateTimeshift verified: %d → %d", firstTarget.Year(), secondTarget.Year())
			return nil
		},
	)
	if err != nil {
		t.Fatalf("WithTime: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func runCmd(t *testing.T, name string, args ...string) {
	t.Helper()
	out, err := exec.Command(name, args...).CombinedOutput()
	if err != nil {
		t.Fatalf("%s %v: %v\n%s", name, args, err, out)
	}
}

func outputCmd(t *testing.T, name string, args ...string) string {
	t.Helper()
	out, err := exec.Command(name, args...).CombinedOutput()
	if err != nil {
		t.Fatalf("%s %v: %v\n%s", name, args, err, out)
	}
	return string(out)
}
