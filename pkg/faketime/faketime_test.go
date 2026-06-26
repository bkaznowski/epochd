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
