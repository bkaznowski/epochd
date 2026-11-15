//go:build linux

package inject

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/bkaznowski/epochd/pkg/procmem"
	"github.com/bkaznowski/epochd/pkg/vdso"
)

// Regression tests for injecting into processes that are running and
// multi-threaded, the way a real service is: attaching must not kill them,
// must not disturb anything already mapped, must be repeatable without leaking,
// and must be undoable.

// TestInjectHelperBusyClock is a multi-threaded target: several goroutines read
// the clock in a tight loop (every time.Now() goes through the vDSO), while the
// main goroutine prints the time it sees every 50 ms.
func TestInjectHelperBusyClock(t *testing.T) {
	if os.Getenv(helperEnv) != "3" {
		t.Skip()
	}
	for i := 0; i < 4; i++ {
		go func() {
			for {
				_ = time.Now()
			}
		}()
	}
	for {
		fmt.Println(time.Now().Format(time.RFC3339Nano))
		time.Sleep(50 * time.Millisecond)
	}
}

// startBusyClockChild starts the busy target as an ordinary child (no
// PTRACE_TRACEME: it is attached to the way an agent attaches to a container).
func startBusyClockChild(t *testing.T) (*exec.Cmd, *bufio.Scanner) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(exe, "-test.run=TestInjectHelperBusyClock", "-test.v")
	cmd.Env = append(os.Environ(), helperEnv+"=3")
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cmd.Process.Kill(); cmd.Wait() }) //nolint:errcheck
	sc := bufio.NewScanner(out)
	if _, ok := scanTimestamp(sc, 100); !ok {
		t.Fatal("busy target never printed a timestamp")
	}
	return cmd, sc
}

// scanTimestamp returns the next parseable timestamp line, reading at most
// maxLines lines.
func scanTimestamp(sc *bufio.Scanner, maxLines int) (time.Time, bool) {
	for i := 0; i < maxLines && sc.Scan(); i++ {
		line := strings.TrimSpace(sc.Text())
		if ts, err := time.Parse(time.RFC3339Nano, line); err == nil {
			return ts, true
		}
	}
	return time.Time{}, false
}

// awaitTimestampNear reads until a printed time is within tol of want. Lines
// printed before an injection or uninstall took effect are skipped.
func awaitTimestampNear(t *testing.T, sc *bufio.Scanner, want time.Time, tol time.Duration) {
	t.Helper()
	for i := 0; i < 200 && sc.Scan(); i++ {
		ts, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(sc.Text()))
		if err != nil {
			continue
		}
		if d := ts.Sub(want); d >= -tol && d <= tol {
			return
		}
	}
	t.Fatalf("the target never printed a time within %v of %v", tol, want)
}

// procState returns the one-letter state of pid, or "gone".
func procState(pid int) string {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return "gone"
	}
	s := string(b)
	return s[strings.LastIndexByte(s, ')')+2:][:1]
}

func requireAlive(t *testing.T, pid int, doing string) {
	t.Helper()
	if st := procState(pid); st == "Z" || st == "gone" || st == "X" {
		t.Fatalf("the target died (state %q) while %s", st, doing)
	}
}

// injectOrSkip injects and skips the test where ptrace is not permitted.
func injectOrSkip(t *testing.T, pid int, target time.Time) *Handle {
	t.Helper()
	h, err := InjectAtTime(pid, target)
	if err != nil {
		if errors.Is(err, unix.EPERM) {
			t.Skipf("ptrace not permitted here: %v", err)
		}
		t.Fatalf("InjectAtTime: %v", err)
	}
	return h
}

// rwxPages counts the 4 KiB rwx mappings in pid: the trampoline pages.
func rwxPages(t *testing.T, pid int) int {
	t.Helper()
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/maps", pid))
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Fields(line)
		if len(f) < 2 || f[1] != "rwxp" {
			continue
		}
		dash := strings.IndexByte(f[0], '-')
		if parseHexAddr(f[0][dash+1:])-parseHexAddr(f[0][:dash]) == 4096 {
			n++
		}
	}
	return n
}

// Attaching to a process whose other threads keep reading the clock used to
// kill it: only one thread was stopped, so the others could run the
// temporary syscall+int3 stub or a half-written JMP. Every attach here must
// leave the target alive and on the fake clock.
func TestInjectIntoBusyMultiThreadedProcess(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns several busy processes")
	}
	const attempts = 10
	for i := 0; i < attempts; i++ {
		cmd, sc := startBusyClockChild(t)
		pid := cmd.Process.Pid

		target := time.Now().Add(24 * time.Hour)
		injectOrSkip(t, pid, target)

		time.Sleep(150 * time.Millisecond)
		requireAlive(t, pid, fmt.Sprintf("being injected (attempt %d)", i))
		awaitTimestampNear(t, sc, target, 10*time.Second)
	}
}

// Injecting the same process again (an agent re-injecting after a restart, or a
// second Inject call) must reuse the trampoline already there rather than map
// another page and re-patch on top of the first.
func TestInjectReusesAnExistingTrampoline(t *testing.T) {
	cmd, sc := startBusyClockChild(t)
	pid := cmd.Process.Pid

	h1 := injectOrSkip(t, pid, time.Now().Add(24*time.Hour))
	if n := rwxPages(t, pid); n != 1 {
		t.Fatalf("%d trampoline pages after the first injection, want 1", n)
	}

	later := time.Now().Add(72 * time.Hour)
	h2, err := InjectAtTime(pid, later)
	if err != nil {
		t.Fatalf("second InjectAtTime: %v", err)
	}
	if h2.StateAddr != h1.StateAddr {
		t.Errorf("second injection mapped a new trampoline (state 0x%x, was 0x%x)", h2.StateAddr, h1.StateAddr)
	}
	if n := rwxPages(t, pid); n != 1 {
		t.Errorf("%d trampoline pages after re-injecting, want 1 (a page leaked)", n)
	}
	awaitTimestampNear(t, sc, later, 10*time.Second)

	// The reused injection still knows how to undo itself, even though it never
	// saw the original bytes.
	if err := h2.Uninstall(); err != nil {
		t.Fatalf("Uninstall: %v", err)
	}
	assertVDSOPristine(t, pid)
	if n := rwxPages(t, pid); n != 0 {
		t.Errorf("%d trampoline pages left after Uninstall, want 0", n)
	}
}

// assertVDSOPristine checks that every hooked vDSO entry point in pid holds
// exactly the bytes this process's own, unpatched vDSO has.
func assertVDSOPristine(t *testing.T, pid int) {
	t.Helper()
	info, err := vdso.Locate(pid)
	if err != nil {
		t.Fatal(err)
	}
	for name, addr := range map[string]uintptr{
		"clock_gettime": info.ClockGettimeAddr,
		"gettimeofday":  info.GettimeofdayAddr,
		"time":          info.TimeAddr,
	} {
		want, err := pristineEntry(info, addr)
		if err != nil {
			t.Fatal(err)
		}
		var got [hookLen]byte
		if _, err := procmem.ReadMem(pid, addr, got[:]); err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Errorf("vDSO %s entry is % x, want the pristine % x", name, got, want)
		}
	}
}

// Uninstall puts the vDSO back and unmaps the page, so the process gets the
// kernel's fast path (and real time) again; the Handle is then unusable.
func TestUninstallRestoresTheVDSO(t *testing.T) {
	cmd, sc := startBusyClockChild(t)
	pid := cmd.Process.Pid

	h := injectOrSkip(t, pid, time.Now().Add(24*time.Hour))

	info, err := vdso.Locate(pid)
	if err != nil {
		t.Fatal(err)
	}
	var entry [hookLen]byte
	if _, err := procmem.ReadMem(pid, info.ClockGettimeAddr, entry[:]); err != nil {
		t.Fatal(err)
	}
	if entry[0] != 0xE9 {
		t.Fatalf("clock_gettime entry is % x after injection, want a JMP (0xE9)", entry)
	}

	if err := h.Uninstall(); err != nil {
		t.Fatalf("Uninstall: %v", err)
	}
	requireAlive(t, pid, "being uninstalled")
	assertVDSOPristine(t, pid)
	if n := rwxPages(t, pid); n != 0 {
		t.Errorf("%d trampoline pages left after Uninstall, want 0", n)
	}
	awaitTimestampNear(t, sc, time.Now(), 5*time.Second) // back on real time

	if err := h.SetTime(time.Now()); !errors.Is(err, ErrUninstalled) {
		t.Errorf("SetTime after Uninstall = %v, want ErrUninstalled", err)
	}
	if err := h.Uninstall(); err != nil {
		t.Errorf("second Uninstall = %v, want nil", err)
	}
}

// A process that has already exited has nothing to uninstall.
func TestUninstallOfAnExitedProcessSucceeds(t *testing.T) {
	cmd, _ := startBusyClockChild(t)
	h := injectOrSkip(t, cmd.Process.Pid, time.Now().Add(time.Hour))

	cmd.Process.Kill() //nolint:errcheck
	cmd.Wait()         //nolint:errcheck
	if err := h.Uninstall(); err != nil {
		t.Errorf("Uninstall of an exited process = %v, want nil", err)
	}
}

// remoteMmap at a fixed address must refuse an address that is already mapped.
// It used to retry with MAP_FIXED on EEXIST, which would have replaced the
// existing mapping.
func TestRemoteMmapNeverReplacesAMapping(t *testing.T) {
	tr, pid := startPtraceChild(t, "1")
	info, err := vdso.Locate(pid)
	if err != nil {
		t.Fatal(err)
	}

	// Pick a writable mapping that is already there and remember what is in it.
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/maps", pid))
	if err != nil {
		t.Fatal(err)
	}
	var addr uintptr
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Fields(line)
		if len(f) >= 2 && strings.HasPrefix(f[1], "rw") && !strings.Contains(line, "[vvar") {
			addr = parseHexAddr(f[0][:strings.IndexByte(f[0], '-')])
			break
		}
	}
	if addr == 0 {
		t.Skip("no writable mapping found")
	}
	before := make([]byte, 256)
	if _, err := procmem.ReadMem(pid, addr, before); err != nil {
		t.Fatal(err)
	}

	_, err = remoteMmap(tr, pid, info.ClockGettimeAddr, addr)
	if !errors.Is(err, errAddrInUse) {
		t.Fatalf("remoteMmap onto a mapped address = %v, want errAddrInUse", err)
	}

	after := make([]byte, 256)
	if _, err := procmem.ReadMem(pid, addr, after); err != nil {
		t.Fatalf("the mapping at 0x%x is gone or unreadable after the refused mmap: %v", addr, err)
	}
	if string(before) != string(after) {
		t.Error("the existing mapping's contents changed")
	}
	if _, err := tr.GetRegs(); err != nil {
		t.Errorf("the tracee was left in a bad state: %v", err)
	}
}

// When the first free page it finds is taken, mapTrampolinePage must move on to
// the next rather than fail or clobber it.
func TestMapTrampolinePageSkipsATakenPage(t *testing.T) {
	tr, pid := startPtraceChild(t, "1")
	info, err := vdso.Locate(pid)
	if err != nil {
		t.Fatal(err)
	}
	near := info.ClockGettimeAddr

	first, err := findNearbyGap(pid, near)
	if err != nil {
		t.Fatal(err)
	}
	// Take it, so a search that still believes it is free would collide.
	if _, err := remoteMmap(tr, pid, near, first); err != nil {
		t.Fatalf("occupying 0x%x: %v", first, err)
	}

	page, err := mapTrampolinePage(tr, pid, near)
	if err != nil {
		t.Fatalf("mapTrampolinePage: %v", err)
	}
	if page == first {
		t.Fatalf("mapTrampolinePage returned the page that was already taken (0x%x)", page)
	}
	if d := int64(page) - int64(near); d > 1<<31 || d < -(1<<31) {
		t.Errorf("page 0x%x is out of JMP rel32 reach of the vDSO (0x%x)", page, near)
	}
}

// A signal that arrives while the injector drives the target (the Go runtime
// sends SIGURG constantly) used to abort the injection with "expected
// SIGTRAP". The target must also still receive it afterwards.
func TestRemoteSyscallToleratesSignals(t *testing.T) {
	cmd, _ := startBusyClockChild(t)
	pid := cmd.Process.Pid

	tr := procmem.NewTracer()
	defer tr.Close()
	if err := tr.Seize(pid); err != nil {
		if errors.Is(err, unix.EPERM) {
			t.Skipf("ptrace not permitted here: %v", err)
		}
		t.Fatal(err)
	}
	defer tr.Detach() //nolint:errcheck

	info, err := vdso.Locate(pid)
	if err != nil {
		t.Fatal(err)
	}
	// Queue several signals for the thread we are about to run.
	for i := 0; i < 5; i++ {
		syscall.Tgkill(pid, pid, syscall.SIGURG) //nolint:errcheck
	}
	got, err := remoteSyscall(tr, pid, info.ClockGettimeAddr, syscall.SYS_GETPID)
	if err != nil {
		t.Fatalf("remoteSyscall: %v", err)
	}
	if int(got) != pid {
		t.Errorf("getpid in the target returned %d, want %d", got, pid)
	}
}
