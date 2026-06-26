//go:build linux

package inject

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/bkaznowski/epochd/pkg/procmem"
	"github.com/bkaznowski/epochd/pkg/trampoline"
	"github.com/bkaznowski/epochd/pkg/vdso"
)

const helperEnv = "EPOCHD_INJECT_HELPER"

// ---------------------------------------------------------------------------
// Helper processes
// ---------------------------------------------------------------------------

// TestInjectHelperBlock blocks indefinitely. Used as the ptrace target for
// tests that only need a stable process to attach to.
func TestInjectHelperBlock(t *testing.T) {
	if os.Getenv(helperEnv) != "1" {
		t.Skip()
	}
	select {}
}

// TestInjectHelperClock prints time.Now() to stdout once every 100 ms.
// Used by TestInjectObserved to verify the trampoline actually intercepts calls.
func TestInjectHelperClock(t *testing.T) {
	if os.Getenv(helperEnv) != "2" {
		t.Skip()
	}
	for {
		fmt.Println(time.Now().Format(time.RFC3339Nano))
		time.Sleep(100 * time.Millisecond)
	}
}

// ---------------------------------------------------------------------------
// Shared setup
// ---------------------------------------------------------------------------

// startPtraceChild spawns the test binary as a ptrace tracee using the given
// helper env value, waits for the initial SIGTRAP, and returns the Tracer and
// pid. The caller is responsible for Detach and Kill via t.Cleanup.
func startPtraceChild(t *testing.T, helperVal string, extraArgs ...string) (*procmem.Tracer, int) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	args := append([]string{"-test.run=TestInjectHelper", "-test.v"}, extraArgs...)
	cmd := exec.Command(exe, args...)
	cmd.Env = append(os.Environ(), helperEnv+"="+helperVal)
	cmd.SysProcAttr = &syscall.SysProcAttr{Ptrace: true}
	if err := cmd.Start(); err != nil {
		t.Fatalf("cmd.Start: %v", err)
	}
	t.Cleanup(func() { cmd.Process.Kill(); cmd.Wait() })

	tr := procmem.NewTracer()
	if err := tr.FollowChild(cmd.Process.Pid); err != nil {
		t.Fatalf("FollowChild: %v", err)
	}
	t.Cleanup(func() { tr.Detach() })

	return tr, cmd.Process.Pid
}

// ---------------------------------------------------------------------------
// Phase 3 regression — remoteMmap
// ---------------------------------------------------------------------------

func TestRemoteMmap(t *testing.T) {
	tr, pid := startPtraceChild(t, "1")

	info, err := vdso.Locate(pid)
	if err != nil {
		t.Fatalf("vdso.Locate: %v", err)
	}
	t.Logf("vDSO [0x%x, 0x%x), clock_gettime at 0x%x", info.Start, info.End, info.ClockGettimeAddr)

	beforePatch := make([]byte, 8)
	if _, err := procmem.ReadMem(pid, info.ClockGettimeAddr, beforePatch); err != nil {
		t.Fatalf("ReadMem (before): %v", err)
	}

	newPage, err := remoteMmap(tr, pid, info.ClockGettimeAddr, 0 /* kernel chooses */)
	if err != nil {
		t.Fatalf("remoteMmap: %v", err)
	}
	t.Logf("new rwx page: 0x%x", newPage)

	mapStart, mapEnd, mapPerms, err := findMapContaining(pid, newPage)
	if err != nil {
		t.Fatalf("finding new page: %v", err)
	}
	if mapStart != newPage {
		t.Errorf("map start 0x%x != page address 0x%x", mapStart, newPage)
	}
	if mapEnd-mapStart != 4096 {
		t.Errorf("map size %d bytes, want 4096", mapEnd-mapStart)
	}
	if !strings.Contains(mapPerms, "r") || !strings.Contains(mapPerms, "w") || !strings.Contains(mapPerms, "x") {
		t.Errorf("new page perms %q: want rwx", mapPerms)
	}

	pageContent := make([]byte, 4096)
	if _, err := procmem.ReadMem(pid, newPage, pageContent); err != nil {
		t.Fatalf("ReadMem (page content): %v", err)
	}
	if !bytes.Equal(pageContent, make([]byte, 4096)) {
		t.Errorf("new page not zeroed (first non-zero byte at offset %d)", firstNonZero(pageContent))
	}

	afterPatch := make([]byte, 8)
	if _, err := procmem.ReadMem(pid, info.ClockGettimeAddr, afterPatch); err != nil {
		t.Fatalf("ReadMem (after): %v", err)
	}
	if !bytes.Equal(afterPatch, beforePatch) {
		t.Errorf("vDSO bytes not restored:\n  before: % x\n  after:  % x", beforePatch, afterPatch)
	}
}

// ---------------------------------------------------------------------------
// Phase 5 — injection mechanics
// ---------------------------------------------------------------------------

// TestInjectMechanics verifies the structural result of injectWithTracer:
//  1. The trampoline payload is written to a new rwx page.
//  2. The state struct at Handle.StateAddr has the expected initial field values.
//  3. The vDSO clock_gettime entry is patched with a valid JMP rel32.
//  4. writeState updates the state correctly via process_vm_writev.
//  5. The child is still alive after Detach.
func TestInjectMechanics(t *testing.T) {
	tr, pid := startPtraceChild(t, "1")

	info, err := vdso.Locate(pid)
	if err != nil {
		t.Fatalf("vdso.Locate: %v", err)
	}
	t.Logf("clock_gettime at 0x%x", info.ClockGettimeAddr)

	// Choose a fake time 24 h in the future.
	target := time.Now().Add(24 * time.Hour)
	wantSec, wantNsec := diffSecNsec(target, time.Now())

	h, err := injectWithTracer(tr, pid, info.ClockGettimeAddr, wantSec, wantNsec, trampoline.MaskEnabled)
	if err != nil {
		t.Fatalf("injectWithTracer: %v", err)
	}
	t.Logf("Handle: PID=%d StateAddr=0x%x", h.PID, h.StateAddr)

	// 1 — state struct: read it back and check each field.
	stateBytes := make([]byte, trampoline.StateSize)
	if _, err := procmem.ReadMem(pid, h.StateAddr, stateBytes); err != nil {
		t.Fatalf("ReadMem (state): %v", err)
	}
	gotSec, gotNsec, gotMask, gotGen, err := trampoline.DecodeState(stateBytes)
	if err != nil {
		t.Fatalf("DecodeState: %v", err)
	}
	if gotSec != wantSec {
		t.Errorf("state.offsetSec = %d, want %d", gotSec, wantSec)
	}
	if gotNsec != wantNsec {
		t.Errorf("state.offsetNsec = %d, want %d", gotNsec, wantNsec)
	}
	if gotMask != 1 {
		t.Errorf("state.enabledMask = %d, want 1", gotMask)
	}
	if gotGen != 0 {
		t.Errorf("state.generation = %d, want 0 (initial)", gotGen)
	}

	// 2 — JMP patch: first byte must be 0xE9 and displacement must target newPage.
	var jmpBytes [5]byte
	if _, err := procmem.ReadMem(pid, info.ClockGettimeAddr, jmpBytes[:]); err != nil {
		t.Fatalf("ReadMem (JMP): %v", err)
	}
	if jmpBytes[0] != 0xE9 {
		t.Errorf("vDSO byte[0] = 0x%02x, want 0xE9 (JMP rel32)", jmpBytes[0])
	}
	newPage := h.StateAddr - uintptr(trampoline.StateOffset)
	wantDisp := int32(int64(newPage) - int64(info.ClockGettimeAddr+5))
	gotDisp := int32(binary.LittleEndian.Uint32(jmpBytes[1:]))
	if gotDisp != wantDisp {
		t.Errorf("JMP displacement = %d (0x%08x), want %d (0x%08x)",
			gotDisp, uint32(gotDisp), wantDisp, uint32(wantDisp))
	}
	t.Logf("JMP rel32: page=0x%x disp=%d", newPage, gotDisp)

	// 3 — writeState: write new values and read them back.
	newTarget := target.Add(time.Hour)
	newSec, newNsec := diffSecNsec(newTarget, time.Now())
	if err := h.writeState(newSec, newNsec, trampoline.MaskEnabled); err != nil {
		t.Fatalf("writeState: %v", err)
	}
	if _, err := procmem.ReadMem(pid, h.StateAddr, stateBytes); err != nil {
		t.Fatalf("ReadMem (state after writeState): %v", err)
	}
	gotSec2, gotNsec2, _, gotGen2, _ := trampoline.DecodeState(stateBytes)
	if gotSec2 != newSec {
		t.Errorf("after writeState: offsetSec = %d, want %d", gotSec2, newSec)
	}
	if gotNsec2 != newNsec {
		t.Errorf("after writeState: offsetNsec = %d, want %d", gotNsec2, newNsec)
	}
	if gotGen2 != 1 {
		t.Errorf("after writeState: generation = %d, want 1", gotGen2)
	}

	// 4 — Detach and confirm child is still alive.
	tr.Detach()
	if err := syscall.Kill(pid, 0); err != nil {
		t.Errorf("child not alive after Detach: %v", err)
	}
}

// TestInjectObserved spawns a clock-printing process, injects a +24 h offset,
// and verifies that the printed timestamps are approximately 24 h in the future.
// This is the end-to-end functional test for the trampoline.
//
// Requires EPOCHD_INJECT_E2E=1 because some CI environments (e.g. GitHub Actions)
// have Go runtime internals that consume the ptrace exec-stop before FollowChild
// can collect it, causing an ESRCH. TestRemoteMmap and TestInjectMechanics cover
// the individual ptrace primitives and run reliably in all environments.
func TestInjectObserved(t *testing.T) {
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

	offset := 24 * time.Hour
	target := time.Now().Add(offset)
	wantSec, wantNsec := diffSecNsec(target, time.Now())
	if _, err := injectWithTracer(tr, pid, info.ClockGettimeAddr, wantSec, wantNsec, trampoline.MaskEnabled); err != nil {
		t.Fatalf("injectWithTracer: %v", err)
	}
	tr.Detach()

	// Read timestamped lines from the child's stdout and verify they're ~24 h ahead.
	sc := bufio.NewScanner(pr)
	const (
		wantLines = 5
		tolerance = 5 * time.Second
	)
	checked := 0
	for sc.Scan() && checked < wantLines {
		line := strings.TrimSpace(sc.Text())
		// Filter out test framework output lines.
		if strings.HasPrefix(line, "=== ") || strings.HasPrefix(line, "--- ") || line == "" {
			continue
		}
		ts, err := time.Parse(time.RFC3339Nano, line)
		if err != nil {
			continue // skip unparseable lines (e.g. "PASS")
		}
		diff := time.Until(ts)
		if diff < offset-tolerance || diff > offset+tolerance {
			t.Errorf("printed time %v is %.1f h from now, want ~%.1f h",
				ts, diff.Hours(), offset.Hours())
		}
		t.Logf("observed: %v  (offset %.3f h)", ts, diff.Hours())
		checked++
	}
	if checked == 0 {
		t.Error("no parseable timestamp lines received from injected process")
	}
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func findMapContaining(pid int, addr uintptr) (start, end uintptr, perms string, err error) {
	f, err := os.Open(fmt.Sprintf("/proc/%d/maps", pid))
	if err != nil {
		return 0, 0, "", err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 5 {
			continue
		}
		dash := strings.IndexByte(fields[0], '-')
		if dash < 0 {
			continue
		}
		s, e := parseHex(fields[0][:dash]), parseHex(fields[0][dash+1:])
		if addr >= s && addr < e {
			return s, e, fields[1], nil
		}
	}
	return 0, 0, "", fmt.Errorf("no mapping contains 0x%x in /proc/%d/maps", addr, pid)
}

func parseHex(s string) uintptr {
	var v uint64
	fmt.Sscanf(s, "%x", &v)
	return uintptr(v)
}

func firstNonZero(b []byte) int {
	for i, v := range b {
		if v != 0 {
			return i
		}
	}
	return -1
}
