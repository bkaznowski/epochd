//go:build linux

package procmem

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestHelperBlock is the tracee target for TestTracerBasic.
// It blocks until killed so the parent has time to read/write its memory.
func TestHelperBlock(t *testing.T) {
	if os.Getenv("EPOCHD_TRACER_HELPER") != "1" {
		t.Skip()
	}
	select {}
}

// TestTracerBasic exercises the full lifecycle using SysProcAttr{Ptrace: true}
// (PTRACE_TRACEME), which avoids the PTRACE_ATTACH permission restrictions
// imposed by Yama ptrace_scope in Docker/CI environments.
//
// Flow: spawn child with Ptrace:true → child stops on SIGTRAP after exec →
// FollowChild collects the stop → ReadMem, WriteMem, PokeText → Detach →
// verify child resumes.
func TestTracerBasic(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}

	// Start the helper as a ptrace tracee. The child calls PTRACE_TRACEME
	// before exec and stops on SIGTRAP; no PTRACE_ATTACH from our side.
	cmd := exec.Command(exe, "-test.run=TestHelperBlock", "-test.v")
	cmd.Env = append(os.Environ(), "EPOCHD_TRACER_HELPER=1")
	cmd.SysProcAttr = &syscall.SysProcAttr{Ptrace: true}
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting helper: %v", err)
	}
	t.Cleanup(func() {
		cmd.Process.Kill()
		cmd.Wait()
	})

	pid := cmd.Process.Pid
	tr := NewTracer()

	// Collect the initial SIGTRAP — no PTRACE_ATTACH required.
	if err := tr.FollowChild(pid); err != nil {
		t.Fatalf("FollowChild: %v", err)
	}
	detached := false
	t.Cleanup(func() {
		if !detached {
			tr.Detach()
		}
	})

	// Wait a moment for the child's maps to stabilise.
	time.Sleep(50 * time.Millisecond)

	// --- locate useful pages ---
	vdsoStart, err := findMapEntry(pid, "[vdso]")
	if err != nil {
		t.Fatalf("finding vDSO: %v", err)
	}
	stackStart, err := findMapEntry(pid, "[stack]")
	if err != nil {
		t.Fatalf("finding stack: %v", err)
	}
	t.Logf("vDSO start:  0x%x", vdsoStart)
	t.Logf("stack start: 0x%x", stackStart)

	// --- ReadMem: ELF magic at vDSO base ---
	var got [4]byte
	if _, err := ReadMem(pid, vdsoStart, got[:]); err != nil {
		t.Fatalf("ReadMem: %v", err)
	}
	want := [4]byte{0x7f, 'E', 'L', 'F'}
	if got != want {
		t.Errorf("ReadMem at vDSO: got % x, want ELF magic % x", got, want)
	}

	// --- WriteMem: round-trip on the stack (always rw-p, always present) ---
	pattern := []byte("epochd_procmem_write_test_1234")
	orig := make([]byte, len(pattern))
	if _, err := ReadMem(pid, stackStart, orig); err != nil {
		t.Fatalf("ReadMem (save original): %v", err)
	}
	if _, err := WriteMem(pid, stackStart, pattern); err != nil {
		t.Fatalf("WriteMem: %v", err)
	}
	readback := make([]byte, len(pattern))
	if _, err := ReadMem(pid, stackStart, readback); err != nil {
		t.Fatalf("ReadMem (verify): %v", err)
	}
	if !bytes.Equal(readback, pattern) {
		t.Errorf("WriteMem/ReadMem: got %q, want %q", readback, pattern)
	}
	WriteMem(pid, stackStart, orig) // restore

	// --- WriteMem on the r-xp vDSO should fail (soft check) ---
	if _, err := WriteMem(pid, vdsoStart, []byte{0x00}); err == nil {
		t.Log("note: WriteMem succeeded on r-xp vDSO (kernel allows it here — PokeText fallback not needed on this host)")
	}

	// --- PokeText: write to r-xp vDSO, read back, restore ---
	// +4 skips the ELF magic so we're not touching the ELF header identity.
	const pokeOff = uintptr(4)
	origByte := make([]byte, 1)
	if _, err := ReadMem(pid, vdsoStart+pokeOff, origByte); err != nil {
		t.Fatalf("ReadMem (save poke target): %v", err)
	}
	poke := []byte{^origByte[0]} // flip all bits — guaranteed different
	if err := tr.PokeText(vdsoStart+pokeOff, poke); err != nil {
		t.Fatalf("PokeText: %v", err)
	}
	check := make([]byte, 1)
	if _, err := ReadMem(pid, vdsoStart+pokeOff, check); err != nil {
		t.Fatalf("ReadMem (verify poke): %v", err)
	}
	if check[0] != poke[0] {
		t.Errorf("PokeText: read back 0x%02x, want 0x%02x", check[0], poke[0])
	}
	tr.PokeText(vdsoStart+pokeOff, origByte) // restore

	// --- GetRegs / SetRegs round-trip ---
	regs, err := tr.GetRegs()
	if err != nil {
		t.Fatalf("GetRegs: %v", err)
	}
	if err := tr.SetRegs(regs); err != nil {
		t.Fatalf("SetRegs (no-op round-trip): %v", err)
	}

	// --- Detach ---
	if err := tr.Detach(); err != nil {
		t.Fatalf("Detach: %v", err)
	}
	detached = true

	// Child must still be alive after detach.
	if err := cmd.Process.Signal(syscall.Signal(0)); err != nil {
		t.Errorf("child not running after Detach: %v", err)
	}
}

// findMapEntry returns the start address of the first /proc/<pid>/maps line
// whose pathname field equals name.
func findMapEntry(pid int, name string) (uintptr, error) {
	f, err := os.Open(fmt.Sprintf("/proc/%d/maps", pid))
	if err != nil {
		return 0, err
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 6 || fields[5] != name {
			continue
		}
		var start uint64
		if _, err := fmt.Sscanf(fields[0], "%x-", &start); err != nil {
			return 0, err
		}
		return uintptr(start), nil
	}
	return 0, fmt.Errorf("%s not found in /proc/%d/maps", name, pid)
}
