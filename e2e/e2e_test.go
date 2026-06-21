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

// clockprinterImage returns the container image used for e2e test pods.
// It prints time.Now().Format(time.RFC3339) once per second without forking,
// so the injected process's log output directly reflects fake time.
func clockprinterImage() string {
	if img := os.Getenv("CLOCKPRINTER_IMAGE"); img != "" {
		return img
	}
	return "epochd-clockprinter:dev"
}

// TestTimeshiftDate deploys a clockprinter pod, shifts its clock one year
// forward via epochd, reads recent log output from the injected process, and
// asserts the year in that output matches the shifted time.
//
// The clockprinter binary calls time.Now() directly (no fork+exec), so the
// vDSO patch persists and the logs reflect fake time. kubectl exec would not
// work because exec replaces the address space, discarding the vDSO patch.
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

	exec.Command("kubectl", "create", "namespace", ns).Run() //nolint:errcheck
	t.Cleanup(func() {
		runCmd(t, "kubectl", "delete", "namespace", ns, "--ignore-not-found=true")
	})

	runCmd(t, "kubectl", "run", pod,
		"-n", ns,
		"--image="+clockprinterImage(),
		"--image-pull-policy=Never",
		"--restart=Never",
		"--labels=app=clocktest",
	)
	runCmd(t, "kubectl", "wait",
		"-n", ns,
		"pod/"+pod,
		"--for=condition=Ready",
		"--timeout=60s",
	)

	target := time.Now().UTC().AddDate(1, 0, 0).Truncate(time.Second)
	wantYear := fmt.Sprintf("%d", target.Year())

	err := sdk.WithTime(ctx, client, ns, "app=clocktest", target, 10*time.Minute,
		func(ctx context.Context, ts *sdk.Timeshift) error {
			// The clockprinter process prints once per second; wait long enough
			// for at least two new lines to appear after the trampoline is live.
			time.Sleep(3 * time.Second)

			logs := strings.TrimSpace(outputCmd(t,
				"kubectl", "logs", "-n", ns, pod, "--tail=5",
			))
			got := lastYear(logs)
			if got != wantYear {
				return fmt.Errorf("year in logs = %q, want %q (target %v)\nlogs:\n%s",
					got, wantYear, target, logs)
			}
			t.Logf("clock injection verified: year = %s", got)
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
		"--image="+clockprinterImage(),
		"--image-pull-policy=Never",
		"--restart=Never",
		"--labels=app=updatetest",
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
			time.Sleep(3 * time.Second)

			logs := strings.TrimSpace(outputCmd(t,
				"kubectl", "logs", "-n", ns, pod, "--tail=5",
			))
			got := lastYear(logs)
			if got != fmt.Sprintf("%d", firstTarget.Year()) {
				return fmt.Errorf("before update: year = %q, want %d\nlogs:\n%s",
					got, firstTarget.Year(), logs)
			}

			// Update to 5 years ahead.
			if _, err := client.UpdateTimeshift(ctx, ts.ID, secondTarget); err != nil {
				return fmt.Errorf("UpdateTimeshift: %w", err)
			}
			time.Sleep(3 * time.Second)

			logs = strings.TrimSpace(outputCmd(t,
				"kubectl", "logs", "-n", ns, pod, "--tail=5",
			))
			got = lastYear(logs)
			if got != fmt.Sprintf("%d", secondTarget.Year()) {
				return fmt.Errorf("after update: year = %q, want %d\nlogs:\n%s",
					got, secondTarget.Year(), logs)
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

// lastYear scans newline-separated log lines from the bottom (most recent) and
// returns the year portion of the first RFC3339 timestamp found.
func lastYear(logs string) string {
	lines := strings.Split(logs, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		ts, err := time.Parse(time.RFC3339, line)
		if err == nil {
			return fmt.Sprintf("%d", ts.Year())
		}
	}
	return ""
}
