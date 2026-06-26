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

// WithSession calls setup to add processes to a new session targeting target,
// then calls fn. t.Cleanup resets all handles and waits on any commands added
// via session.Start.
func WithSession(t *testing.T, target time.Time, setup func(*Session) error, fn func(*testing.T, *Session)) {
	t.Helper()
	s := NewSession(target)
	if err := setup(s); err != nil {
		t.Fatalf("faketime.WithSession: setup: %v", err)
	}
	t.Cleanup(func() {
		if err := s.Reset(); err != nil {
			t.Logf("faketime.WithSession: cleanup Reset: %v", err)
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
