//go:build linux

package inject

import (
	"errors"
	"os/exec"
	"testing"
	"time"
)

// Adopt must recognise a process that carries the hooks (even though the
// Handle that injected it is long gone) and refuse one that does not; the
// adopted Handle must be fully usable, uninstall included.
func TestAdoptRecognisesAnInjectedProcess(t *testing.T) {
	cmd, sc := startBusyClockChild(t)
	pid := cmd.Process.Pid

	if _, err := Adopt(pid); !errors.Is(err, ErrNotInjected) {
		t.Fatalf("Adopt of a process that was never injected = %v, want ErrNotInjected", err)
	}

	original := injectOrSkip(t, pid, time.Now().Add(24*time.Hour))

	adopted, err := Adopt(pid)
	if err != nil {
		t.Fatalf("Adopt of an injected process: %v", err)
	}
	if adopted.StateAddr != original.StateAddr || adopted.PID != pid {
		t.Errorf("Adopt returned state 0x%x pid %d, want state 0x%x pid %d",
			adopted.StateAddr, adopted.PID, original.StateAddr, pid)
	}

	later := time.Now().Add(96 * time.Hour)
	if err := adopted.SetTime(later); err != nil {
		t.Fatalf("SetTime through the adopted handle: %v", err)
	}
	awaitTimestampNear(t, sc, later, 10*time.Second)

	if err := adopted.Uninstall(); err != nil {
		t.Fatalf("Uninstall through the adopted handle: %v", err)
	}
	assertVDSOPristine(t, pid)
	if _, err := Adopt(pid); !errors.Is(err, ErrNotInjected) {
		t.Errorf("Adopt after Uninstall = %v, want ErrNotInjected", err)
	}
}

// After exec the process has a fresh address space: the recorded state address
// means nothing there, and writing to it would corrupt whatever the new program
// mapped. The write must be refused, not performed.
func TestWriteAfterExecReportsHooksLost(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available")
	}
	if _, err := exec.LookPath("sleep"); err != nil {
		t.Skip("sleep not available")
	}
	// The shell waits on a child, then replaces itself with sleep.
	cmd := exec.Command("sh", "-c", "sleep 0.6; exec sleep 30")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cmd.Process.Kill(); cmd.Wait() }) //nolint:errcheck
	pid := cmd.Process.Pid

	h := injectOrSkip(t, pid, time.Now().Add(time.Hour))
	if err := h.SetTime(time.Now().Add(2 * time.Hour)); err != nil {
		t.Fatalf("SetTime before the exec: %v", err)
	}

	time.Sleep(1200 * time.Millisecond) // the shell has exec'd into sleep by now
	if err := h.SetTime(time.Now().Add(3 * time.Hour)); !errors.Is(err, ErrHooksLost) {
		t.Errorf("SetTime after the process exec'd = %v, want ErrHooksLost", err)
	}
	if err := h.Freeze(time.Now()); !errors.Is(err, ErrHooksLost) {
		t.Errorf("Freeze after the process exec'd = %v, want ErrHooksLost", err)
	}
}
