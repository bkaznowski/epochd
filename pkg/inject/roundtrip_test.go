//go:build linux

package inject

import (
	"bufio"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"

	"epochd/pkg/procmem"
	"epochd/pkg/vdso"
)

// TestInjectRoundTrip is the phase-6 integration test for the complete
// inject → observe → reset → observe cycle:
//
//  1. Inject a +24 h offset into a running clock-printing child process.
//  2. Read several timestamps from its stdout and confirm they are ~24 h ahead.
//  3. Call h.SetTime(time.Now()) to snap the target back to the real clock — no
//     ptrace stop; this is a single process_vm_writev call.
//  4. Read several more timestamps and confirm they have returned to ~real time.
func TestInjectRoundTrip(t *testing.T) {
	if os.Getenv("EPOCHD_INJECT_E2E") == "" {
		t.Skip("set EPOCHD_INJECT_E2E=1 to run full injection test")
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}

	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	defer pr.Close()

	// Reuse the existing TestInjectHelperClock helper (helperEnv=2):
	// prints time.Now().Format(RFC3339Nano) every 100 ms.
	cmd := exec.Command(exe, "-test.run=TestInjectHelperClock", "-test.v")
	cmd.Env = append(os.Environ(), helperEnv+"=2")
	cmd.Stdout = pw
	cmd.SysProcAttr = &syscall.SysProcAttr{Ptrace: true}
	if err := cmd.Start(); err != nil {
		t.Fatalf("cmd.Start: %v", err)
	}
	pw.Close()
	t.Cleanup(func() { cmd.Process.Kill(); cmd.Wait() })

	pid := cmd.Process.Pid
	tr := procmem.NewTracer()
	if err := tr.FollowChild(pid); err != nil {
		t.Fatalf("FollowChild: %v", err)
	}

	info, err := vdso.Locate(pid)
	if err != nil {
		t.Fatalf("vdso.Locate: %v", err)
	}

	const fakeOffset = 24 * time.Hour
	target := time.Now().Add(fakeOffset)
	wantSec, wantNsec := diffSecNsec(target, time.Now())
	h, err := injectWithTracer(tr, pid, info.ClockGettimeAddr, wantSec, wantNsec)
	if err != nil {
		t.Fatalf("injectWithTracer: %v", err)
	}
	tr.Detach()

	const (
		wantEach  = 3             // timestamps to verify in each phase
		tolerance = 5 * time.Second
	)
	sc := bufio.NewScanner(pr)

	// -------------------------------------------------------------------------
	// Phase A — confirm +24 h offset is live.
	// -------------------------------------------------------------------------
	t.Log("phase A: verifying +24 h offset")
	phaseA := 0
	for sc.Scan() && phaseA < wantEach {
		ts, ok := parseRoundTripTimestamp(sc.Text())
		if !ok {
			continue
		}
		diff := time.Until(ts)
		if diff < fakeOffset-tolerance || diff > fakeOffset+tolerance {
			t.Errorf("phase A: printed time %v is %.3f h from now, want ~%.1f h",
				ts, diff.Hours(), fakeOffset.Hours())
		}
		t.Logf("phase A: %v  (offset %.3f h)", ts, diff.Hours())
		phaseA++
	}
	if phaseA < wantEach {
		t.Fatalf("phase A: received only %d/%d timestamps", phaseA, wantEach)
	}

	// -------------------------------------------------------------------------
	// Phase B — reset to real time, confirm timestamps return.
	// -------------------------------------------------------------------------
	t.Log("phase B: calling SetTime(now) and verifying reset")
	if err := h.SetTime(time.Now()); err != nil {
		t.Fatalf("SetTime: %v", err)
	}

	phaseB := 0
	for sc.Scan() && phaseB < wantEach {
		ts, ok := parseRoundTripTimestamp(sc.Text())
		if !ok {
			continue
		}
		// Skip timestamps still draining from the pipe buffer that were printed
		// before SetTime reached the trampoline (they'll still be ~24 h ahead).
		if time.Until(ts).Abs() > fakeOffset/2 {
			t.Logf("phase B: discarding pre-reset timestamp %v", ts)
			continue
		}
		diff := time.Until(ts).Abs()
		if diff > tolerance {
			t.Errorf("phase B: printed time %v is %v from real now, want <%v",
				ts, diff.Round(time.Millisecond), tolerance)
		}
		t.Logf("phase B: %v  (Δ %v from real)", ts, diff.Round(time.Millisecond))
		phaseB++
	}
	if phaseB < wantEach {
		t.Fatalf("phase B: received only %d/%d real-time timestamps after reset", phaseB, wantEach)
	}
}

// parseRoundTripTimestamp trims a scanner line and parses it as RFC3339Nano.
// Returns (zero, false) for test-framework noise ("=== RUN", "--- PASS", blank).
func parseRoundTripTimestamp(line string) (time.Time, bool) {
	line = strings.TrimSpace(line)
	if line == "" || strings.HasPrefix(line, "=== ") || strings.HasPrefix(line, "--- ") {
		return time.Time{}, false
	}
	ts, err := time.Parse(time.RFC3339Nano, line)
	if err != nil {
		return time.Time{}, false
	}
	return ts, true
}
