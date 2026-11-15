//go:build linux

package preload

import (
	"bufio"
	"bytes"
	"debug/elf"
	"encoding/binary"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unsafe"

	"github.com/bkaznowski/epochd/pkg/trampoline"
)

// secondsTolerance absorbs process startup and `date`'s one-second resolution.
const secondsTolerance = 3

func requireTool(t *testing.T, name string) string {
	t.Helper()
	p, err := exec.LookPath(name)
	if err != nil {
		t.Skipf("%s not available: %v", name, err)
	}
	return p
}

// epochOf parses the output of `date -u +%s`.
func epochOf(t *testing.T, out []byte) int64 {
	t.Helper()
	n, err := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
	if err != nil {
		t.Fatalf("unexpected output %q: %v", out, err)
	}
	return n
}

func within(got, want int64) bool {
	d := got - want
	return d >= -secondsTolerance && d <= secondsTolerance
}

func TestAdvancingClock(t *testing.T) {
	requireTool(t, "date")
	target := time.Now().Add(48 * time.Hour)

	cmd := exec.Command("date", "-u", "+%s")
	var out bytes.Buffer
	cmd.Stdout = &out
	h, err := Start(cmd, target)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer h.Close()
	if err := cmd.Wait(); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if got := epochOf(t, out.Bytes()); !within(got, target.Unix()) {
		t.Errorf("date printed %d, want ~%d (target %s)", got, target.Unix(), target)
	}
}

func TestNegativeOffset(t *testing.T) {
	requireTool(t, "date")
	target := time.Now().Add(-90*24*time.Hour - 500*time.Millisecond)

	cmd := exec.Command("date", "-u", "+%s")
	var out bytes.Buffer
	cmd.Stdout = &out
	h, err := Start(cmd, target)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer h.Close()
	if err := cmd.Wait(); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if got := epochOf(t, out.Bytes()); !within(got, target.Unix()) {
		t.Errorf("date printed %d, want ~%d (target %s)", got, target.Unix(), target)
	}
}

// A frozen clock must return the same instant to every process in the tree, and
// the child processes here (sh -> date, date) prove LD_PRELOAD is inherited.
func TestFrozenClockAcrossChildren(t *testing.T) {
	requireTool(t, "sh")
	requireTool(t, "date")
	requireTool(t, "sleep")
	target := time.Date(2031, 3, 4, 5, 6, 7, 0, time.UTC)

	cmd := exec.Command("sh", "-c", "date -u +%s; sleep 1.2; date -u +%s")
	var out bytes.Buffer
	cmd.Stdout = &out
	h, err := StartFrozen(cmd, target)
	if err != nil {
		t.Fatalf("StartFrozen: %v", err)
	}
	defer h.Close()
	if err := cmd.Wait(); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	lines := strings.Fields(out.String())
	if len(lines) != 2 {
		t.Fatalf("want 2 lines of output, got %q", out.String())
	}
	for i, l := range lines {
		got, _ := strconv.ParseInt(l, 10, 64)
		if got != target.Unix() {
			t.Errorf("line %d = %d, want exactly %d (clock should not advance)", i, got, target.Unix())
		}
	}
}

// SetTime, Freeze and Reset change the clock of a process that is already
// running, which is the point of keeping the state in shared memory.
func TestLiveUpdates(t *testing.T) {
	requireTool(t, "sh")
	requireTool(t, "date")

	cmd := exec.Command("sh", "-c", "while read x; do date -u +%s; done")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	h, err := Start(cmd, time.Now().Add(24*time.Hour))
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() {
		stdin.Close()
		cmd.Wait() //nolint:errcheck
		h.Close()
	}()

	buf := make([]byte, 64)
	read := func() int64 {
		t.Helper()
		if _, err := stdin.Write([]byte("go\n")); err != nil {
			t.Fatalf("write: %v", err)
		}
		n, err := stdout.Read(buf)
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		return epochOf(t, buf[:n])
	}

	if got, want := read(), time.Now().Add(24*time.Hour).Unix(); !within(got, want) {
		t.Errorf("initial: got %d, want ~%d", got, want)
	}

	later := time.Now().Add(10 * 24 * time.Hour)
	if err := h.SetTime(later); err != nil {
		t.Fatal(err)
	}
	if got := read(); !within(got, later.Unix()) {
		t.Errorf("after SetTime: got %d, want ~%d", got, later.Unix())
	}

	pinned := time.Date(2040, 1, 2, 3, 4, 5, 0, time.UTC)
	if err := h.Freeze(pinned); err != nil {
		t.Fatal(err)
	}
	if got := read(); got != pinned.Unix() {
		t.Errorf("after Freeze: got %d, want exactly %d", got, pinned.Unix())
	}

	if err := h.Reset(); err != nil {
		t.Fatal(err)
	}
	if got := read(); !within(got, time.Now().Unix()) {
		t.Errorf("after Reset: got %d, want ~%d (real time)", got, time.Now().Unix())
	}
}

// One Handle is one clock for several independent processes.
func TestSharedClockAcrossProcesses(t *testing.T) {
	requireTool(t, "date")
	target := time.Now().Add(72 * time.Hour)

	h, err := New(target)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()

	for i := 0; i < 3; i++ {
		cmd := exec.Command("date", "-u", "+%s")
		var out bytes.Buffer
		cmd.Stdout = &out
		if err := h.Start(cmd); err != nil {
			t.Fatalf("Start #%d: %v", i, err)
		}
		if err := cmd.Wait(); err != nil {
			t.Fatalf("Wait #%d: %v", i, err)
		}
		if got := epochOf(t, out.Bytes()); !within(got, target.Unix()) {
			t.Errorf("process %d: got %d, want ~%d", i, got, target.Unix())
		}
	}
}

func TestEnv(t *testing.T) {
	h, err := New(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()

	env := h.Env([]string{"A=1", "LD_PRELOAD=/other.so", "EPOCHD_STATE_FILE=/stale", "B=2"})
	get := func(k string) []string {
		var vs []string
		for _, kv := range env {
			if strings.HasPrefix(kv, k+"=") {
				vs = append(vs, strings.TrimPrefix(kv, k+"="))
			}
		}
		return vs
	}
	if got := get("LD_PRELOAD"); len(got) != 1 || got[0] != h.libPath+":/other.so" {
		t.Errorf("LD_PRELOAD = %q, want ours followed by the existing one", got)
	}
	if got := get("EPOCHD_STATE_FILE"); len(got) != 1 || got[0] != h.statePath {
		t.Errorf("EPOCHD_STATE_FILE = %q, want exactly %q", got, h.statePath)
	}
	if len(get("A")) != 1 || len(get("B")) != 1 {
		t.Errorf("unrelated variables were not preserved: %q", env)
	}

	if got := (&Handle{libPath: "/lib.so", statePath: "/s"}).Env([]string{"LD_PRELOAD="}); got[len(got)-2] != "LD_PRELOAD=/lib.so" {
		t.Errorf("empty LD_PRELOAD should not leave a dangling colon: %q", got)
	}
}

func TestCloseRemovesFilesAndRejectsWrites(t *testing.T) {
	h, err := New(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	dir := h.dir
	if err := h.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("temp dir %s still exists after Close (err=%v)", dir, err)
	}
	if err := h.SetTime(time.Now()); err == nil {
		t.Error("SetTime after Close should fail")
	}
	if err := h.Close(); err != nil {
		t.Errorf("second Close should be a no-op, got %v", err)
	}
}

// The state file shares the trampoline's field order, so both implementations
// can be driven from the same notion of a clock.
func TestStateLayoutMatchesTrampoline(t *testing.T) {
	target := time.Date(2033, 5, 6, 7, 8, 9, 123456789, time.UTC)
	h, err := NewFrozen(target)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()

	got, err := os.ReadFile(h.statePath)
	if err != nil {
		t.Fatal(err)
	}
	want := trampoline.EncodeState(target.Unix(), int64(target.Nanosecond()), trampoline.MaskFrozen, 0)
	if len(got) != trampoline.StateSize {
		t.Fatalf("state file is %d bytes, want %d", len(got), trampoline.StateSize)
	}
	if !bytes.Equal(got[:offSeq], want[:offSeq]) {
		t.Errorf("state fields differ from trampoline layout:\n got % x\nwant % x", got[:offSeq], want[:offSeq])
	}
	if seq := binary.LittleEndian.Uint32(got[offSeq:]); seq%2 != 0 || seq == 0 {
		t.Errorf("seq = %d, want a nonzero even value after a completed write", seq)
	}
}

func TestCheckInterposable(t *testing.T) {
	t.Run("go binary is rejected", func(t *testing.T) {
		self, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		err = CheckInterposable(self)
		if !errors.Is(err, ErrNotInterposable) || !strings.Contains(err.Error(), "Go binary") {
			t.Errorf("got %v, want ErrNotInterposable mentioning a Go binary", err)
		}
	})
	t.Run("dynamic C binary is accepted", func(t *testing.T) {
		requireTool(t, "date")
		if err := CheckInterposable("date"); err != nil {
			t.Errorf("date: %v", err)
		}
	})
	t.Run("script is accepted", func(t *testing.T) {
		p := filepath.Join(t.TempDir(), "s.sh")
		if err := os.WriteFile(p, []byte("#!/bin/sh\ntrue\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := CheckInterposable(p); err != nil {
			t.Errorf("script: %v", err)
		}
	})
	t.Run("Start refuses a Go binary and leaves nothing behind", func(t *testing.T) {
		self, _ := os.Executable()
		before, _ := filepath.Glob(filepath.Join(os.TempDir(), "epochd-preload-*"))
		if _, err := Start(exec.Command(self), time.Now()); !errors.Is(err, ErrNotInterposable) {
			t.Errorf("got %v, want ErrNotInterposable", err)
		}
		after, _ := filepath.Glob(filepath.Join(os.TempDir(), "epochd-preload-*"))
		if len(after) > len(before) {
			t.Errorf("temp dir leaked on refusal: before=%v after=%v", before, after)
		}
	})
}

// Guards the compatibility floor: Redpanda bundles glibc 2.35, so the embedded
// library must not require anything newer. Catches rebuilding it on a newer distro.
func TestEmbeddedLibrary(t *testing.T) {
	f, err := elf.NewFile(bytes.NewReader(library))
	if err != nil {
		t.Fatalf("embedded library is not a valid ELF file: %v", err)
	}
	defer f.Close()
	if f.Type != elf.ET_DYN {
		t.Errorf("type = %v, want a shared object", f.Type)
	}

	syms, err := f.DynamicSymbols()
	if err != nil {
		t.Fatal(err)
	}
	exported := map[string]bool{}
	for _, s := range syms {
		if s.Section != elf.SHN_UNDEF {
			exported[s.Name] = true
		}
	}
	for _, name := range []string{"clock_gettime", "gettimeofday", "time"} {
		if !exported[name] {
			t.Errorf("library does not export %s", name)
		}
	}

	// The same file must load under musl, whose loader has no libc.so.6 and no
	// fortified *_chk functions: a DT_NEEDED or such an import makes it fail to
	// load there (and the process then runs on real time).
	if needed, _ := f.ImportedLibraries(); len(needed) != 0 {
		t.Errorf("library depends on %v; it must have no DT_NEEDED so it also loads under musl", needed)
	}
	imports, err := f.ImportedSymbols()
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range imports {
		if strings.Contains(s.Name, "_chk") {
			t.Errorf("import %s is glibc-only; build with -U_FORTIFY_SOURCE -fno-stack-protector", s.Name)
		}
	}
	for _, s := range imports {
		if !strings.HasPrefix(s.Version, "GLIBC_2.") {
			continue
		}
		minor, _ := strconv.Atoi(strings.Split(strings.TrimPrefix(s.Version, "GLIBC_2."), ".")[0])
		if minor > 35 {
			t.Errorf("import %s requires %s, newer than the glibc 2.35 floor", s.Name, s.Version)
		}
	}
}

// The library counts each process that loads it, so a command that bypassed the
// preload is distinguishable from one the clock simply did not move.
func TestAttachedCountsLoadedProcesses(t *testing.T) {
	requireTool(t, "date")

	t.Run("a process that loads the library is counted", func(t *testing.T) {
		cmd := exec.Command("date", "-u", "+%s")
		h, err := Start(cmd, time.Now().Add(time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		defer h.Close()
		if err := cmd.Wait(); err != nil {
			t.Fatal(err)
		}
		if got := h.Attached(); got != 1 {
			t.Errorf("Attached() = %d after one process, want 1", got)
		}
	})

	t.Run("a process started without the preload environment is not", func(t *testing.T) {
		h, err := New(time.Now().Add(time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		defer h.Close()
		if err := exec.Command("date").Run(); err != nil { // plain environment
			t.Fatal(err)
		}
		if got := h.Attached(); got != 0 {
			t.Errorf("Attached() = %d, want 0 when nothing loaded the library", got)
		}
	})

	t.Run("each exec counts and a closed handle reports zero", func(t *testing.T) {
		requireTool(t, "sh")
		cmd := exec.Command("sh", "-c", "date >/dev/null; date >/dev/null")
		h, err := Start(cmd, time.Now().Add(time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		if err := cmd.Wait(); err != nil {
			t.Fatal(err)
		}
		if got := h.Attached(); got != 3 { // sh + two dates
			t.Errorf("Attached() = %d, want 3 (sh and two dates)", got)
		}
		h.Close()
		if got := h.Attached(); got != 0 {
			t.Errorf("Attached() after Close = %d, want 0", got)
		}
	})

	t.Run("counting must not disturb the clock fields", func(t *testing.T) {
		target := time.Date(2034, 1, 2, 3, 4, 5, 0, time.UTC)
		cmd := exec.Command("date", "-u", "+%s")
		var out bytes.Buffer
		cmd.Stdout = &out
		h, err := StartFrozen(cmd, target)
		if err != nil {
			t.Fatal(err)
		}
		defer h.Close()
		cmd.Wait() //nolint:errcheck
		if got := epochOf(t, out.Bytes()); got != target.Unix() {
			t.Errorf("date printed %d, want exactly %d", got, target.Unix())
		}
		b, _ := os.ReadFile(h.statePath)
		if binary.LittleEndian.Uint64(b[offSec:]) != uint64(target.Unix()) {
			t.Errorf("a loading process changed the clock fields: % x", b)
		}
	})
}

// A child that changes uid cannot enter a 0700 directory, and the loader then
// drops the preload without a word. The directory has to be traversable.
func TestTempDirIsTraversable(t *testing.T) {
	h, err := New(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	fi, err := os.Stat(h.dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := fi.Mode().Perm(); got != 0o711 {
		t.Errorf("dir mode = %o, want 711 (others may traverse, not list)", got)
	}
	for _, p := range []string{h.libPath, h.statePath} {
		fi, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm()&0o004 == 0 {
			t.Errorf("%s mode %o is not world-readable", p, fi.Mode().Perm())
		}
		if fi.Mode().Perm()&0o002 != 0 {
			t.Errorf("%s mode %o is world-writable", p, fi.Mode().Perm())
		}
	}
}

// InUse is how Session.Prune tells a clock nobody is using from one a
// descendant still reads.
func TestInUseFollowsRunningProcesses(t *testing.T) {
	requireTool(t, "sleep")
	cmd := exec.Command("sleep", "60")
	h, err := Start(cmd, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()

	// The loader maps the library before main runs, but give exec a moment.
	deadline := time.Now().Add(5 * time.Second)
	for !h.InUse() && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if !h.InUse() {
		t.Fatal("InUse() = false while a process started through the handle is running")
	}

	cmd.Process.Kill() //nolint:errcheck
	cmd.Wait()         //nolint:errcheck
	if h.InUse() {
		t.Error("InUse() = true after the only process exited")
	}

	// A handle nothing was ever started on is not in use either (this process
	// maps the state itself and must not count).
	idle, err := New(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	defer idle.Close()
	if idle.InUse() {
		t.Error("InUse() = true for a handle nothing was started on; the controlling process must not count itself")
	}
}

// If the writer dies mid-update the sequence number stays odd for good. A
// long-lived reader must carry on with the clock it last saw instead of
// snapping back to real time, which its program would see as a jump of the
// whole offset.
func TestStuckWriterKeepsLastGoodClock(t *testing.T) {
	requireTool(t, "bash") // needs one process that reads the clock repeatedly
	cmd := exec.Command("bash", "-c", `while read x; do printf '%(%s)T\n' -1; done`)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	target := time.Now().Add(72 * time.Hour)
	h, err := Start(cmd, target)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	defer func() { stdin.Close(); cmd.Wait() }() //nolint:errcheck
	rd := bufio.NewReader(stdout)
	poke := func() int64 {
		t.Helper()
		if _, err := stdin.Write([]byte("go\n")); err != nil {
			t.Fatal(err)
		}
		line, err := rd.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		return epochOf(t, []byte(line))
	}

	if got := poke(); !within(got, target.Unix()) {
		t.Fatalf("before the writer died the process saw %d, want ~%d", got, target.Unix())
	}

	// Simulate a writer killed between its two increments.
	h.mu.Lock()
	atomic.AddUint32((*uint32)(unsafe.Pointer(&h.mem[offSeq])), 1)
	h.mu.Unlock()

	if got := poke(); !within(got, target.Unix()) {
		t.Errorf("with a stuck writer the process saw %d, want the last good clock ~%d (real time is ~%d)",
			got, target.Unix(), time.Now().Unix())
	}
}

func TestCheckStrict(t *testing.T) {
	requireTool(t, "date")
	is := func(t *testing.T, err error, substr string) {
		t.Helper()
		if !errors.Is(err, ErrNotInterposable) || !strings.Contains(err.Error(), substr) {
			t.Errorf("got %v, want ErrNotInterposable mentioning %q", err, substr)
		}
	}

	t.Run("dynamic C binary passes", func(t *testing.T) {
		if err := CheckStrict("date"); err != nil {
			t.Errorf("date: %v", err)
		}
	})
	t.Run("go binary is refused", func(t *testing.T) {
		self, _ := os.Executable()
		is(t, CheckStrict(self), "Go binary")
	})
	t.Run("script is refused although CheckInterposable lets it through", func(t *testing.T) {
		p := filepath.Join(t.TempDir(), "s.sh")
		if err := os.WriteFile(p, []byte("#!/bin/sh\ntrue\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := CheckInterposable(p); err != nil {
			t.Fatalf("premise: CheckInterposable should accept a script, got %v", err)
		}
		is(t, CheckStrict(p), "not an ELF")
	})
	t.Run("setuid copy is refused", func(t *testing.T) {
		src, err := exec.LookPath("date")
		if err != nil {
			t.Fatal(err)
		}
		b, err := os.ReadFile(src)
		if err != nil {
			t.Fatal(err)
		}
		p := filepath.Join(t.TempDir(), "date-suid")
		if err := os.WriteFile(p, b, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(p, 0o755|os.ModeSetuid); err != nil {
			t.Skipf("cannot set the setuid bit here: %v", err)
		}
		is(t, CheckStrict(p), "setuid")
	})
	t.Run("missing file is refused", func(t *testing.T) {
		is(t, CheckStrict(filepath.Join(t.TempDir(), "nope")), "no such file")
	})
}
