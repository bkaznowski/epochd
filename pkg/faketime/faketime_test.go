//go:build linux

package faketime

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

const helperEnv = "EPOCHD_FAKETIME_HELPER"

// TestFaketimeHelper is the clock-printing child used by all faketime tests.
// When run by the test framework helperEnv is unset and the test is skipped.
// When spawned as a child process with helperEnv=1 it prints RFC3339Nano
// timestamps every 100 ms until killed.
func TestFaketimeHelper(t *testing.T) {
	if os.Getenv(helperEnv) != "1" {
		t.Skip()
	}
	for {
		fmt.Println(time.Now().Format(time.RFC3339Nano))
		time.Sleep(100 * time.Millisecond)
	}
}

// TestStartSingleProcess verifies the full Start → inject → Reset cycle using
// the FollowChild path (no elevated permissions required):
//
//  1. Start a helper with +24 h fake time.
//  2. Read timestamps from its stdout and confirm they are ~24 h ahead.
//  3. Call Reset; confirm timestamps return to real time.
func TestStartSingleProcess(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}

	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	defer pr.Close()

	const fakeOffset = 24 * time.Hour
	target := time.Now().Add(fakeOffset)

	cmd := exec.Command(exe, "-test.run=TestFaketimeHelper", "-test.v")
	cmd.Env = append(os.Environ(), helperEnv+"=1")
	cmd.Stdout = pw

	h, err := Start(cmd, target)
	if err != nil {
		pw.Close()
		t.Fatalf("Start: %v", err)
	}
	pw.Close()
	t.Cleanup(func() { cmd.Process.Kill(); cmd.Wait() })

	const (
		wantEach  = 3
		tolerance = 5 * time.Second
	)
	sc := bufio.NewScanner(pr)

	// Phase A: verify +24 h offset is active.
	t.Log("phase A: verifying +24h offset")
	phaseA := 0
	for sc.Scan() && phaseA < wantEach {
		ts, ok := parseFaketimeTimestamp(sc.Text())
		if !ok {
			continue
		}
		diff := time.Until(ts)
		if diff < fakeOffset-tolerance || diff > fakeOffset+tolerance {
			t.Errorf("phase A: timestamp %v is %v from now, want ~%v",
				ts, diff.Round(time.Millisecond), fakeOffset)
		}
		t.Logf("phase A: %v  (offset %v)", ts, diff.Round(time.Millisecond))
		phaseA++
	}
	if phaseA < wantEach {
		t.Fatalf("phase A: received only %d/%d timestamps", phaseA, wantEach)
	}

	// Phase B: reset to real time.
	t.Log("phase B: calling Reset and verifying real-time return")
	if err := h.Reset(); err != nil {
		t.Fatalf("Reset: %v", err)
	}
	phaseB := 0
	for sc.Scan() && phaseB < wantEach {
		ts, ok := parseFaketimeTimestamp(sc.Text())
		if !ok {
			continue
		}
		// Drain any timestamps still buffered from before Reset reached the trampoline.
		if time.Until(ts).Abs() > fakeOffset/2 {
			t.Logf("phase B: discarding pre-reset timestamp %v", ts)
			continue
		}
		diff := time.Until(ts).Abs()
		if diff > tolerance {
			t.Errorf("phase B: timestamp %v is %v from real now, want <%v",
				ts, diff.Round(time.Millisecond), tolerance)
		}
		t.Logf("phase B: %v  (Δ %v from real)", ts, diff.Round(time.Millisecond))
		phaseB++
	}
	if phaseB < wantEach {
		t.Fatalf("phase B: received only %d/%d real-time timestamps after Reset", phaseB, wantEach)
	}
}

// TestSessionTwoProcesses verifies the Session API: two child processes share
// the same target, and Session.SetTime updates both without error.
func TestSessionTwoProcesses(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}

	newPipe := func() (*os.File, *os.File) {
		pr, pw, err := os.Pipe()
		if err != nil {
			t.Fatalf("os.Pipe: %v", err)
		}
		return pr, pw
	}
	newHelper := func(pw *os.File) *exec.Cmd {
		cmd := exec.Command(exe, "-test.run=TestFaketimeHelper", "-test.v")
		cmd.Env = append(os.Environ(), helperEnv+"=1")
		cmd.Stdout = pw
		return cmd
	}

	const fakeOffset = 24 * time.Hour
	target := time.Now().Add(fakeOffset)
	s := NewSession(target)

	pr1, pw1 := newPipe()
	pr2, pw2 := newPipe()
	defer pr1.Close()
	defer pr2.Close()

	cmd1 := newHelper(pw1)
	if err := s.Start(cmd1); err != nil {
		pw1.Close(); pw2.Close()
		t.Fatalf("Session.Start cmd1: %v", err)
	}
	pw1.Close()

	cmd2 := newHelper(pw2)
	if err := s.Start(cmd2); err != nil {
		pw2.Close()
		t.Fatalf("Session.Start cmd2: %v", err)
	}
	pw2.Close()

	t.Cleanup(func() {
		s.Reset()
		cmd1.Process.Kill(); cmd1.Wait()
		cmd2.Process.Kill(); cmd2.Wait()
	})

	if s.Len() != 2 {
		t.Fatalf("Len() = %d, want 2", s.Len())
	}

	const tolerance = 5 * time.Second

	// Verify each process sees the initial +24h target.
	readFirst := func(pr *os.File, label string) {
		sc := bufio.NewScanner(pr)
		for sc.Scan() {
			ts, ok := parseFaketimeTimestamp(sc.Text())
			if !ok {
				continue
			}
			diff := time.Until(ts)
			if diff < fakeOffset-tolerance || diff > fakeOffset+tolerance {
				t.Errorf("%s: timestamp %v is %v from now, want ~%v",
					label, ts, diff.Round(time.Millisecond), fakeOffset)
			}
			t.Logf("%s: %v  (offset %v)", label, ts, diff.Round(time.Millisecond))
			return
		}
		t.Errorf("%s: no timestamp received", label)
	}
	readFirst(pr1, "proc1 initial")
	readFirst(pr2, "proc2 initial")

	// Advance to +48h; both handles must update without error.
	const newOffset = 48 * time.Hour
	newTarget := time.Now().Add(newOffset)
	if err := s.SetTime(newTarget); err != nil {
		t.Fatalf("Session.SetTime: %v", err)
	}

	// Verify each process has transitioned to +48h (draining pre-update timestamps).
	readAfterUpdate := func(pr *os.File, label string) {
		sc := bufio.NewScanner(pr)
		for sc.Scan() {
			ts, ok := parseFaketimeTimestamp(sc.Text())
			if !ok {
				continue
			}
			diff := time.Until(ts)
			if diff < fakeOffset && diff < newOffset-tolerance {
				t.Logf("%s: discarding pre-update timestamp %v", label, ts)
				continue
			}
			if diff < newOffset-tolerance || diff > newOffset+tolerance {
				t.Errorf("%s: post-update timestamp %v is %v from now, want ~%v",
					label, ts, diff.Round(time.Millisecond), newOffset)
			}
			t.Logf("%s: %v  (offset %v)", label, ts, diff.Round(time.Millisecond))
			return
		}
		t.Errorf("%s: no timestamp received after SetTime", label)
	}
	readAfterUpdate(pr1, "proc1 after SetTime")
	readAfterUpdate(pr2, "proc2 after SetTime")
}

// TestWithSession verifies the WithSession helper starts the session, calls fn,
// and cleans up without deadlocking.
func TestWithSession(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}

	target := time.Now().Add(24 * time.Hour)
	called := false

	WithSession(t, target,
		func(s *Session) error {
			pr, pw, err := os.Pipe()
			if err != nil {
				return err
			}
			cmd := exec.Command(exe, "-test.run=TestFaketimeHelper", "-test.v")
			cmd.Env = append(os.Environ(), helperEnv+"=1")
			cmd.Stdout = pw
			if err := s.Start(cmd); err != nil {
				pw.Close(); pr.Close()
				return err
			}
			pw.Close()
			pr.Close() // we only need the process running; discard output
			return nil
		},
		func(t *testing.T, s *Session) {
			if s.Len() != 1 {
				t.Fatalf("session Len() = %d, want 1", s.Len())
			}
			called = true
		},
	)

	if !called {
		t.Error("WithSession: fn was never called")
	}
}

// TestSessionWithTracking verifies that a session created with WithTracking
// tracks the child process and that Session.Close shuts down cleanly.
func TestSessionWithTracking(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}

	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	defer pr.Close()

	const fakeOffset = 24 * time.Hour
	target := time.Now().Add(fakeOffset)

	s := NewSession(target, WithTracking())

	cmd := exec.Command(exe, "-test.run=TestFaketimeHelper", "-test.v")
	cmd.Env = append(os.Environ(), helperEnv+"=1")
	cmd.Stdout = pw

	if err := s.Start(cmd); err != nil {
		pw.Close()
		t.Fatalf("Session.Start: %v", err)
	}
	pw.Close()

	if s.Len() != 1 {
		t.Errorf("Len() = %d, want 1", s.Len())
	}

	// Read a few timestamps to confirm the fake offset is active.
	const (
		wantEach  = 2
		tolerance = 5 * time.Second
	)
	sc := bufio.NewScanner(pr)
	got := 0
	for sc.Scan() && got < wantEach {
		ts, ok := parseFaketimeTimestamp(sc.Text())
		if !ok {
			continue
		}
		diff := time.Until(ts)
		if diff < fakeOffset-tolerance || diff > fakeOffset+tolerance {
			t.Errorf("timestamp %v is %v from now, want ~%v",
				ts, diff.Round(time.Millisecond), fakeOffset)
		}
		t.Logf("tracked: %v  (offset %v)", ts, diff.Round(time.Millisecond))
		got++
	}
	if got < wantEach {
		t.Fatalf("received only %d/%d timestamps", got, wantEach)
	}

	// Advance the session clock; children are updated via applyAll.
	const newOffset = 48 * time.Hour
	if err := s.SetTime(time.Now().Add(newOffset)); err != nil {
		t.Fatalf("Session.SetTime: %v", err)
	}

	// Close should stop the tracker goroutine without error.
	if err := s.Close(); err != nil {
		t.Fatalf("Session.Close: %v", err)
	}

	cmd.Process.Kill() //nolint:errcheck
	cmd.Wait()         //nolint:errcheck
}

// TestWithSessionTracking verifies the WithSession helper with the WithTracking option.
func TestWithSessionTracking(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}

	target := time.Now().Add(24 * time.Hour)

	WithSession(t, target,
		func(s *Session) error {
			pr, pw, err := os.Pipe()
			if err != nil {
				return err
			}
			cmd := exec.Command(exe, "-test.run=TestFaketimeHelper", "-test.v")
			cmd.Env = append(os.Environ(), helperEnv+"=1")
			cmd.Stdout = pw
			if err := s.Start(cmd); err != nil {
				pw.Close()
				pr.Close()
				return err
			}
			pw.Close()
			pr.Close()
			return nil
		},
		func(t *testing.T, s *Session) {
			if s.Len() != 1 {
				t.Fatalf("Len() = %d, want 1", s.Len())
			}
		},
		WithTracking(),
	)
}

// TestSessionIsFrozen verifies Session.IsFrozen reflects Freeze/SetTime transitions.
func TestSessionIsFrozen(t *testing.T) {
	target := time.Now().Add(24 * time.Hour)
	s := NewSession(target)

	if s.IsFrozen() {
		t.Error("IsFrozen() = true after NewSession (advancing mode)")
	}
	if err := s.Freeze(target); err != nil {
		// No handles yet — Freeze only updates internal state, should not error.
		t.Fatalf("Freeze: %v", err)
	}
	if !s.IsFrozen() {
		t.Error("IsFrozen() = false after Freeze")
	}
	if err := s.SetTime(target); err != nil {
		t.Fatalf("SetTime: %v", err)
	}
	if s.IsFrozen() {
		t.Error("IsFrozen() = true after SetTime")
	}
}

// TestChildTrackerIsFrozen verifies ChildTracker.IsFrozen delegates to the parent handle.
func TestChildTrackerIsFrozen(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	cmd := exec.Command(exe, "-test.run=TestFaketimeHelper", "-test.v")
	cmd.Env = append(os.Environ(), helperEnv+"=1")
	target := time.Now().Add(24 * time.Hour)

	ct, err := StartWithTracking(cmd, target)
	if err != nil {
		t.Fatalf("StartWithTracking: %v", err)
	}
	t.Cleanup(func() { ct.Close(); cmd.Process.Kill(); cmd.Wait() }) //nolint:errcheck

	if ct.IsFrozen() {
		t.Error("IsFrozen() = true after StartWithTracking (advancing mode)")
	}
	if err := ct.Freeze(target); err != nil {
		t.Fatalf("Freeze: %v", err)
	}
	if !ct.IsFrozen() {
		t.Error("IsFrozen() = false after Freeze")
	}
}

// TestHandleIsFrozen verifies IsFrozen reflects mode transitions.
func TestHandleIsFrozen(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	cmd := exec.Command(exe, "-test.run=TestFaketimeHelper", "-test.v")
	cmd.Env = append(os.Environ(), helperEnv+"=1")

	target := time.Now().Add(24 * time.Hour)
	h, err := Start(cmd, target)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { cmd.Process.Kill(); cmd.Wait() }) //nolint:errcheck

	if h.IsFrozen() {
		t.Error("IsFrozen() = true after Start (advancing mode)")
	}
	if err := h.Freeze(target); err != nil {
		t.Fatalf("Freeze: %v", err)
	}
	if !h.IsFrozen() {
		t.Error("IsFrozen() = false after Freeze")
	}
	if err := h.SetTime(target); err != nil {
		t.Fatalf("SetTime: %v", err)
	}
	if h.IsFrozen() {
		t.Error("IsFrozen() = true after SetTime (advancing mode)")
	}
}

// TestChildTrackerPIDs verifies PIDs returns the parent PID first and reflects
// the current tracked set.
func TestChildTrackerPIDs(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	cmd := exec.Command(exe, "-test.run=TestFaketimeHelper", "-test.v")
	cmd.Env = append(os.Environ(), helperEnv+"=1")

	ct, err := StartWithTracking(cmd, time.Now().Add(24*time.Hour))
	if err != nil {
		t.Fatalf("StartWithTracking: %v", err)
	}
	t.Cleanup(func() {
		ct.Close()         //nolint:errcheck
		cmd.Process.Kill() //nolint:errcheck
		cmd.Wait()         //nolint:errcheck
	})

	pids := ct.PIDs()
	if len(pids) == 0 {
		t.Fatal("PIDs() returned empty slice")
	}
	if pids[0] != cmd.Process.Pid {
		t.Errorf("PIDs()[0] = %d, want parent PID %d", pids[0], cmd.Process.Pid)
	}
}

// TestHandleMethods verifies PID, IsAlive, and EffectiveTime on a live handle.
func TestHandleMethods(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}

	cmd := exec.Command(exe, "-test.run=TestFaketimeHelper", "-test.v")
	cmd.Env = append(os.Environ(), helperEnv+"=1")

	const fakeOffset = 24 * time.Hour
	target := time.Now().Add(fakeOffset)

	h, err := Start(cmd, target)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { cmd.Process.Kill(); cmd.Wait() }) //nolint:errcheck

	t.Run("PID", func(t *testing.T) {
		if got := h.PID(); got != cmd.Process.Pid {
			t.Errorf("PID() = %d, want %d", got, cmd.Process.Pid)
		}
	})

	t.Run("IsAlive_running", func(t *testing.T) {
		if !h.IsAlive() {
			t.Error("IsAlive() = false for a running process")
		}
	})

	t.Run("EffectiveTime_advancing", func(t *testing.T) {
		got := h.EffectiveTime()
		diff := time.Until(got)
		const tolerance = 5 * time.Second
		if diff < fakeOffset-tolerance || diff > fakeOffset+tolerance {
			t.Errorf("EffectiveTime() = %v (diff %v from now), want ~%v",
				got, diff.Round(time.Millisecond), fakeOffset)
		}
	})

	t.Run("EffectiveTime_frozen", func(t *testing.T) {
		frozen := time.Now().Add(48 * time.Hour)
		if err := h.Freeze(frozen); err != nil {
			t.Fatalf("Freeze: %v", err)
		}
		got := h.EffectiveTime()
		if !got.Equal(frozen) {
			t.Errorf("EffectiveTime() after Freeze = %v, want %v", got, frozen)
		}
	})

	t.Run("IsAlive_dead", func(t *testing.T) {
		cmd.Process.Kill() //nolint:errcheck
		cmd.Wait()         //nolint:errcheck
		if h.IsAlive() {
			t.Error("IsAlive() = true after process was killed")
		}
	})
}

// ptraceScopeAllows returns true if PTRACE_ATTACH is permitted (ptrace_scope <= 1).
func ptraceScopeAllows(t *testing.T) bool {
	t.Helper()
	data, err := os.ReadFile("/proc/sys/kernel/yama/ptrace_scope")
	if err != nil {
		// No Yama — PTRACE_ATTACH is unrestricted.
		return true
	}
	scope := strings.TrimSpace(string(data))
	return scope == "0" || scope == "1"
}

// startHelper starts the clock-printing helper process without ptrace so that
// Attach / AttachFrozen can PTRACE_ATTACH to it from the outside.
func startHelper(t *testing.T) (*exec.Cmd, *os.File) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	cmd := exec.Command(exe, "-test.run=TestFaketimeHelper", "-test.v")
	cmd.Env = append(os.Environ(), helperEnv+"=1")
	cmd.Stdout = pw
	if err := cmd.Start(); err != nil {
		pw.Close(); pr.Close()
		t.Fatalf("cmd.Start: %v", err)
	}
	pw.Close()
	t.Cleanup(func() { cmd.Process.Kill(); cmd.Wait() }) //nolint:errcheck
	return cmd, pr
}

// TestAttach verifies Attach injects a fake time offset into a running process
// and that Reset restores real time. Skipped when ptrace_scope > 1.
func TestAttach(t *testing.T) {
	if !ptraceScopeAllows(t) {
		t.Skip("ptrace_scope > 1: PTRACE_ATTACH not permitted")
	}

	cmd, pr := startHelper(t)
	defer pr.Close()

	const fakeOffset = 24 * time.Hour
	target := time.Now().Add(fakeOffset)

	h, err := Attach(cmd.Process.Pid, target)
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}

	const (
		wantEach  = 3
		tolerance = 5 * time.Second
	)
	sc := bufio.NewScanner(pr)

	t.Log("phase A: verifying +24h offset after Attach")
	got := 0
	for sc.Scan() && got < wantEach {
		ts, ok := parseFaketimeTimestamp(sc.Text())
		if !ok {
			continue
		}
		diff := time.Until(ts)
		if diff < fakeOffset-tolerance || diff > fakeOffset+tolerance {
			t.Errorf("phase A: timestamp %v is %v from now, want ~%v",
				ts, diff.Round(time.Millisecond), fakeOffset)
		}
		t.Logf("phase A: %v  (offset %v)", ts, diff.Round(time.Millisecond))
		got++
	}
	if got < wantEach {
		t.Fatalf("phase A: received only %d/%d timestamps", got, wantEach)
	}

	t.Log("phase B: calling Reset and verifying real-time return")
	if err := h.Reset(); err != nil {
		t.Fatalf("Reset: %v", err)
	}
	got = 0
	for sc.Scan() && got < wantEach {
		ts, ok := parseFaketimeTimestamp(sc.Text())
		if !ok {
			continue
		}
		if time.Until(ts).Abs() > fakeOffset/2 {
			t.Logf("phase B: discarding pre-reset timestamp %v", ts)
			continue
		}
		diff := time.Until(ts).Abs()
		if diff > tolerance {
			t.Errorf("phase B: timestamp %v is %v from real now, want <%v",
				ts, diff.Round(time.Millisecond), tolerance)
		}
		t.Logf("phase B: %v  (Δ %v from real)", ts, diff.Round(time.Millisecond))
		got++
	}
	if got < wantEach {
		t.Fatalf("phase B: received only %d/%d real-time timestamps after Reset", got, wantEach)
	}
}

// TestAttachFrozen verifies AttachFrozen pins the clock at a fixed instant and
// that the clock stays frozen across multiple reads.
func TestAttachFrozen(t *testing.T) {
	if !ptraceScopeAllows(t) {
		t.Skip("ptrace_scope > 1: PTRACE_ATTACH not permitted")
	}

	cmd, pr := startHelper(t)
	defer pr.Close()

	frozen := time.Now().Add(24 * time.Hour).Truncate(time.Second)
	h, err := AttachFrozen(cmd.Process.Pid, frozen)
	if err != nil {
		t.Fatalf("AttachFrozen: %v", err)
	}

	if got := h.EffectiveTime(); !got.Equal(frozen) {
		t.Errorf("EffectiveTime() = %v, want %v", got, frozen)
	}

	const wantEach = 3
	sc := bufio.NewScanner(pr)
	got := 0
	for sc.Scan() && got < wantEach {
		ts, ok := parseFaketimeTimestamp(sc.Text())
		if !ok {
			continue
		}
		if !ts.Equal(frozen) {
			t.Errorf("frozen timestamp %v != %v", ts, frozen)
		}
		t.Logf("frozen: %v", ts)
		got++
	}
	if got < wantEach {
		t.Fatalf("received only %d/%d frozen timestamps", got, wantEach)
	}
}

// TestWithPID verifies the WithPID helper attaches, calls fn, and resets on cleanup.
// Skipped when ptrace_scope > 1.
func TestWithPID(t *testing.T) {
	if !ptraceScopeAllows(t) {
		t.Skip("ptrace_scope > 1: PTRACE_ATTACH not permitted")
	}

	cmd, pr := startHelper(t)
	defer pr.Close()

	const fakeOffset = 24 * time.Hour
	target := time.Now().Add(fakeOffset)
	called := false

	WithPID(t, cmd.Process.Pid, target, func(t *testing.T, h *Handle) {
		called = true
		if h.PID() != cmd.Process.Pid {
			t.Errorf("PID() = %d, want %d", h.PID(), cmd.Process.Pid)
		}
		const (
			wantEach  = 2
			tolerance = 5 * time.Second
		)
		sc := bufio.NewScanner(pr)
		got := 0
		for sc.Scan() && got < wantEach {
			ts, ok := parseFaketimeTimestamp(sc.Text())
			if !ok {
				continue
			}
			diff := time.Until(ts)
			if diff < fakeOffset-tolerance || diff > fakeOffset+tolerance {
				t.Errorf("timestamp %v is %v from now, want ~%v",
					ts, diff.Round(time.Millisecond), fakeOffset)
			}
			got++
		}
		if got < wantEach {
			t.Fatalf("received only %d/%d timestamps", got, wantEach)
		}
	})

	if !called {
		t.Error("WithPID: fn was never called")
	}
}

// TestSessionAttach verifies that Session.Attach adds a running process to the
// session and that SetTime propagates to it. Skipped when ptrace_scope > 1.
func TestSessionAttach(t *testing.T) {
	if !ptraceScopeAllows(t) {
		t.Skip("ptrace_scope > 1: PTRACE_ATTACH not permitted")
	}

	cmd, pr := startHelper(t)
	defer pr.Close()

	const fakeOffset = 24 * time.Hour
	target := time.Now().Add(fakeOffset)
	s := NewSession(target)
	t.Cleanup(func() { s.Reset() }) //nolint:errcheck

	if err := s.Attach(cmd.Process.Pid); err != nil {
		t.Fatalf("Session.Attach: %v", err)
	}
	if s.Len() != 1 {
		t.Fatalf("Len() = %d, want 1", s.Len())
	}

	const (
		wantEach  = 2
		tolerance = 5 * time.Second
	)
	sc := bufio.NewScanner(pr)

	// Verify initial offset.
	got := 0
	for sc.Scan() && got < wantEach {
		ts, ok := parseFaketimeTimestamp(sc.Text())
		if !ok {
			continue
		}
		diff := time.Until(ts)
		if diff < fakeOffset-tolerance || diff > fakeOffset+tolerance {
			t.Errorf("initial: timestamp %v is %v from now, want ~%v",
				ts, diff.Round(time.Millisecond), fakeOffset)
		}
		got++
	}
	if got < wantEach {
		t.Fatalf("initial: received only %d/%d timestamps", got, wantEach)
	}

	// Advance to +48h via Session.SetTime.
	const newOffset = 48 * time.Hour
	if err := s.SetTime(time.Now().Add(newOffset)); err != nil {
		t.Fatalf("Session.SetTime: %v", err)
	}
	got = 0
	for sc.Scan() && got < wantEach {
		ts, ok := parseFaketimeTimestamp(sc.Text())
		if !ok {
			continue
		}
		diff := time.Until(ts)
		if diff < fakeOffset && diff < newOffset-tolerance {
			t.Logf("discarding pre-update timestamp %v", ts)
			continue
		}
		if diff < newOffset-tolerance || diff > newOffset+tolerance {
			t.Errorf("after SetTime: timestamp %v is %v from now, want ~%v",
				ts, diff.Round(time.Millisecond), newOffset)
		}
		t.Logf("after SetTime: %v  (offset %v)", ts, diff.Round(time.Millisecond))
		got++
	}
	if got < wantEach {
		t.Fatalf("after SetTime: received only %d/%d timestamps", got, wantEach)
	}
}

// TestAttachWithTracking verifies AttachWithTracking attaches to a running
// process, injects fake time, and that Close shuts down cleanly.
func TestAttachWithTracking(t *testing.T) {
	if !ptraceScopeAllows(t) {
		t.Skip("ptrace_scope > 1: PTRACE_ATTACH not permitted")
	}

	cmd, pr := startHelper(t)
	defer pr.Close()

	const fakeOffset = 24 * time.Hour
	target := time.Now().Add(fakeOffset)

	ct, err := AttachWithTracking(cmd.Process.Pid, target)
	if err != nil {
		t.Fatalf("AttachWithTracking: %v", err)
	}

	if ct.Handle.PID() != cmd.Process.Pid {
		t.Errorf("Handle.PID() = %d, want %d", ct.Handle.PID(), cmd.Process.Pid)
	}

	const (
		wantEach  = 2
		tolerance = 5 * time.Second
	)
	sc := bufio.NewScanner(pr)
	got := 0
	for sc.Scan() && got < wantEach {
		ts, ok := parseFaketimeTimestamp(sc.Text())
		if !ok {
			continue
		}
		diff := time.Until(ts)
		if diff < fakeOffset-tolerance || diff > fakeOffset+tolerance {
			t.Errorf("timestamp %v is %v from now, want ~%v",
				ts, diff.Round(time.Millisecond), fakeOffset)
		}
		t.Logf("tracked: %v  (offset %v)", ts, diff.Round(time.Millisecond))
		got++
	}
	if got < wantEach {
		t.Fatalf("received only %d/%d timestamps", got, wantEach)
	}

	if err := ct.Close(); err != nil {
		t.Fatalf("ChildTracker.Close: %v", err)
	}
}

// TestAttachFrozenWithTracking verifies AttachFrozenWithTracking pins the clock
// and that Close detaches cleanly.
func TestAttachFrozenWithTracking(t *testing.T) {
	if !ptraceScopeAllows(t) {
		t.Skip("ptrace_scope > 1: PTRACE_ATTACH not permitted")
	}

	cmd, pr := startHelper(t)
	defer pr.Close()

	frozen := time.Now().Add(24 * time.Hour).Truncate(time.Second)
	ct, err := AttachFrozenWithTracking(cmd.Process.Pid, frozen)
	if err != nil {
		t.Fatalf("AttachFrozenWithTracking: %v", err)
	}

	if got := ct.Handle.EffectiveTime(); !got.Equal(frozen) {
		t.Errorf("EffectiveTime() = %v, want %v", got, frozen)
	}

	const wantEach = 2
	sc := bufio.NewScanner(pr)
	got := 0
	for sc.Scan() && got < wantEach {
		ts, ok := parseFaketimeTimestamp(sc.Text())
		if !ok {
			continue
		}
		if !ts.Equal(frozen) {
			t.Errorf("frozen timestamp %v != %v", ts, frozen)
		}
		t.Logf("frozen: %v", ts)
		got++
	}
	if got < wantEach {
		t.Fatalf("received only %d/%d frozen timestamps", got, wantEach)
	}

	if err := ct.Close(); err != nil {
		t.Fatalf("ChildTracker.Close: %v", err)
	}
}

// TestSessionAttachWithTracking verifies that Session.Attach on a WithTracking
// session uses AttachWithTracking (returns a ChildTracker), and that Close is
// called on cleanup.
func TestSessionAttachWithTracking(t *testing.T) {
	if !ptraceScopeAllows(t) {
		t.Skip("ptrace_scope > 1: PTRACE_ATTACH not permitted")
	}

	cmd, pr := startHelper(t)
	defer pr.Close()

	const fakeOffset = 24 * time.Hour
	target := time.Now().Add(fakeOffset)
	s := NewSession(target, WithTracking())
	t.Cleanup(func() {
		s.Reset() //nolint:errcheck
		s.Close() //nolint:errcheck
	})

	if err := s.Attach(cmd.Process.Pid); err != nil {
		t.Fatalf("Session.Attach (WithTracking): %v", err)
	}
	if s.Len() != 1 {
		t.Fatalf("Len() = %d, want 1", s.Len())
	}

	const (
		wantEach  = 2
		tolerance = 5 * time.Second
	)
	sc := bufio.NewScanner(pr)
	got := 0
	for sc.Scan() && got < wantEach {
		ts, ok := parseFaketimeTimestamp(sc.Text())
		if !ok {
			continue
		}
		diff := time.Until(ts)
		if diff < fakeOffset-tolerance || diff > fakeOffset+tolerance {
			t.Errorf("timestamp %v is %v from now, want ~%v",
				ts, diff.Round(time.Millisecond), fakeOffset)
		}
		t.Logf("session+tracking: %v  (offset %v)", ts, diff.Round(time.Millisecond))
		got++
	}
	if got < wantEach {
		t.Fatalf("received only %d/%d timestamps", got, wantEach)
	}
}

// TestSessionAttachFrozen verifies that Session.Attach correctly uses
// AttachFrozen when the session is in frozen mode.
func TestSessionAttachFrozen(t *testing.T) {
	if !ptraceScopeAllows(t) {
		t.Skip("ptrace_scope > 1: PTRACE_ATTACH not permitted")
	}

	cmd, pr := startHelper(t)
	defer pr.Close()

	frozen := time.Now().Add(24 * time.Hour).Truncate(time.Second)
	s := NewSession(frozen)
	if err := s.Freeze(frozen); err != nil {
		t.Fatalf("Session.Freeze: %v", err)
	}
	t.Cleanup(func() { s.Reset() }) //nolint:errcheck

	if err := s.Attach(cmd.Process.Pid); err != nil {
		t.Fatalf("Session.Attach (frozen session): %v", err)
	}

	const wantEach = 2
	sc := bufio.NewScanner(pr)
	got := 0
	for sc.Scan() && got < wantEach {
		ts, ok := parseFaketimeTimestamp(sc.Text())
		if !ok {
			continue
		}
		if !ts.Equal(frozen) {
			t.Errorf("frozen timestamp %v != %v", ts, frozen)
		}
		t.Logf("frozen: %v", ts)
		got++
	}
	if got < wantEach {
		t.Fatalf("received only %d/%d frozen timestamps", got, wantEach)
	}
}

// TestSessionPruneAndPIDs verifies Session.Prune removes dead handles and
// Session.PIDs reflects the current live set.
func TestSessionPruneAndPIDs(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	newCmd := func() *exec.Cmd {
		cmd := exec.Command(exe, "-test.run=TestFaketimeHelper", "-test.v")
		cmd.Env = append(os.Environ(), helperEnv+"=1")
		return cmd
	}

	target := time.Now().Add(24 * time.Hour)
	s := NewSession(target)
	t.Cleanup(func() { s.Reset() }) //nolint:errcheck

	cmd1 := newCmd()
	cmd2 := newCmd()
	if err := s.Start(cmd1); err != nil {
		t.Fatalf("Start cmd1: %v", err)
	}
	if err := s.Start(cmd2); err != nil {
		t.Fatalf("Start cmd2: %v", err)
	}
	t.Cleanup(func() { cmd1.Process.Kill(); cmd1.Wait() }) //nolint:errcheck
	t.Cleanup(func() { cmd2.Process.Kill(); cmd2.Wait() }) //nolint:errcheck

	if s.Len() != 2 {
		t.Fatalf("Len() = %d, want 2", s.Len())
	}
	pids := s.PIDs()
	if len(pids) != 2 {
		t.Fatalf("PIDs() len = %d, want 2", len(pids))
	}
	wantPIDs := map[int]bool{cmd1.Process.Pid: true, cmd2.Process.Pid: true}
	for _, pid := range pids {
		if !wantPIDs[pid] {
			t.Errorf("PIDs() contains unexpected PID %d", pid)
		}
	}

	// Kill cmd1 and verify Prune removes it.
	cmd1.Process.Kill() //nolint:errcheck
	cmd1.Wait()         //nolint:errcheck
	// Give the OS a moment to mark the PID as gone.
	time.Sleep(20 * time.Millisecond)

	n := s.Prune()
	if n != 1 {
		t.Errorf("Prune() = %d, want 1", n)
	}
	if s.Len() != 1 {
		t.Errorf("Len() after Prune = %d, want 1", s.Len())
	}
	pids = s.PIDs()
	if len(pids) != 1 || pids[0] != cmd2.Process.Pid {
		t.Errorf("PIDs() after Prune = %v, want [%d]", pids, cmd2.Process.Pid)
	}
}

// TestSessionSetTimeAutoprune verifies that SetTime silently drops handles for
// processes that have exited (ESRCH) rather than returning an error.
func TestSessionSetTimeAutoprune(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	newCmd := func() *exec.Cmd {
		cmd := exec.Command(exe, "-test.run=TestFaketimeHelper", "-test.v")
		cmd.Env = append(os.Environ(), helperEnv+"=1")
		return cmd
	}

	target := time.Now().Add(24 * time.Hour)
	s := NewSession(target)

	cmd1 := newCmd()
	cmd2 := newCmd()
	if err := s.Start(cmd1); err != nil {
		t.Fatalf("Start cmd1: %v", err)
	}
	if err := s.Start(cmd2); err != nil {
		t.Fatalf("Start cmd2: %v", err)
	}
	t.Cleanup(func() {
		s.Reset()          //nolint:errcheck
		cmd1.Process.Kill() //nolint:errcheck
		cmd1.Wait()         //nolint:errcheck
		cmd2.Process.Kill() //nolint:errcheck
		cmd2.Wait()         //nolint:errcheck
	})

	// Kill cmd1 — its handle will get ESRCH on the next SetTime.
	cmd1.Process.Kill() //nolint:errcheck
	cmd1.Wait()         //nolint:errcheck
	time.Sleep(20 * time.Millisecond)

	// SetTime must succeed and auto-remove the dead handle.
	if err := s.SetTime(time.Now().Add(48 * time.Hour)); err != nil {
		t.Fatalf("SetTime after one process died: %v", err)
	}
	if s.Len() != 1 {
		t.Errorf("Len() after auto-prune = %d, want 1", s.Len())
	}
}

// TestWithFrozenProcess verifies the WithFrozenProcess helper starts a frozen
// process and calls fn.
func TestWithFrozenProcess(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}

	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	defer pr.Close()

	frozen := time.Now().Add(24 * time.Hour).Truncate(time.Second)
	cmd := exec.Command(exe, "-test.run=TestFaketimeHelper", "-test.v")
	cmd.Env = append(os.Environ(), helperEnv+"=1")
	cmd.Stdout = pw

	called := false
	WithFrozenProcess(t, cmd, frozen, func(t *testing.T, h *Handle) {
		pw.Close()
		called = true
		if got := h.EffectiveTime(); !got.Equal(frozen) {
			t.Errorf("EffectiveTime() = %v, want %v", got, frozen)
		}
		const wantEach = 2
		sc := bufio.NewScanner(pr)
		got := 0
		for sc.Scan() && got < wantEach {
			ts, ok := parseFaketimeTimestamp(sc.Text())
			if !ok {
				continue
			}
			if !ts.Equal(frozen) {
				t.Errorf("frozen timestamp %v != %v", ts, frozen)
			}
			got++
		}
		if got < wantEach {
			t.Fatalf("received only %d/%d frozen timestamps", got, wantEach)
		}
	})
	if !called {
		t.Error("WithFrozenProcess: fn was never called")
	}
}

// TestWithFrozenChildTracker verifies the WithFrozenChildTracker helper starts
// a frozen tracked process and calls fn.
func TestWithFrozenChildTracker(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}

	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	defer pr.Close()

	frozen := time.Now().Add(24 * time.Hour).Truncate(time.Second)
	cmd := exec.Command(exe, "-test.run=TestFaketimeHelper", "-test.v")
	cmd.Env = append(os.Environ(), helperEnv+"=1")
	cmd.Stdout = pw

	called := false
	WithFrozenChildTracker(t, cmd, frozen, func(t *testing.T, ct *ChildTracker) {
		pw.Close()
		called = true
		if got := ct.Handle.EffectiveTime(); !got.Equal(frozen) {
			t.Errorf("EffectiveTime() = %v, want %v", got, frozen)
		}
		const wantEach = 2
		sc := bufio.NewScanner(pr)
		got := 0
		for sc.Scan() && got < wantEach {
			ts, ok := parseFaketimeTimestamp(sc.Text())
			if !ok {
				continue
			}
			if !ts.Equal(frozen) {
				t.Errorf("frozen timestamp %v != %v", ts, frozen)
			}
			got++
		}
		if got < wantEach {
			t.Fatalf("received only %d/%d frozen timestamps", got, wantEach)
		}
	})
	if !called {
		t.Error("WithFrozenChildTracker: fn was never called")
	}
}

// TestWithFrozenPID verifies the WithFrozenPID helper (requires ptrace_scope <= 1).
func TestWithFrozenPID(t *testing.T) {
	if !ptraceScopeAllows(t) {
		t.Skip("ptrace_scope > 1: PTRACE_ATTACH not permitted")
	}

	cmd, pr := startHelper(t)
	defer pr.Close()

	frozen := time.Now().Add(24 * time.Hour).Truncate(time.Second)
	called := false

	WithFrozenPID(t, cmd.Process.Pid, frozen, func(t *testing.T, h *Handle) {
		called = true
		if got := h.EffectiveTime(); !got.Equal(frozen) {
			t.Errorf("EffectiveTime() = %v, want %v", got, frozen)
		}
		const wantEach = 2
		sc := bufio.NewScanner(pr)
		got := 0
		for sc.Scan() && got < wantEach {
			ts, ok := parseFaketimeTimestamp(sc.Text())
			if !ok {
				continue
			}
			if !ts.Equal(frozen) {
				t.Errorf("frozen timestamp %v != %v", ts, frozen)
			}
			got++
		}
		if got < wantEach {
			t.Fatalf("received only %d/%d frozen timestamps", got, wantEach)
		}
	})
	if !called {
		t.Error("WithFrozenPID: fn was never called")
	}
}

// TestChildTrackerBulkMethods verifies SetTime, Freeze, Advance, and Reset on a
// ChildTracker (parent process only — no forked children needed to exercise the
// bulk update path).
func TestChildTrackerBulkMethods(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}

	newTrackedHelper := func(t *testing.T) (*ChildTracker, *bufio.Scanner) {
		t.Helper()
		pr, pw, err := os.Pipe()
		if err != nil {
			t.Fatalf("os.Pipe: %v", err)
		}
		cmd := exec.Command(exe, "-test.run=TestFaketimeHelper", "-test.v")
		cmd.Env = append(os.Environ(), helperEnv+"=1")
		cmd.Stdout = pw
		ct, err := StartWithTracking(cmd, time.Now()) // initial target overridden per sub-test
		if err != nil {
			pw.Close(); pr.Close()
			t.Fatalf("StartWithTracking: %v", err)
		}
		pw.Close()
		t.Cleanup(func() {
			ct.Close()         //nolint:errcheck
			cmd.Process.Kill() //nolint:errcheck
			cmd.Wait()         //nolint:errcheck
			pr.Close()
		})
		return ct, bufio.NewScanner(pr)
	}

	readTimestamps := func(t *testing.T, sc *bufio.Scanner, n int) []time.Time {
		t.Helper()
		out := make([]time.Time, 0, n)
		for sc.Scan() && len(out) < n {
			ts, ok := parseFaketimeTimestamp(sc.Text())
			if ok {
				out = append(out, ts)
			}
		}
		return out
	}

	const tolerance = 5 * time.Second

	t.Run("SetTime", func(t *testing.T) {
		ct, sc := newTrackedHelper(t)
		target := time.Now().Add(24 * time.Hour)
		if err := ct.SetTime(target); err != nil {
			t.Fatalf("SetTime: %v", err)
		}
		got := readTimestamps(t, sc, 2)
		if len(got) < 2 {
			t.Fatalf("got %d timestamps, want 2", len(got))
		}
		for _, ts := range got {
			diff := time.Until(ts)
			if diff < 24*time.Hour-tolerance || diff > 24*time.Hour+tolerance {
				t.Errorf("SetTime: timestamp %v off by %v", ts, diff-24*time.Hour)
			}
		}
	})

	t.Run("Freeze", func(t *testing.T) {
		ct, sc := newTrackedHelper(t)
		frozen := time.Now().Add(48 * time.Hour).Truncate(time.Second)
		if err := ct.Freeze(frozen); err != nil {
			t.Fatalf("Freeze: %v", err)
		}
		// EffectiveTime must report the frozen instant.
		if got := ct.Handle.EffectiveTime(); !got.Equal(frozen) {
			t.Errorf("EffectiveTime() = %v, want %v", got, frozen)
		}
		got := readTimestamps(t, sc, 2)
		if len(got) < 2 {
			t.Fatalf("got %d timestamps, want 2", len(got))
		}
		for _, ts := range got {
			if !ts.Equal(frozen) {
				t.Errorf("Freeze: timestamp %v != frozen %v", ts, frozen)
			}
		}
	})

	t.Run("Advance_advancing", func(t *testing.T) {
		ct, sc := newTrackedHelper(t)
		base := time.Now().Add(24 * time.Hour)
		if err := ct.SetTime(base); err != nil {
			t.Fatalf("SetTime: %v", err)
		}
		if err := ct.Advance(24 * time.Hour); err != nil {
			t.Fatalf("Advance: %v", err)
		}
		got := readTimestamps(t, sc, 2)
		if len(got) < 2 {
			t.Fatalf("got %d timestamps, want 2", len(got))
		}
		for _, ts := range got {
			diff := time.Until(ts)
			if diff < 48*time.Hour-tolerance || diff > 48*time.Hour+tolerance {
				t.Errorf("Advance: timestamp %v off by %v from +48h", ts, diff-48*time.Hour)
			}
		}
	})

	t.Run("Advance_frozen", func(t *testing.T) {
		ct, sc := newTrackedHelper(t)
		frozen := time.Now().Add(24 * time.Hour).Truncate(time.Second)
		if err := ct.Freeze(frozen); err != nil {
			t.Fatalf("Freeze: %v", err)
		}
		if err := ct.Advance(time.Hour); err != nil {
			t.Fatalf("Advance (frozen): %v", err)
		}
		want := frozen.Add(time.Hour)
		if got := ct.Handle.EffectiveTime(); !got.Equal(want) {
			t.Errorf("EffectiveTime() after Advance = %v, want %v", got, want)
		}
		got := readTimestamps(t, sc, 2)
		if len(got) < 2 {
			t.Fatalf("got %d timestamps, want 2", len(got))
		}
		for _, ts := range got {
			if !ts.Equal(want) {
				t.Errorf("Advance(frozen): timestamp %v != %v", ts, want)
			}
		}
	})

	t.Run("Reset", func(t *testing.T) {
		ct, sc := newTrackedHelper(t)
		if err := ct.SetTime(time.Now().Add(24 * time.Hour)); err != nil {
			t.Fatalf("SetTime: %v", err)
		}
		if err := ct.Reset(); err != nil {
			t.Fatalf("Reset: %v", err)
		}
		got := readTimestamps(t, sc, 3)
		realNow := time.Now()
		for _, ts := range got {
			if ts.After(realNow.Add(-time.Hour)) && ts.Before(realNow.Add(time.Hour)) {
				return // found at least one real-time timestamp
			}
		}
		t.Errorf("Reset: no real-time timestamp found in %v", got)
	})
}

// TestWithChildTracker verifies the helper starts tracking, calls fn, and
// cleans up without deadlocking.
func TestWithChildTracker(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}

	target := time.Now().Add(24 * time.Hour)
	called := false

	cmd := exec.Command(exe, "-test.run=TestFaketimeHelper", "-test.v")
	cmd.Env = append(os.Environ(), helperEnv+"=1")

	WithChildTracker(t, cmd, target, func(t *testing.T, ct *ChildTracker) {
		called = true
		if ct.Handle.PID() != cmd.Process.Pid {
			t.Errorf("PID() = %d, want %d", ct.Handle.PID(), cmd.Process.Pid)
		}
		if !ct.Handle.IsAlive() {
			t.Error("IsAlive() = false immediately after start")
		}
		diff := time.Until(ct.Handle.EffectiveTime())
		const tolerance = 5 * time.Second
		if diff < 24*time.Hour-tolerance || diff > 24*time.Hour+tolerance {
			t.Errorf("EffectiveTime() offset = %v, want ~24h", diff)
		}
	})

	if !called {
		t.Error("WithChildTracker: fn was never called")
	}
}

// parseFaketimeTimestamp trims and parses an RFC3339Nano line, returning
// (zero, false) for test-framework noise or unparseable lines.
func parseFaketimeTimestamp(line string) (time.Time, bool) {
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
