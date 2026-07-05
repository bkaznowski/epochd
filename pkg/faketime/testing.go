//go:build linux

package faketime

import (
	"os/exec"
	"testing"
	"time"
)

// WithProcess starts cmd with fake time, calls fn, then kills and waits for
// the process and resets its clock. t.Cleanup handles teardown so it runs even
// when fn calls t.Fatal. No elevated permissions required.
//
// The caller must not call cmd.Start() before passing cmd to WithProcess.
func WithProcess(t *testing.T, cmd *exec.Cmd, target time.Time, fn func(*testing.T, *Handle)) {
	t.Helper()
	h, err := Start(cmd, target)
	if err != nil {
		t.Fatalf("faketime.WithProcess: %v", err)
	}
	t.Cleanup(func() {
		if err := h.Reset(); err != nil {
			t.Logf("faketime.WithProcess: cleanup Reset: %v", err)
		}
		cmd.Process.Kill() //nolint:errcheck
		cmd.Wait()         //nolint:errcheck
	})
	fn(t, h)
}

// WithFrozenProcess starts cmd with the clock frozen at target, calls fn, then
// kills the process and resets the clock. t.Cleanup handles teardown so it
// runs even when fn calls t.Fatal. No elevated permissions required.
//
// The caller must not call cmd.Start() before passing cmd to WithFrozenProcess.
func WithFrozenProcess(t *testing.T, cmd *exec.Cmd, target time.Time, fn func(*testing.T, *Handle)) {
	t.Helper()
	h, err := StartFrozen(cmd, target)
	if err != nil {
		t.Fatalf("faketime.WithFrozenProcess: %v", err)
	}
	t.Cleanup(func() {
		if err := h.Reset(); err != nil {
			t.Logf("faketime.WithFrozenProcess: cleanup Reset: %v", err)
		}
		cmd.Process.Kill() //nolint:errcheck
		cmd.Wait()         //nolint:errcheck
	})
	fn(t, h)
}

// WithPID attaches to an already-running process with fake time, calls fn,
// then resets the clock. Requires CAP_SYS_PTRACE.
func WithPID(t *testing.T, pid int, target time.Time, fn func(*testing.T, *Handle)) {
	t.Helper()
	h, err := Attach(pid, target)
	if err != nil {
		t.Fatalf("faketime.WithPID: %v", err)
	}
	t.Cleanup(func() {
		if err := h.Reset(); err != nil {
			t.Logf("faketime.WithPID: cleanup Reset: %v", err)
		}
	})
	fn(t, h)
}

// WithFrozenPID attaches to an already-running process with the clock frozen
// at target, calls fn, then resets the clock. Requires CAP_SYS_PTRACE.
func WithFrozenPID(t *testing.T, pid int, target time.Time, fn func(*testing.T, *Handle)) {
	t.Helper()
	h, err := AttachFrozen(pid, target)
	if err != nil {
		t.Fatalf("faketime.WithFrozenPID: %v", err)
	}
	t.Cleanup(func() {
		if err := h.Reset(); err != nil {
			t.Logf("faketime.WithFrozenPID: cleanup Reset: %v", err)
		}
	})
	fn(t, h)
}

// WithFrozenChildTracker starts cmd with the clock frozen at target and child
// tracking enabled, calls fn, then resets all clocks, stops the tracker
// goroutine, and kills the process. t.Cleanup handles teardown.
//
// The caller must not call cmd.Start() before passing cmd to WithFrozenChildTracker.
func WithFrozenChildTracker(t *testing.T, cmd *exec.Cmd, target time.Time, fn func(*testing.T, *ChildTracker)) {
	t.Helper()
	ct, err := StartFrozenWithTracking(cmd, target)
	if err != nil {
		t.Fatalf("faketime.WithFrozenChildTracker: %v", err)
	}
	t.Cleanup(func() {
		if err := ct.Reset(); err != nil {
			t.Logf("faketime.WithFrozenChildTracker: cleanup Reset: %v", err)
		}
		if err := ct.Close(); err != nil {
			t.Logf("faketime.WithFrozenChildTracker: cleanup Close: %v", err)
		}
		cmd.Process.Kill() //nolint:errcheck
		cmd.Wait()         //nolint:errcheck
	})
	fn(t, ct)
}

// WithChildTracker starts cmd with fake time and child-process tracking, calls
// fn, then resets all clocks, stops the tracker goroutine, and kills the
// process. t.Cleanup handles teardown so it runs even when fn calls t.Fatal.
// No elevated permissions required.
//
// The caller must not call cmd.Start() before passing cmd to WithChildTracker.
func WithChildTracker(t *testing.T, cmd *exec.Cmd, target time.Time, fn func(*testing.T, *ChildTracker)) {
	t.Helper()
	ct, err := StartWithTracking(cmd, target)
	if err != nil {
		t.Fatalf("faketime.WithChildTracker: %v", err)
	}
	t.Cleanup(func() {
		if err := ct.Reset(); err != nil {
			t.Logf("faketime.WithChildTracker: cleanup Reset: %v", err)
		}
		if err := ct.Close(); err != nil {
			t.Logf("faketime.WithChildTracker: cleanup Close: %v", err)
		}
		cmd.Process.Kill() //nolint:errcheck
		cmd.Wait()         //nolint:errcheck
	})
	fn(t, ct)
}

// WithSession calls setup to add processes to a new session targeting target,
// then calls fn. t.Cleanup resets all handles, closes any active trackers, and
// waits on any commands added via session.Start. Pass WithTracking() in opts to
// enable automatic child-process injection.
func WithSession(t *testing.T, target time.Time, setup func(*Session) error, fn func(*testing.T, *Session), opts ...SessionOption) {
	t.Helper()
	s := NewSession(target, opts...)
	if err := setup(s); err != nil {
		t.Fatalf("faketime.WithSession: setup: %v", err)
	}
	t.Cleanup(func() {
		if err := s.Reset(); err != nil {
			t.Logf("faketime.WithSession: cleanup Reset: %v", err)
		}
		if err := s.Close(); err != nil {
			t.Logf("faketime.WithSession: cleanup Close: %v", err)
		}
		s.mu.Lock()
		cmds := make([]*exec.Cmd, len(s.cmds))
		copy(cmds, s.cmds)
		s.mu.Unlock()
		for _, cmd := range cmds {
			cmd.Process.Kill() //nolint:errcheck
			cmd.Wait()         //nolint:errcheck
		}
	})
	fn(t, s)
}
