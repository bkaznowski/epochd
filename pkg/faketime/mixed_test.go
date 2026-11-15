//go:build linux

package faketime

import (
	"bufio"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/bkaznowski/epochd/pkg/preload"
)

// The tests in this file put processes of both modes in one Session: `sh` loops
// run under LD_PRELOAD, and the Go clock-printing helper (which LD_PRELOAD
// cannot affect) runs under the ptrace/vDSO backend.

// recipeFilter is the ModeFilter from the WithModeFilter documentation: preload
// where it works, vDSO where it does not.
func recipeFilter(path string) Mode {
	if preload.CheckInterposable(path) != nil {
		return ModeVDSO
	}
	return ModePreload
}

// newGoHelper builds (without starting) the Go clock-printing child and a
// scanner over its stdout.
func newGoHelper(t *testing.T) (*exec.Cmd, *bufio.Scanner) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	cmd := exec.Command(exe, "-test.run=TestFaketimeHelper", "-test.v")
	cmd.Env = append(os.Environ(), helperEnv+"=1")
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	return cmd, bufio.NewScanner(out)
}

// awaitTimestamp reads the helper's output until a timestamp within tol of want
// shows up, skipping lines printed before a clock change reached the process.
func awaitTimestamp(t *testing.T, sc *bufio.Scanner, want time.Time, tol time.Duration) time.Time {
	t.Helper()
	for i := 0; i < 80 && sc.Scan(); i++ {
		ts, ok := parseFaketimeTimestamp(sc.Text())
		if !ok {
			continue
		}
		if d := ts.Sub(want); d >= -tol && d <= tol {
			return ts
		}
	}
	t.Fatalf("no timestamp within %v of %v arrived from the helper", tol, want)
	return time.Time{}
}

// handleKinds reports how many of the session's handles use each backend.
func handleKinds(s *Session) (preloaded, vdso int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, h := range s.handles {
		if h.pre != nil {
			preloaded++
		} else {
			vdso++
		}
	}
	return
}

func killAndWait(cmd *exec.Cmd) {
	cmd.Process.Kill() //nolint:errcheck
	cmd.Wait()         //nolint:errcheck
}

// One clock, two mechanisms: SetTime, Freeze and Advance reach the preloaded
// shell and the vDSO-patched Go process alike.
func TestSessionMixedModes(t *testing.T) {
	needTool(t, "sh", "date")
	target := time.Now().Add(24 * time.Hour)
	s := NewSession(target, WithModeFilter(recipeFilter))

	sh := newLoopProc(t)
	if err := s.Start(sh.cmd); err != nil {
		t.Fatalf("Start sh: %v", err)
	}
	goCmd, goOut := newGoHelper(t)
	if err := s.Start(goCmd); err != nil {
		t.Fatalf("Start go helper: %v", err)
	}
	t.Cleanup(func() {
		s.Reset() //nolint:errcheck
		sh.in.Close()
		sh.cmd.Wait() //nolint:errcheck
		killAndWait(goCmd)
		s.Close() //nolint:errcheck
	})

	if p, v := handleKinds(s); p != 1 || v != 1 {
		t.Fatalf("handle backends: %d preload, %d vdso; want 1 and 1", p, v)
	}
	if s.Len() != 2 || len(s.PIDs()) != 2 {
		t.Fatalf("Len=%d PIDs=%v, want 2 processes", s.Len(), s.PIDs())
	}

	const tol = 5 * time.Second
	if got := sh.epoch(t); !near(got, target.Unix()) {
		t.Errorf("preload process saw %d, want ~%d", got, target.Unix())
	}
	awaitTimestamp(t, goOut, target, tol)

	later := time.Now().Add(9 * 24 * time.Hour)
	if err := s.SetTime(later); err != nil {
		t.Fatalf("SetTime: %v", err)
	}
	if got := sh.epoch(t); !near(got, later.Unix()) {
		t.Errorf("after SetTime the preload process saw %d, want ~%d", got, later.Unix())
	}
	awaitTimestamp(t, goOut, later, tol)

	pinned := time.Date(2038, 8, 9, 10, 11, 12, 0, time.UTC)
	if err := s.Freeze(pinned); err != nil {
		t.Fatalf("Freeze: %v", err)
	}
	if got := sh.epoch(t); got != pinned.Unix() {
		t.Errorf("after Freeze the preload process saw %d, want exactly %d", got, pinned.Unix())
	}
	if ts := awaitTimestamp(t, goOut, pinned, tol); !ts.Equal(pinned) {
		t.Errorf("after Freeze the vdso process saw %v, want exactly %v", ts, pinned)
	}

	if err := s.Advance(3 * time.Hour); err != nil {
		t.Fatalf("Advance: %v", err)
	}
	want := pinned.Add(3 * time.Hour)
	if got := sh.epoch(t); got != want.Unix() {
		t.Errorf("after Advance the preload process saw %d, want exactly %d", got, want.Unix())
	}
	if ts := awaitTimestamp(t, goOut, want, tol); !ts.Equal(want) {
		t.Errorf("after Advance the vdso process saw %v, want exactly %v", ts, want)
	}
}

// StartMode decides one call, whatever the session default says.
func TestSessionStartModeChoosesPerCall(t *testing.T) {
	needTool(t, "date")
	s := NewSession(time.Now().Add(time.Hour)) // default ModeVDSO

	date := exec.Command("date")
	date.Stdout = nil
	if err := s.StartMode(date, ModePreload); err != nil {
		t.Fatalf("StartMode preload: %v", err)
	}
	goCmd, _ := newGoHelper(t)
	if err := s.StartMode(goCmd, ModeVDSO); err != nil {
		t.Fatalf("StartMode vdso: %v", err)
	}
	t.Cleanup(func() {
		s.WaitPID(date.Process.Pid) //nolint:errcheck
		killAndWait(goCmd)
		s.Close() //nolint:errcheck
	})
	if p, v := handleKinds(s); p != 1 || v != 1 {
		t.Errorf("handle backends: %d preload, %d vdso; want 1 and 1", p, v)
	}
}

// Precedence is StartMode > ModeFilter > WithMode: here the session default
// would refuse the Go helper, the filter overrides the default, and StartMode
// overrides the filter.
func TestSessionModePrecedence(t *testing.T) {
	needTool(t, "date")
	s := NewSession(time.Now(), WithMode(ModePreload), WithModeFilter(func(string) Mode { return ModeVDSO }))

	goCmd, _ := newGoHelper(t)
	if err := s.Start(goCmd); err != nil {
		t.Fatalf("filter should have beaten the ModePreload default: %v", err)
	}
	t.Cleanup(func() { killAndWait(goCmd); s.Close() }) //nolint:errcheck

	date := exec.Command("date")
	if err := s.StartMode(date, ModePreload); err != nil {
		t.Fatalf("StartMode should have beaten the filter: %v", err)
	}
	s.WaitPID(date.Process.Pid) //nolint:errcheck
	if p, v := handleKinds(s); p != 1 || v != 1 {
		t.Errorf("handle backends: %d preload, %d vdso; want 1 and 1", p, v)
	}
}

func TestSessionRejectsUnknownMode(t *testing.T) {
	s := NewSession(time.Now())

	cmd := exec.Command("true")
	err := s.StartMode(cmd, Mode(7))
	if err == nil || !strings.Contains(err.Error(), "Mode(7)") {
		t.Errorf("StartMode(Mode(7)) = %v, want an error naming the bad mode", err)
	}
	if cmd.Process != nil {
		t.Error("a process was started despite the invalid mode")
	}

	bad := NewSession(time.Now(), WithModeFilter(func(string) Mode { return Mode(-1) }))
	if err := bad.Start(exec.Command("true")); err == nil {
		t.Error("a filter returning an undefined Mode should fail Start")
	}
	if err := bad.Attach(os.Getpid()); err == nil {
		t.Error("a filter returning an undefined Mode should fail Attach")
	}
	if s.Len() != 0 || bad.Len() != 0 {
		t.Error("nothing should have been added to either session")
	}
}

func TestModeString(t *testing.T) {
	for m, want := range map[Mode]string{ModeVDSO: "vdso", ModePreload: "preload", Mode(9): "Mode(9)"} {
		if got := m.String(); got != want {
			t.Errorf("Mode(%d).String() = %q, want %q", int(m), got, want)
		}
	}
}

// In a mixed session a filter can send attach targets to vDSO (the only mode
// that can reach a running process) and still refuse the ones it routes to
// preload, with an error that points at the fix.
func TestSessionMixedAttach(t *testing.T) {
	s := NewSession(time.Now(), WithModeFilter(func(string) Mode { return ModePreload }))
	err := s.Attach(os.Getpid())
	if err == nil || !strings.Contains(err.Error(), "WithModeFilter") {
		t.Errorf("Attach under a preload filter = %v, want an error pointing at WithModeFilter", err)
	}

	if !ptraceScopeAllows(t) {
		t.Skip("ptrace_scope > 1: PTRACE_ATTACH not permitted")
	}
	s = NewSession(time.Now().Add(24*time.Hour), WithModeFilter(func(string) Mode { return ModeVDSO }))
	cmd, pr := startHelper(t)
	defer pr.Close()
	if err := s.Attach(cmd.Process.Pid); err != nil {
		t.Fatalf("Attach with a vdso filter: %v", err)
	}
	if p, v := handleKinds(s); p != 0 || v != 1 {
		t.Errorf("handle backends: %d preload, %d vdso; want 0 and 1", p, v)
	}
	sc := bufio.NewScanner(pr)
	awaitTimestamp(t, sc, time.Now().Add(24*time.Hour), 5*time.Second)
}

// Close has two jobs in a mixed session, stopping trackers for the vDSO
// processes and releasing files for the preload ones, and must do both.
func TestSessionMixedCloseReleasesBoth(t *testing.T) {
	needTool(t, "sh", "date")
	s := NewSession(time.Now().Add(time.Hour), WithModeFilter(recipeFilter), WithTracking())

	sh := newLoopProc(t)
	if err := s.Start(sh.cmd); err != nil {
		t.Fatalf("Start sh: %v", err)
	}
	goCmd, _ := newGoHelper(t)
	if err := s.Start(goCmd); err != nil {
		t.Fatalf("Start go helper: %v", err)
	}
	t.Cleanup(func() {
		sh.in.Close()
		sh.cmd.Wait() //nolint:errcheck
		killAndWait(goCmd)
	})

	if len(s.trackers) != 1 {
		t.Fatalf("trackers = %d, want exactly 1: only the vDSO process is tracked", len(s.trackers))
	}
	var preHandle *Handle
	for _, h := range s.handles {
		if h.pre != nil {
			preHandle = h
		}
	}
	if preHandle == nil {
		t.Fatal("no preload handle in the session")
	}

	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := preHandle.SetTime(time.Now()); err == nil {
		t.Error("the preload handle should be released by Close, but SetTime still worked")
	}
}
