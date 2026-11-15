//go:build linux

package procmem

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
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

	// StartAndFollowChild forks on the Tracer's own pinned thread and collects the
	// initial SIGTRAP there: no PTRACE_ATTACH required, and the tracer thread is
	// guaranteed to be the one the kernel recorded as the tracee's tracer. Starting
	// the child with cmd.Start() on whatever thread the test goroutine happens to
	// be on works only when the scheduler happens to line the two threads up.
	tr := NewTracer()
	pid, err := tr.StartAndFollowChild(cmd)
	if err != nil {
		t.Fatalf("StartAndFollowChild: %v", err)
	}
	t.Cleanup(func() {
		cmd.Process.Kill()
		cmd.Wait()
	})
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

// TestTracerOps exercises Tracer methods that require a stopped ptrace child:
// SingleStep, Cont, Wait, SetTracee, SetOptions, SetOptionsPID,
// WaitAnyNonBlocking, ContPID, InterruptDetach, DetachAll, isNoProcess.
func TestTracerOps(t *testing.T) {
	spawnHelper := func(t *testing.T) (int, *Tracer) {
		t.Helper()
		exe, err := os.Executable()
		if err != nil {
			t.Fatalf("os.Executable: %v", err)
		}
		cmd := exec.Command(exe, "-test.run=TestHelperBlock", "-test.v")
		cmd.Env = append(os.Environ(), "EPOCHD_TRACER_HELPER=1")
		cmd.SysProcAttr = &syscall.SysProcAttr{Ptrace: true}
		tr := NewTracer()
		pid, err := tr.StartAndFollowChild(cmd) // forks on the tracer's own thread
		if err != nil {
			t.Fatalf("StartAndFollowChild: %v", err)
		}
		t.Cleanup(func() { cmd.Process.Kill(); cmd.Wait() }) //nolint:errcheck
		return pid, tr
	}

	t.Run("SingleStep_Wait", func(t *testing.T) {
		pid, tr := spawnHelper(t)
		defer tr.Detach() //nolint:errcheck

		if err := tr.SingleStep(); err != nil {
			t.Fatalf("SingleStep: %v", err)
		}
		ws, err := tr.Wait()
		if err != nil {
			t.Fatalf("Wait after SingleStep: %v", err)
		}
		if !ws.Stopped() {
			t.Errorf("expected stopped status after SingleStep, pid=%d status=0x%08x", pid, uint32(ws))
		}
	})

	t.Run("SetOptions", func(t *testing.T) {
		_, tr := spawnHelper(t)
		defer tr.Detach() //nolint:errcheck

		if err := tr.SetOptions(unix.PTRACE_O_TRACEFORK); err != nil {
			t.Fatalf("SetOptions: %v", err)
		}
	})

	t.Run("SetOptionsPID", func(t *testing.T) {
		pid, tr := spawnHelper(t)
		defer tr.Detach() //nolint:errcheck

		if err := tr.SetOptionsPID(pid, unix.PTRACE_O_TRACEFORK); err != nil {
			t.Fatalf("SetOptionsPID: %v", err)
		}
	})

	t.Run("SetTracee", func(t *testing.T) {
		pid, tr := spawnHelper(t)
		defer tr.Detach() //nolint:errcheck

		// Change tracee and back — purely exercises the mutex-locked field update.
		tr.SetTracee(0)
		tr.SetTracee(pid)
	})

	t.Run("Cont_Wait", func(t *testing.T) {
		pid, tr := spawnHelper(t)
		defer tr.Detach() //nolint:errcheck

		if err := tr.Cont(0); err != nil {
			t.Fatalf("Cont: %v", err)
		}
		// Send SIGSTOP so we can wait for a stop event.
		if err := syscall.Kill(pid, syscall.SIGSTOP); err != nil {
			t.Fatalf("SIGSTOP: %v", err)
		}
		ws, err := tr.Wait()
		if err != nil {
			t.Fatalf("Wait after Cont/SIGSTOP: %v", err)
		}
		if !ws.Stopped() {
			t.Errorf("expected stopped status, got 0x%08x", uint32(ws))
		}
	})

	t.Run("WaitAnyNonBlocking", func(t *testing.T) {
		_, tr := spawnHelper(t)
		defer tr.Detach() //nolint:errcheck

		// Child is stopped at the initial SIGTRAP — there should be a pending event.
		// Non-blocking wait may or may not return the stopped child depending on
		// whether the kernel surfaces it; just assert no error.
		_, _, err := tr.WaitAnyNonBlocking()
		if err != nil {
			t.Fatalf("WaitAnyNonBlocking: %v", err)
		}
	})

	t.Run("ContPID", func(t *testing.T) {
		pid, tr := spawnHelper(t)

		if err := tr.ContPID(pid, 0); err != nil {
			t.Fatalf("ContPID: %v", err)
		}
		// Interrupt and detach so the cleanup doesn't double-detach.
		tr.InterruptDetach() //nolint:errcheck
	})

	t.Run("InterruptDetach", func(t *testing.T) {
		_, tr := spawnHelper(t)

		if err := tr.Cont(0); err != nil {
			t.Fatalf("Cont before InterruptDetach: %v", err)
		}
		if err := tr.InterruptDetach(); err != nil {
			t.Fatalf("InterruptDetach: %v", err)
		}
	})

	t.Run("DetachAll", func(t *testing.T) {
		pid, tr := spawnHelper(t)

		// DetachAll sends PTRACE_INTERRUPT then waits; child must be running first.
		if err := tr.Cont(0); err != nil {
			t.Fatalf("Cont before DetachAll: %v", err)
		}
		if err := tr.DetachAll([]int{pid}); err != nil {
			t.Fatalf("DetachAll: %v", err)
		}
	})

	t.Run("DetachAll_DeadPID_isNoProcess", func(t *testing.T) {
		// A PID we're not the parent of causes PtraceInterrupt→ESRCH (ignored),
		// Wait4→ECHILD (ignored), PtraceDetach→ESRCH (swallowed by isNoProcess).
		// DetachAll must return nil.
		tr := NewTracer()
		if err := tr.DetachAll([]int{999999999}); err != nil {
			t.Fatalf("DetachAll with non-existent PID: %v", err)
		}
	})
}

// TestTracerAttach tests Attach on a running process. In privileged Docker this
// succeeds; on hosts with Yama ptrace_scope=1+ it is skipped.
func TestTracerAttach(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	cmd := exec.Command(exe, "-test.run=TestHelperBlock", "-test.v")
	cmd.Env = append(os.Environ(), "EPOCHD_TRACER_HELPER=1")
	// No Ptrace:true — we want a freely-running process.
	if err := cmd.Start(); err != nil {
		t.Fatalf("cmd.Start: %v", err)
	}
	t.Cleanup(func() { cmd.Process.Kill(); cmd.Wait() }) //nolint:errcheck

	pid := cmd.Process.Pid
	tr := NewTracer()

	if err := tr.Attach(pid); err != nil {
		// Skip if Yama or permissions prevent PTRACE_ATTACH.
		t.Skipf("Attach not permitted (ptrace_scope): %v", err)
	}
	t.Cleanup(func() { tr.Detach() }) //nolint:errcheck

	// After PTRACE_ATTACH the process is stopped — GetRegs should succeed.
	if _, err := tr.GetRegs(); err != nil {
		t.Fatalf("GetRegs after Attach: %v", err)
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

// ---------------------------------------------------------------------------
// Tracer lifecycle and whole-thread-group stops
// ---------------------------------------------------------------------------

// A Tracer pins an OS thread for its whole life, so dropping one without Close
// leaks that thread. Close must give back exactly the thread each tracer held.
// This tracks the tracers' own thread ids rather than the process's thread
// count, which moves with whatever else the Go runtime is doing.
func TestTracerCloseReleasesItsThread(t *testing.T) {
	const n = 40
	tids := make([]int, n)
	tracers := make([]*Tracer, n)
	for i := range tracers {
		tracers[i] = NewTracer()
		tracers[i].run(func() { tids[i] = unix.Gettid() }) // runs on the pinned thread
	}

	seen := map[int]bool{}
	for _, tid := range tids {
		if tid == 0 || seen[tid] {
			t.Fatalf("tracers did not each pin their own OS thread: %v", tids)
		}
		seen[tid] = true
		if _, err := os.Stat(fmt.Sprintf("/proc/self/task/%d", tid)); err != nil {
			t.Fatalf("thread %d should be alive while its tracer is open: %v", tid, err)
		}
	}

	for _, tr := range tracers {
		tr.Close()
		tr.Close() // idempotent
	}
	for _, tid := range tids {
		deadline := time.Now().Add(5 * time.Second)
		for {
			if _, err := os.Stat(fmt.Sprintf("/proc/self/task/%d", tid)); err != nil {
				break
			}
			if time.Now().After(deadline) {
				t.Errorf("thread %d is still alive 5s after its tracer was closed: the pinned thread was not released", tid)
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
}

func TestTracerMethodsFailCleanlyAfterClose(t *testing.T) {
	tr := NewTracer()
	tr.Close()

	if err := tr.Seize(1); !errors.Is(err, ErrTracerClosed) {
		t.Errorf("Seize after Close = %v, want ErrTracerClosed", err)
	}
	if _, err := tr.GetRegs(); !errors.Is(err, ErrTracerClosed) {
		t.Errorf("GetRegs after Close = %v, want ErrTracerClosed", err)
	}
	if err := tr.Detach(); !errors.Is(err, ErrTracerClosed) {
		t.Errorf("Detach after Close = %v, want ErrTracerClosed", err)
	}
	if got := tr.Threads(); got != nil {
		t.Errorf("Threads after Close = %v, want nil", got)
	}
}

// Only EIO (the tracee was not attached with PTRACE_SEIZE) justifies falling
// back to SIGSTOP. ESRCH means the process is gone; signalling that pid could
// stop an unrelated process that has reused it.
func TestSigstopFallbackOnlyForEIO(t *testing.T) {
	for _, c := range []struct {
		err  error
		want bool
	}{
		{unix.EIO, true},
		{fmt.Errorf("wrapped: %w", unix.EIO), true},
		{unix.ESRCH, false},
		{unix.EPERM, false},
		{nil, false},
	} {
		if got := needsSigstopFallback(c.err); got != c.want {
			t.Errorf("needsSigstopFallback(%v) = %v, want %v", c.err, got, c.want)
		}
	}
}

// startBusyChild starts the test binary as an ordinary (non-ptrace) child with
// several OS threads that are all running, like a real service.
func startBusyChild(t *testing.T) int {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(exe, "-test.run=TestHelperBusy", "-test.v")
	cmd.Env = append(os.Environ(), "EPOCHD_TRACER_HELPER=2")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cmd.Process.Kill(); cmd.Wait() }) //nolint:errcheck
	time.Sleep(200 * time.Millisecond)                   // let the runtime start its threads
	return cmd.Process.Pid
}

// TestHelperBusy is a multi-threaded target whose threads are all busy.
func TestHelperBusy(t *testing.T) {
	if os.Getenv("EPOCHD_TRACER_HELPER") != "2" {
		t.Skip()
	}
	for i := 0; i < 6; i++ {
		go func() {
			for {
				_ = time.Now()
			}
		}()
	}
	select {}
}

// threadState returns the one-letter state of a thread from /proc.
func threadState(t *testing.T, pid, tid int) string {
	t.Helper()
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/task/%d/stat", pid, tid))
	if err != nil {
		return "gone"
	}
	rest := string(b)[strings.LastIndexByte(string(b), ')')+2:]
	return rest[:1]
}

func allThreads(t *testing.T, pid int) []int {
	t.Helper()
	tids, err := listThreads(pid)
	if err != nil {
		t.Fatal(err)
	}
	return tids
}

// Attaching to one thread leaves the others running, free to execute code the
// caller is about to rewrite. Seize must stop the whole group, and Detach must
// let it go again.
func TestSeizeStopsEveryThread(t *testing.T) {
	pid := startBusyChild(t)
	if n := len(allThreads(t, pid)); n < 4 {
		t.Skipf("target only has %d threads; need a multi-threaded process", n)
	}

	tr := NewTracer()
	defer tr.Close()
	if err := tr.Seize(pid); err != nil {
		if errors.Is(err, unix.EPERM) {
			t.Skipf("ptrace not permitted here: %v", err)
		}
		t.Fatalf("Seize: %v", err)
	}

	tids := allThreads(t, pid)
	for _, tid := range tids {
		if st := threadState(t, pid, tid); st != "t" {
			t.Errorf("thread %d is %q after Seize, want t (tracing stop)", tid, st)
		}
	}
	if got := tr.Threads(); len(got) != len(tids) || got[0] != pid {
		t.Errorf("Threads() = %v, want the primary %d first and all %d threads", got, pid, len(tids))
	}

	if err := tr.Detach(); err != nil {
		t.Fatalf("Detach: %v", err)
	}
	time.Sleep(100 * time.Millisecond)
	for _, tid := range allThreads(t, pid) {
		if st := threadState(t, pid, tid); st == "t" || st == "T" {
			t.Errorf("thread %d is still stopped (%q) after Detach", tid, st)
		}
	}
}

// notInSyscall reports whether a stopped thread is in user code rather than
// parked inside a system call. Single-stepping a thread that sits on a blocking
// syscall just re-executes the call and blocks, which is not what the callers
// of StepOutOf (code in the vDSO) ever meet.
func notInSyscall(pid, tid int) bool {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/task/%d/syscall", pid, tid))
	return err == nil && strings.HasPrefix(string(b), "-1 ")
}

// StepOutOf moves a thread that is inside a protected range out of it, which is
// what keeps a thread from being resumed in the middle of rewritten bytes.
func TestStepOutOfMovesThreadsOutOfRange(t *testing.T) {
	pid := startBusyChild(t)
	tr := NewTracer()
	defer tr.Close()
	if err := tr.Seize(pid); err != nil {
		if errors.Is(err, unix.EPERM) {
			t.Skipf("ptrace not permitted here: %v", err)
		}
		t.Fatal(err)
	}
	defer tr.Detach() //nolint:errcheck

	tid := 0
	for _, candidate := range tr.Threads() {
		if notInSyscall(pid, candidate) {
			tid = candidate
			break
		}
	}
	if tid == 0 {
		t.Skip("no thread of the target is running user code right now")
	}

	regs, err := tr.GetRegsPID(tid)
	if err != nil {
		t.Fatal(err)
	}
	// A one-byte range at the instruction that thread is about to run.
	r := AddrRange{Start: uintptr(regs.Rip), End: uintptr(regs.Rip) + 1}
	if err := tr.StepOutOf(r); err != nil {
		t.Fatalf("StepOutOf: %v", err)
	}
	after, err := tr.GetRegsPID(tid)
	if err != nil {
		t.Fatal(err)
	}
	if after.Rip == regs.Rip {
		t.Errorf("thread %d is still at 0x%x: it was not stepped out of the range", tid, regs.Rip)
	}
}
