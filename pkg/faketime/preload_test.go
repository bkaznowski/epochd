//go:build linux

package faketime

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/bkaznowski/epochd/pkg/preload"
)

// The preload backend needs no ptrace, so none of these tests are gated on
// ptrace_scope. They run `date`/`sh` because the helper child used by the vDSO
// tests is a Go binary, which LD_PRELOAD cannot affect.

func needTool(t *testing.T, names ...string) {
	t.Helper()
	for _, n := range names {
		if _, err := exec.LookPath(n); err != nil {
			t.Skipf("%s not available: %v", n, err)
		}
	}
}

func near(got, want int64) bool {
	d := got - want
	return d >= -3 && d <= 3
}

func parseEpoch(t *testing.T, s string) int64 {
	t.Helper()
	n, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	if err != nil {
		t.Fatalf("unexpected output %q: %v", s, err)
	}
	return n
}

// loopProc is a long-lived `sh` that prints the date it sees each time it is
// poked, so a test can read the clock of a running process repeatedly.
type loopProc struct {
	cmd *exec.Cmd
	in  io.WriteCloser
	out *bufio.Reader
}

func newLoopProc(t *testing.T) *loopProc {
	t.Helper()
	cmd := exec.Command("sh", "-c", "while read x; do date -u +%s; done")
	in, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	return &loopProc{cmd: cmd, in: in, out: bufio.NewReader(out)}
}

func (p *loopProc) epoch(t *testing.T) int64 {
	t.Helper()
	if _, err := fmt.Fprintln(p.in, "go"); err != nil {
		t.Fatalf("poke: %v", err)
	}
	line, err := p.out.ReadString('\n')
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	return parseEpoch(t, line)
}

func TestStartPreload(t *testing.T) {
	needTool(t, "date")
	target := time.Now().Add(48 * time.Hour)

	cmd := exec.Command("date", "-u", "+%s")
	var out bytes.Buffer
	cmd.Stdout = &out
	h, err := StartPreload(cmd, target)
	if err != nil {
		t.Fatalf("StartPreload: %v", err)
	}
	defer h.Close()

	if h.PID() != cmd.Process.Pid {
		t.Errorf("PID() = %d, want %d", h.PID(), cmd.Process.Pid)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if got := parseEpoch(t, out.String()); !near(got, target.Unix()) {
		t.Errorf("process saw %d, want ~%d", got, target.Unix())
	}
	if h.IsAlive() {
		t.Error("IsAlive() should be false after the process exited")
	}
	if d := h.EffectiveTime().Sub(target); d < 0 || d > 5*time.Second {
		t.Errorf("EffectiveTime() is %v off the target, want a small positive drift", d)
	}
}

func TestStartFrozenPreloadHoldsAcrossChildren(t *testing.T) {
	needTool(t, "sh", "date", "sleep")
	target := time.Date(2034, 6, 7, 8, 9, 10, 0, time.UTC)

	cmd := exec.Command("sh", "-c", "date -u +%s; sleep 1.2; date -u +%s")
	var out bytes.Buffer
	cmd.Stdout = &out
	h, err := StartFrozenPreload(cmd, target)
	if err != nil {
		t.Fatalf("StartFrozenPreload: %v", err)
	}
	defer h.Close()
	if err := cmd.Wait(); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if !h.IsFrozen() {
		t.Error("IsFrozen() = false, want true")
	}
	lines := strings.Fields(out.String())
	if len(lines) != 2 {
		t.Fatalf("want 2 lines, got %q", out.String())
	}
	for i, l := range lines {
		if got := parseEpoch(t, l); got != target.Unix() {
			t.Errorf("reading %d = %d, want exactly %d", i, got, target.Unix())
		}
	}
}

// SetTime, Freeze, Advance and Reset all take effect on a process that is
// already running, through the same Handle methods the vDSO backend has.
func TestPreloadHandleLiveUpdates(t *testing.T) {
	needTool(t, "sh", "date")
	p := newLoopProc(t)
	h, err := StartPreload(p.cmd, time.Now().Add(24*time.Hour))
	if err != nil {
		t.Fatalf("StartPreload: %v", err)
	}
	defer func() {
		p.in.Close()
		p.cmd.Wait() //nolint:errcheck
		h.Close()
	}()

	if got, want := p.epoch(t), time.Now().Add(24*time.Hour).Unix(); !near(got, want) {
		t.Errorf("initial: %d, want ~%d", got, want)
	}

	later := time.Now().Add(10 * 24 * time.Hour)
	if err := h.SetTime(later); err != nil {
		t.Fatal(err)
	}
	if got := p.epoch(t); !near(got, later.Unix()) {
		t.Errorf("after SetTime: %d, want ~%d", got, later.Unix())
	}

	if err := h.Advance(time.Hour); err != nil {
		t.Fatal(err)
	}
	if got, want := p.epoch(t), later.Add(time.Hour).Unix(); !near(got, want) {
		t.Errorf("after Advance(1h): %d, want ~%d", got, want)
	}

	pinned := time.Date(2041, 2, 3, 4, 5, 6, 0, time.UTC)
	if err := h.Freeze(pinned); err != nil {
		t.Fatal(err)
	}
	if got := p.epoch(t); got != pinned.Unix() {
		t.Errorf("after Freeze: %d, want exactly %d", got, pinned.Unix())
	}
	if !h.IsFrozen() {
		t.Error("IsFrozen() = false after Freeze")
	}

	if err := h.Reset(); err != nil {
		t.Fatal(err)
	}
	if got := p.epoch(t); !near(got, time.Now().Unix()) {
		t.Errorf("after Reset: %d, want ~%d (real time)", got, time.Now().Unix())
	}
}

func TestSessionPreloadSharesOneClock(t *testing.T) {
	needTool(t, "sh", "date")
	target := time.Now().Add(72 * time.Hour)
	s := NewSession(target, WithMode(ModePreload))

	procs := []*loopProc{newLoopProc(t), newLoopProc(t)}
	for _, p := range procs {
		if err := s.Start(p.cmd); err != nil {
			t.Fatalf("Start: %v", err)
		}
	}
	if s.Len() != 2 || len(s.PIDs()) != 2 {
		t.Fatalf("Len=%d PIDs=%v, want 2 processes", s.Len(), s.PIDs())
	}

	check := func(label string, want func() int64, exact bool) {
		t.Helper()
		for i, p := range procs {
			got := p.epoch(t)
			ok := near(got, want())
			if exact {
				ok = got == want()
			}
			if !ok {
				t.Errorf("%s: process %d saw %d, want %d", label, i, got, want())
			}
		}
	}
	check("initial", func() int64 { return target.Unix() }, false)

	later := time.Now().Add(9 * 24 * time.Hour)
	if err := s.SetTime(later); err != nil {
		t.Fatal(err)
	}
	check("SetTime", func() int64 { return later.Unix() }, false)

	pinned := time.Date(2037, 7, 8, 9, 10, 11, 0, time.UTC)
	if err := s.Freeze(pinned); err != nil {
		t.Fatal(err)
	}
	check("Freeze", func() int64 { return pinned.Unix() }, true)

	if err := s.Advance(2 * time.Hour); err != nil {
		t.Fatal(err)
	}
	check("Advance", func() int64 { return pinned.Add(2 * time.Hour).Unix() }, true)

	for _, p := range procs {
		p.in.Close()
		ws, err := s.WaitPID(p.cmd.Process.Pid)
		if err != nil || ws.ExitStatus() != 0 {
			t.Errorf("WaitPID(%d) = (%v, %v), want clean exit", p.cmd.Process.Pid, ws, err)
		}
	}
	if err := s.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
}

// Descendants inherit the clock, so WithTracking has nothing to track and must
// not start a tracer (which would also demand ptrace).
func TestSessionPreloadIgnoresTracking(t *testing.T) {
	needTool(t, "date")
	s := NewSession(time.Now().Add(time.Hour), WithMode(ModePreload), WithTracking())

	cmd := exec.Command("date")
	cmd.Stdout = io.Discard
	if err := s.Start(cmd); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if len(s.trackers) != 0 {
		t.Errorf("preload sessions should not create ChildTrackers, got %d", len(s.trackers))
	}
	if _, err := s.WaitPID(cmd.Process.Pid); err != nil {
		t.Errorf("WaitPID: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
}

func TestSessionPreloadRefusesGoBinary(t *testing.T) {
	s := NewSession(time.Now(), WithMode(ModePreload))
	err := s.Start(exec.Command(os.Args[0]))
	if !errors.Is(err, preload.ErrNotInterposable) {
		t.Errorf("Start = %v, want an error wrapping preload.ErrNotInterposable", err)
	}
	if s.Len() != 0 {
		t.Errorf("Len = %d, a refused process must not be added", s.Len())
	}
}

func TestSessionPreloadRejectsAttach(t *testing.T) {
	s := NewSession(time.Now(), WithMode(ModePreload))
	err := s.Attach(os.Getpid())
	if err == nil || !strings.Contains(err.Error(), "ModePreload") {
		t.Errorf("Attach = %v, want an error explaining ModePreload cannot attach", err)
	}
	if s.Len() != 0 {
		t.Errorf("Len = %d, want 0", s.Len())
	}
}

func TestSessionPreloadHonoursFakeTimeFilter(t *testing.T) {
	needTool(t, "date")
	s := NewSession(time.Now().Add(100*24*time.Hour), WithMode(ModePreload),
		WithFakeTimeFilter(func(string) bool { return false }))

	cmd := exec.Command("date", "-u", "+%s")
	var out bytes.Buffer
	cmd.Stdout = &out
	if err := s.Start(cmd); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if _, err := s.WaitPID(cmd.Process.Pid); err != nil {
		t.Fatalf("WaitPID: %v", err)
	}
	if got := parseEpoch(t, out.String()); !near(got, time.Now().Unix()) {
		t.Errorf("filtered-out process saw %d, want real time ~%d", got, time.Now().Unix())
	}
	if s.Len() != 0 {
		t.Errorf("Len = %d, a filtered-out process must not get a handle", s.Len())
	}
}

func TestSessionPreloadPruneReleasesDeadHandles(t *testing.T) {
	needTool(t, "true")
	s := NewSession(time.Now().Add(time.Hour), WithMode(ModePreload))
	cmd := exec.Command("true")
	if err := s.Start(cmd); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if _, err := s.WaitPID(cmd.Process.Pid); err != nil {
		t.Fatalf("WaitPID: %v", err)
	}
	if n := s.Prune(); n != 1 {
		t.Errorf("Prune() = %d, want 1", n)
	}
	if s.Len() != 0 {
		t.Errorf("Len = %d after Prune, want 0", s.Len())
	}
	// Close must still succeed: the pruned handle was already released.
	if err := s.Close(); err != nil {
		t.Errorf("Close after Prune: %v", err)
	}
}

func TestDefaultModeIsVDSO(t *testing.T) {
	if s := NewSession(time.Now()); s.mode != ModeVDSO {
		t.Errorf("default mode = %v, want ModeVDSO so existing callers are unaffected", s.mode)
	}
	if ModeVDSO != 0 {
		t.Error("ModeVDSO must be the zero value")
	}
}

// The leader exiting does not end a preload clock: a descendant it left behind
// is still reading it, may still exec programs that load the library by path,
// and is only reachable through this handle. Prune must leave it alone until
// the last such process is gone.
func TestSessionPreloadPruneKeepsHandleWhileDescendantsRun(t *testing.T) {
	needTool(t, "sh", "sleep")
	s := NewSession(time.Now().Add(time.Hour), WithMode(ModePreload))
	t.Cleanup(func() { s.Close() }) //nolint:errcheck

	// The shell backgrounds a sleep (detached from our pipe so Wait returns) and exits.
	cmd := exec.Command("sh", "-c", "sleep 60 >/dev/null 2>&1 </dev/null & echo $!")
	var out bytes.Buffer
	cmd.Stdout = &out
	if err := s.Start(cmd); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if _, err := s.WaitPID(cmd.Process.Pid); err != nil {
		t.Fatalf("WaitPID: %v", err)
	}
	orphan, err := strconv.Atoi(strings.TrimSpace(out.String()))
	if err != nil {
		t.Fatalf("could not read the orphan's pid from %q: %v", out.String(), err)
	}
	t.Cleanup(func() { syscall.Kill(orphan, syscall.SIGKILL) }) //nolint:errcheck

	// Let the orphan finish loading before asking whether it is in use.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if n, _ := s.handles[0].PreloadAttached(); n >= 2 { // sh and sleep
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	if n := s.Prune(); n != 0 {
		t.Errorf("Prune() = %d while a descendant still runs on the clock, want 0", n)
	}
	if s.Len() != 1 {
		t.Fatalf("Len = %d, the handle must stay while its clock is in use", s.Len())
	}
	// The clock must still be controllable, and its library must still exist.
	if err := s.SetTime(time.Now().Add(2 * time.Hour)); err != nil {
		t.Errorf("SetTime on the kept handle: %v", err)
	}

	syscall.Kill(orphan, syscall.SIGKILL) //nolint:errcheck
	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && syscall.Kill(orphan, 0) == nil {
		time.Sleep(10 * time.Millisecond)
	}
	if n := s.Prune(); n != 1 {
		t.Errorf("Prune() = %d once the descendant exited, want 1", n)
	}
	if s.Len() != 0 {
		t.Errorf("Len = %d after the last user of the clock exited, want 0", s.Len())
	}
}

func TestPreloadAttachedExposedOnHandles(t *testing.T) {
	needTool(t, "date")
	cmd := exec.Command("date")
	h, err := StartPreload(cmd, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	cmd.Wait() //nolint:errcheck
	if n, ok := h.PreloadAttached(); !ok || n != 1 {
		t.Errorf("PreloadAttached() = (%d, %v), want (1, true)", n, ok)
	}
	if _, ok := (&Handle{}).PreloadAttached(); ok {
		t.Error("a handle with no preload backend must report ok=false")
	}
}
