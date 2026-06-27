//go:build linux

package inject

import (
	"os"
	"os/exec"
	"syscall"
	"testing"
	"time"
)

// spawnHelperFresh launches the test binary with Ptrace: true but does NOT call
// FollowChild. The returned PID is ready for InjectAtTimeFollowChild and similar
// functions that handle FollowChild internally.
func spawnHelperFresh(t *testing.T) int {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	cmd := exec.Command(exe, "-test.run=TestInjectHelperBlock", "-test.v")
	cmd.Env = append(os.Environ(), helperEnv+"=1")
	cmd.SysProcAttr = &syscall.SysProcAttr{Ptrace: true}
	if err := cmd.Start(); err != nil {
		t.Fatalf("cmd.Start: %v", err)
	}
	t.Cleanup(func() { cmd.Process.Kill(); cmd.Wait() }) //nolint:errcheck
	return cmd.Process.Pid
}

func TestInjectAtTimeFollowChild(t *testing.T) {
	pid := spawnHelperFresh(t)
	target := time.Now().Add(24 * time.Hour)

	h, err := InjectAtTimeFollowChild(pid, target)
	if err != nil {
		t.Fatalf("InjectAtTimeFollowChild: %v", err)
	}
	if h.PID != pid {
		t.Errorf("Handle.PID = %d, want %d", h.PID, pid)
	}
	if h.StateAddr == 0 {
		t.Error("Handle.StateAddr is zero")
	}
	if h.Generation() != 0 {
		t.Errorf("Generation after inject = %d, want 0", h.Generation())
	}

	// SetTime should update the fake clock and increment the generation counter.
	if err := h.SetTime(target.Add(time.Hour)); err != nil {
		t.Fatalf("SetTime: %v", err)
	}
	if h.Generation() != 1 {
		t.Errorf("Generation after SetTime = %d, want 1", h.Generation())
	}

	// Child must still be alive after detach.
	if err := syscall.Kill(pid, 0); err != nil {
		t.Errorf("child not alive after injection: %v", err)
	}
}

func TestInjectFrozenFollowChild(t *testing.T) {
	pid := spawnHelperFresh(t)
	target := time.Now().Add(24 * time.Hour)

	h, err := InjectFrozenFollowChild(pid, target)
	if err != nil {
		t.Fatalf("InjectFrozenFollowChild: %v", err)
	}
	if h.PID != pid {
		t.Errorf("Handle.PID = %d, want %d", h.PID, pid)
	}

	// Freeze should update to a new timestamp and increment the generation.
	if err := h.Freeze(target.Add(48 * time.Hour)); err != nil {
		t.Fatalf("Freeze: %v", err)
	}
	if h.Generation() != 1 {
		t.Errorf("Generation after Freeze = %d, want 1", h.Generation())
	}
}

func TestInjectAtTimeFollowChildKeepTracer(t *testing.T) {
	pid := spawnHelperFresh(t)
	target := time.Now().Add(24 * time.Hour)

	h, tr, err := InjectAtTimeFollowChildKeepTracer(pid, target)
	if err != nil {
		t.Fatalf("InjectAtTimeFollowChildKeepTracer: %v", err)
	}
	if h == nil {
		t.Fatal("Handle is nil")
	}
	if tr == nil {
		t.Fatal("Tracer is nil")
	}
	defer tr.Detach() //nolint:errcheck

	// ChildHandle should share the same StateAddr as the parent handle.
	fakePID := pid + 1000
	ch := ChildHandle(h, fakePID)
	if ch.PID != fakePID {
		t.Errorf("ChildHandle.PID = %d, want %d", ch.PID, fakePID)
	}
	if ch.StateAddr != h.StateAddr {
		t.Errorf("ChildHandle.StateAddr = 0x%x, want 0x%x (same as parent)", ch.StateAddr, h.StateAddr)
	}
}

func TestInjectFrozenFollowChildKeepTracer(t *testing.T) {
	pid := spawnHelperFresh(t)
	target := time.Now().Add(24 * time.Hour)

	h, tr, err := InjectFrozenFollowChildKeepTracer(pid, target)
	if err != nil {
		t.Fatalf("InjectFrozenFollowChildKeepTracer: %v", err)
	}
	if h == nil {
		t.Fatal("Handle is nil")
	}
	if tr == nil {
		t.Fatal("Tracer is nil")
	}
	defer tr.Detach() //nolint:errcheck
}
