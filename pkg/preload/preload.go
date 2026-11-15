//go:build linux

// Package preload fakes the wall clock for a process tree with LD_PRELOAD
// instead of ptrace and vDSO patching (see pkg/inject for that approach).
//
// A Handle is one clock. It owns a small shared-memory state file and a copy of
// the preload library (libepochd_preload.so, embedded below). Every process
// started through Handle.Start inherits LD_PRELOAD and EPOCHD_STATE_FILE in its
// environment, so the whole tree (fork and exec alike) sees the same clock
// with no tracer attached, no CAP_SYS_PTRACE and no per-exec re-injection. The
// clock can be changed while the tree runs, because the library reads the
// shared state on every call.
//
// Trade-offs against the vDSO path:
//
//   - The library interposes libc's clock_gettime, gettimeofday and time, so it
//     cannot affect statically linked binaries or Go binaries (the Go runtime
//     reads the vDSO directly), and it is bypassed by raw syscalls. Start
//     refuses such binaries with ErrNotInterposable rather than silently
//     leaving them on real time.
//   - It only covers descendants that keep the environment. A child started
//     with a scrubbed environment (env -i, some sudo configurations) runs on
//     real time.
//   - It works on processes you start, not on ones already running.
//   - The advancing path calls the real clock_gettime through libc, so it keeps
//     the vDSO fast path that the trampoline gives up.
//   - Only the wall clock moves. Timeouts expressed as an absolute CLOCK_REALTIME
//     deadline (pthread_cond_timedwait, sem_timedwait, ...) are handed to the
//     kernel unchanged, so a process computing "now + 5s" from the fake clock
//     waits against the real one. Relative timeouts and the monotonic clocks are
//     unaffected and behave normally.
//
// One library file serves glibc and musl systems alike; Attached reports whether
// it ever loaded into a process, so a command that bypassed it is noticed.
package preload

import (
	"bufio"
	"bytes"
	"debug/buildinfo"
	"debug/elf"
	_ "embed"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"

	"github.com/bkaznowski/epochd/pkg/trampoline"
)

//go:embed libepochd_preload.so
var library []byte

// ErrNotInterposable is returned (wrapped) when a binary cannot be affected by
// LD_PRELOAD: it is a Go binary or statically linked.
var ErrNotInterposable = errors.New("preload: binary cannot be affected by LD_PRELOAD")

const (
	libName   = "libepochd_preload.so"
	stateName = "state"

	// The state file uses the trampoline's field order (see trampoline.go and
	// lib/epochd_preload.c); offSeq is a seqlock counter here rather than the
	// trampoline's plain generation counter.
	offSec  = 0
	offNsec = 8
	offMask = 16
	offSeq  = 24

	// offAttached counts the processes that loaded the library (the library
	// increments it at load time; see lib/epochd_preload.c).
	offAttached = 28
)

// Handle is one fake clock shared by every process started through it.
type Handle struct {
	dir       string
	libPath   string
	statePath string

	mu  sync.Mutex
	mem []byte // MAP_SHARED view of the state file; nil once closed
}

// New creates a clock that advances from target: a process asked for the time
// right now sees target, and one asked a second later sees target plus a second.
func New(target time.Time) (*Handle, error) {
	return newHandle(func(h *Handle) error { return h.SetTime(target) })
}

// NewFrozen creates a clock stopped at target.
func NewFrozen(target time.Time) (*Handle, error) {
	return newHandle(func(h *Handle) error { return h.Freeze(target) })
}

func newHandle(init func(*Handle) error) (h *Handle, err error) {
	dir, err := os.MkdirTemp("", "epochd-preload-*")
	if err != nil {
		return nil, fmt.Errorf("preload: %w", err)
	}
	h = &Handle{
		dir:       dir,
		libPath:   filepath.Join(dir, libName),
		statePath: filepath.Join(dir, stateName),
	}
	defer func() {
		if err != nil {
			h.Close() //nolint:errcheck
			h = nil
		}
	}()

	// MkdirTemp creates the directory 0700, which would stop a child that
	// changes uid (su, sudo -u, a daemon dropping privileges) from opening the
	// library: the loader then ignores the preload with a one-line warning and
	// that process runs on real time. 0711 lets anyone who knows the random
	// name reach the files without being able to list the directory; the files
	// are the public library and a clock, and only we can write to either.
	if err = os.Chmod(dir, 0o711); err != nil {
		return h, fmt.Errorf("preload: %w", err)
	}

	// 0755: the dynamic loader mmaps the library with PROT_EXEC, which also
	// means $TMPDIR must not be on a noexec mount.
	if err = os.WriteFile(h.libPath, library, 0o755); err != nil {
		return h, fmt.Errorf("preload: write library: %w", err)
	}
	f, err := os.OpenFile(h.statePath, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return h, fmt.Errorf("preload: create state file: %w", err)
	}
	defer f.Close()
	if err = f.Truncate(trampoline.StateSize); err != nil {
		return h, fmt.Errorf("preload: size state file: %w", err)
	}
	h.mem, err = syscall.Mmap(int(f.Fd()), 0, trampoline.StateSize,
		syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_SHARED)
	if err != nil {
		return h, fmt.Errorf("preload: map state file: %w", err)
	}
	if err = init(h); err != nil {
		return h, err
	}
	return h, nil
}

// SetTime sets the clock to target in advancing mode, taking effect for every
// process using this Handle without stopping any of them.
func (h *Handle) SetTime(target time.Time) error {
	d := time.Until(target)
	return h.write(int64(d/time.Second), int64(d%time.Second), trampoline.MaskEnabled)
}

// Freeze stops the clock at target: every read returns exactly target until
// SetTime, Freeze or Reset is called.
func (h *Handle) Freeze(target time.Time) error {
	return h.write(target.Unix(), int64(target.Nanosecond()), trampoline.MaskFrozen)
}

// Reset snaps the clock back to real time.
func (h *Handle) Reset() error { return h.SetTime(time.Now()) }

// write publishes a new state under a seqlock so readers in other processes
// never act on a half-written mix of the old and new fields.
func (h *Handle) write(sec, nsec int64, mask uint64) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.mem == nil {
		return errors.New("preload: handle is closed")
	}
	seq := (*uint32)(unsafe.Pointer(&h.mem[offSeq]))
	atomic.AddUint32(seq, 1) // odd: update in progress
	binary.LittleEndian.PutUint64(h.mem[offSec:], uint64(sec))
	binary.LittleEndian.PutUint64(h.mem[offNsec:], uint64(nsec))
	binary.LittleEndian.PutUint64(h.mem[offMask:], mask)
	atomic.AddUint32(seq, 1) // even: stable
	return nil
}

// Attached returns how many processes have loaded the library for this clock. It
// is how a caller tells "the clock is not working" from "the library never got
// into the process": zero after the command has run means the preload was
// bypassed (a static or Go binary behind a wrapper, a scrubbed environment, a
// noexec $TMPDIR, an incompatible libc) and the command saw real time.
//
// Each exec of a binary that loads the library counts once; a plain fork, which
// shares the parent's mapping, does not. A process running as a different user
// than the one that created the Handle cannot open the state file for writing
// and is not counted, though it still reads the clock.
func (h *Handle) Attached() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.mem == nil {
		return 0
	}
	return int(atomic.LoadUint32((*uint32)(unsafe.Pointer(&h.mem[offAttached]))))
}

// InUse reports whether any other process on the machine still has this clock's
// library or state mapped, that is, whether a process started through the
// Handle (or one of its descendants) is still running with it. Closing a Handle
// that is in use removes the library out from under children that exec later.
//
// It reads /proc/*/maps, so it sees only processes this user may inspect; one
// it cannot read is treated as not using the clock.
func (h *Handle) InUse() bool {
	marker := []byte(h.dir + string(filepath.Separator))
	self := os.Getpid()
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return false
	}
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid == self {
			continue
		}
		if mapsMention(filepath.Join("/proc", e.Name(), "maps"), marker) {
			return true
		}
	}
	return false
}

// mapsMention reports whether the maps file at path has a line containing marker.
func mapsMention(path string, marker []byte) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for sc.Scan() {
		if bytes.Contains(sc.Bytes(), marker) {
			return true
		}
	}
	return false
}

// Env returns base (os.Environ() when nil) with LD_PRELOAD and EPOCHD_STATE_FILE
// set for this clock. An existing LD_PRELOAD is kept after ours.
func (h *Handle) Env(base []string) []string {
	if base == nil {
		base = os.Environ()
	}
	preload := h.libPath
	out := make([]string, 0, len(base)+2)
	for _, kv := range base {
		switch {
		case strings.HasPrefix(kv, "LD_PRELOAD="):
			if v := strings.TrimPrefix(kv, "LD_PRELOAD="); v != "" {
				preload = h.libPath + ":" + v
			}
		case strings.HasPrefix(kv, "EPOCHD_STATE_FILE="):
			// replaced below
		default:
			out = append(out, kv)
		}
	}
	return append(out, "LD_PRELOAD="+preload, "EPOCHD_STATE_FILE="+h.statePath)
}

// Start starts cmd on this clock. It returns an error wrapping ErrNotInterposable
// when cmd's binary cannot be affected by LD_PRELOAD. Call it for as many
// commands as should share the clock. The caller still owns cmd (Wait, etc.).
func (h *Handle) Start(cmd *exec.Cmd) error {
	if err := CheckInterposable(cmd.Path); err != nil {
		return err
	}
	cmd.Env = h.Env(cmd.Env)
	return cmd.Start()
}

// Close unmaps the state and removes the temporary directory. Call it only
// after every process started through the Handle has exited: children that
// exec later load the library by path.
func (h *Handle) Close() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	var err error
	if h.mem != nil {
		err = syscall.Munmap(h.mem)
		h.mem = nil
	}
	if rerr := os.RemoveAll(h.dir); err == nil {
		err = rerr
	}
	return err
}

// Start starts cmd on a new advancing clock that reads target right now, and
// returns the Handle controlling it. Close the Handle after cmd exits.
func Start(cmd *exec.Cmd, target time.Time) (*Handle, error) {
	h, err := New(target)
	if err != nil {
		return nil, err
	}
	if err := h.Start(cmd); err != nil {
		h.Close() //nolint:errcheck
		return nil, err
	}
	return h, nil
}

// StartFrozen is like Start with the clock stopped at target.
func StartFrozen(cmd *exec.Cmd, target time.Time) (*Handle, error) {
	h, err := NewFrozen(target)
	if err != nil {
		return nil, err
	}
	if err := h.Start(cmd); err != nil {
		h.Close() //nolint:errcheck
		return nil, err
	}
	return h, nil
}

// CheckStrict is CheckInterposable for a caller that must be sure, because the
// alternative to preload is a mode that always works and a wrong "yes" leaves the
// process on real time without a word. It returns nil only for a binary that is
// positively a dynamically linked, non-Go ELF file the loader will honour
// LD_PRELOAD for. Everything CheckInterposable lets through as "cannot tell" is
// refused here:
//
//   - a shell script or other non-ELF file: it may go on to exec a Go or static
//     binary, which would stay on real time;
//   - a setuid or setgid file, or one with file capabilities: the loader runs
//     such programs in secure-execution mode and ignores LD_PRELOAD;
//   - a path that cannot be read.
//
// Errors wrap ErrNotInterposable. The check still sees only the binary itself:
// a dynamic C program that later execs a Go child passes.
func CheckStrict(path string) error {
	if resolved, err := exec.LookPath(path); err == nil {
		path = resolved
	}
	fi, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrNotInterposable, err)
	}
	if fi.Mode()&(os.ModeSetuid|os.ModeSetgid) != 0 {
		return fmt.Errorf("%w: %s is setuid/setgid, so the loader ignores LD_PRELOAD for it", ErrNotInterposable, path)
	}
	if sz, err := unix.Getxattr(path, "security.capability", nil); err == nil && sz > 0 {
		return fmt.Errorf("%w: %s has file capabilities, so the loader ignores LD_PRELOAD for it", ErrNotInterposable, path)
	}
	if err := CheckInterposable(path); err != nil {
		return err
	}
	f, err := elf.Open(path)
	if err != nil {
		return fmt.Errorf("%w: %s is not an ELF binary (a script may exec something LD_PRELOAD cannot affect)", ErrNotInterposable, path)
	}
	return f.Close()
}

// CheckInterposable reports whether LD_PRELOAD can affect the binary at path,
// returning an error wrapping ErrNotInterposable when it cannot: Go binaries
// read the vDSO directly and static binaries load no dynamic loader.
//
// Anything it cannot classify (a shell script, say) passes. It inspects only
// the binary itself, not what that binary goes on to execute.
func CheckInterposable(path string) error {
	if resolved, err := exec.LookPath(path); err == nil {
		path = resolved
	}
	if _, err := buildinfo.ReadFile(path); err == nil {
		return fmt.Errorf("%w: %s is a Go binary, whose runtime reads the vDSO directly", ErrNotInterposable, path)
	}
	f, err := elf.Open(path)
	if err != nil {
		return nil // not an ELF file (script, ...): cannot tell, let it run
	}
	defer f.Close()
	for _, p := range f.Progs {
		if p.Type == elf.PT_INTERP {
			return nil
		}
	}
	return fmt.Errorf("%w: %s is statically linked", ErrNotInterposable, path)
}
