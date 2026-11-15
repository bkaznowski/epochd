//go:build linux

// Package inject ties vDSO discovery, ptrace, and the trampoline payload into
// the public injection API.
package inject

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/bkaznowski/epochd/pkg/procmem"
	"github.com/bkaznowski/epochd/pkg/trampoline"
	"github.com/bkaznowski/epochd/pkg/vdso"
	"golang.org/x/sys/unix"
)

// ErrUninstalled is returned by a Handle's methods after Uninstall removed its
// hooks.
var ErrUninstalled = errors.New("inject: the hooks were uninstalled from this process")

// pageSize is the granule everything here maps, searches and unmaps in.
const pageSize = uintptr(4096)

// hookLen is how many bytes of a vDSO entry point a hook overwrites with its
// JMP rel32. patchSpan is how much around an entry no thread may be executing
// while we rewrite it: the JMP itself, plus the 8-byte word PTRACE_POKETEXT
// reads and writes back.
const (
	hookLen   = 5
	patchSpan = 8
)

// Handle represents an active time-offset injection in a target process.
// Obtain one via InjectAtTime or InjectFrozen; update with SetTime or Freeze.
// A Handle is safe for concurrent use.
type Handle struct {
	PID       int
	StateAddr uintptr        // address of the trampoline's mutable state struct in the target
	patched   [3]patchedHook // clock_gettime, gettimeofday, time -- original vDSO bytes before our JMP patch

	mu          sync.Mutex
	gen         uint32 // monotonically incremented on every SetOffset/SetTime/Freeze call
	uninstalled bool
}

// patchedHook records a single vDSO entry point we overwrote with a JMP, so a
// future uninstall can restore it.
type patchedHook struct {
	addr uintptr
	orig [hookLen]byte
}

// InjectAtTime injects the trampoline into pid and sets its clock to target,
// after which time flows forward at the real rate. This is the primary entry
// point for advancing-mode injection; it converts the absolute timestamp to an
// internal offset as close to the actual write as possible.
func InjectAtTime(pid int, target time.Time) (*Handle, error) {
	sec, nsec := diffSecNsec(target, time.Now())
	return injectCore(pid, sec, nsec, trampoline.MaskEnabled)
}

// InjectFrozen injects the trampoline into pid with the clock frozen at target.
// Unlike InjectAtTime, the returned time never advances — every call to
// clock_gettime returns exactly target until SetTime or Freeze changes it.
func InjectFrozen(pid int, target time.Time) (*Handle, error) {
	sec := target.Unix()
	nsec := int64(target.Nanosecond())
	return injectCore(pid, sec, nsec, trampoline.MaskFrozen)
}

// InjectAtTimeFollowChild starts cmd (which must have SysProcAttr.Ptrace set)
// and injects the trampoline into it. The caller must not call cmd.Start()
// itself: cmd is started on the Tracer's own pinned OS thread so that the
// PTRACE_TRACEME relationship established by the fork, and every ptrace
// request this function goes on to issue, come from the same thread — see
// procmem.Tracer's doc comment for why that's required for correctness, not
// just style. No elevated permissions required.
func InjectAtTimeFollowChild(cmd *exec.Cmd, target time.Time) (*Handle, error) {
	sec, nsec := diffSecNsec(target, time.Now())
	return injectFollowChild(cmd, sec, nsec, trampoline.MaskEnabled)
}

// InjectFrozenFollowChild is like InjectAtTimeFollowChild but starts the clock
// in freeze mode so time never advances.
func InjectFrozenFollowChild(cmd *exec.Cmd, target time.Time) (*Handle, error) {
	sec := target.Unix()
	nsec := int64(target.Nanosecond())
	return injectFollowChild(cmd, sec, nsec, trampoline.MaskFrozen)
}

// InjectAtTimeFollowChildKeepTracer is like InjectAtTimeFollowChild but keeps
// ptrace active on the parent with PTRACE_O_TRACEFORK|PTRACE_O_TRACEVFORK set
// and resumes the parent before returning. The caller owns the Tracer and must
// drive the event loop, calling tr.InterruptDetach() when done.
func InjectAtTimeFollowChildKeepTracer(cmd *exec.Cmd, target time.Time) (*Handle, *procmem.Tracer, error) {
	sec, nsec := diffSecNsec(target, time.Now())
	return injectFollowChildKeepTracer(cmd, sec, nsec, trampoline.MaskEnabled)
}

// InjectFrozenFollowChildKeepTracer is like InjectFrozenFollowChild but keeps
// ptrace active on the parent with PTRACE_O_TRACEFORK|PTRACE_O_TRACEVFORK set.
func InjectFrozenFollowChildKeepTracer(cmd *exec.Cmd, target time.Time) (*Handle, *procmem.Tracer, error) {
	sec := target.Unix()
	nsec := int64(target.Nanosecond())
	return injectFollowChildKeepTracer(cmd, sec, nsec, trampoline.MaskFrozen)
}

// InjectAtTimeKeepTracer attaches to an already-running process, injects the
// trampoline in advancing mode, sets PTRACE_O_TRACEFORK|PTRACE_O_TRACEVFORK|
// PTRACE_O_TRACEEXEC, and resumes it. The caller owns the Tracer and must drive
// the event loop, calling tr.InterruptDetach() when done.
// Requires CAP_SYS_PTRACE and ptrace_scope <= 1.
func InjectAtTimeKeepTracer(pid int, target time.Time) (*Handle, *procmem.Tracer, error) {
	sec, nsec := diffSecNsec(target, time.Now())
	return injectCoreKeepTracer(pid, sec, nsec, trampoline.MaskEnabled)
}

// InjectFrozenKeepTracer is like InjectAtTimeKeepTracer but starts the clock
// in freeze mode.
func InjectFrozenKeepTracer(pid int, target time.Time) (*Handle, *procmem.Tracer, error) {
	sec := target.Unix()
	nsec := int64(target.Nanosecond())
	return injectCoreKeepTracer(pid, sec, nsec, trampoline.MaskFrozen)
}

// injectCoreKeepTracer attaches to pid via PTRACE_SEIZE+PTRACE_INTERRUPT,
// injects the trampoline, enables fork/exec tracing options, and resumes the
// process without detaching. The returned Tracer is ready to drive the event
// loop. SEIZE is used instead of PTRACE_ATTACH so that:
//   - no SIGSTOP is delivered to the thread group (avoids group-stop races in
//     remoteMmap that would cause it to see SIGSTOP instead of SIGTRAP), and
//   - PTRACE_INTERRUPT works reliably in InterruptDetach when closing the tracker.
//
// Every other thread of the target is stopped for the duration of the injection
// (see procmem.Tracer.Seize) and released once it is done; only the primary
// thread stays attached. The caller owns the returned Tracer and must Close it
// when finished.
func injectCoreKeepTracer(pid int, sec, nsec int64, mask uint64) (*Handle, *procmem.Tracer, error) {
	tr := procmem.NewTracer()
	if err := tr.Seize(pid); err != nil {
		tr.Close()
		return nil, nil, fmt.Errorf("inject: Seize pid %d: %w", pid, err)
	}
	fail := func(err error) (*Handle, *procmem.Tracer, error) {
		tr.Detach() //nolint:errcheck
		tr.Close()
		return nil, nil, err
	}
	// Read maps while the process is ptrace-stopped — address space is stable.
	info, err := vdso.Locate(pid)
	if err != nil {
		return fail(fmt.Errorf("inject: vdso.Locate: %w", err))
	}
	h, err := injectWithTracer(tr, pid, info, sec, nsec, mask)
	if err != nil {
		return fail(err)
	}
	// The patch is in: let every other thread run again, then arm fork/exec
	// tracing on the primary one and resume it.
	if err := tr.ReleaseThreads(); err != nil {
		return fail(fmt.Errorf("inject: ReleaseThreads: %w", err))
	}
	if err := tr.SetOptions(unix.PTRACE_O_TRACEFORK | unix.PTRACE_O_TRACEVFORK | unix.PTRACE_O_TRACEEXEC); err != nil {
		return fail(fmt.Errorf("inject: SetOptions: %w", err))
	}
	if err := tr.Cont(0); err != nil {
		return fail(fmt.Errorf("inject: Cont parent: %w", err))
	}
	return h, tr, nil
}

func injectFollowChildKeepTracer(cmd *exec.Cmd, sec, nsec int64, mask uint64) (*Handle, *procmem.Tracer, error) {
	tr := procmem.NewTracer()
	pid, err := tr.StartAndFollowChild(cmd)
	if err != nil {
		tr.Close()
		return nil, nil, fmt.Errorf("inject: StartAndFollowChild: %w", err)
	}
	fail := func(err error) (*Handle, *procmem.Tracer, error) {
		tr.Detach() //nolint:errcheck
		tr.Close()
		return nil, nil, err
	}
	info, err := vdso.Locate(pid)
	if err != nil {
		return fail(fmt.Errorf("inject: vdso.Locate: %w", err))
	}
	h, err := injectWithTracer(tr, pid, info, sec, nsec, mask)
	if err != nil {
		return fail(err)
	}
	if err := tr.SetOptions(unix.PTRACE_O_TRACEFORK | unix.PTRACE_O_TRACEVFORK | unix.PTRACE_O_TRACEEXEC); err != nil {
		return fail(fmt.Errorf("inject: SetOptions: %w", err))
	}
	// Resume the parent so it can run and fork.
	if err := tr.Cont(0); err != nil {
		return fail(fmt.Errorf("inject: Cont parent: %w", err))
	}
	return h, tr, nil
}

// ReInjectAtTimeAfterExec re-injects into pid in advancing mode after the
// process has called exec(). pid must be stopped at a PTRACE_EVENT_EXEC stop.
// It performs the full injection sequence on the new address space (vDSO
// locate, trampoline mmap, PokeText, state write). The Tracer's primary tracee
// is temporarily switched to pid and restored to parentPID on return.
func ReInjectAtTimeAfterExec(tr *procmem.Tracer, parentPID, pid int, target time.Time) (*Handle, error) {
	sec, nsec := diffSecNsec(target, time.Now())
	return reInjectAfterExec(tr, parentPID, pid, sec, nsec, trampoline.MaskEnabled)
}

// ReInjectFrozenAfterExec re-injects into pid in frozen mode after exec().
// pid must be stopped at a PTRACE_EVENT_EXEC stop.
func ReInjectFrozenAfterExec(tr *procmem.Tracer, parentPID, pid int, target time.Time) (*Handle, error) {
	sec := target.Unix()
	nsec := int64(target.Nanosecond())
	return reInjectAfterExec(tr, parentPID, pid, sec, nsec, trampoline.MaskFrozen)
}

func reInjectAfterExec(tr *procmem.Tracer, parentPID, pid int, sec, nsec int64, mask uint64) (*Handle, error) {
	tr.SetTracee(pid)
	defer tr.SetTracee(parentPID) // always restore, even on error
	info, err := vdso.Locate(pid)
	if err != nil {
		return nil, fmt.Errorf("inject: vdso.Locate after exec pid %d: %w", pid, err)
	}
	return injectWithTracer(tr, pid, info, sec, nsec, mask)
}

// ChildHandle returns a Handle for a process that forked from parent's target.
// fork() copies the parent's address space, so the child inherits the trampoline
// page and the vDSO JMP patch at the same virtual addresses. No new injection is
// needed; only the PID differs.
func ChildHandle(parent *Handle, childPID int) *Handle {
	return &Handle{
		PID:       childPID,
		StateAddr: parent.StateAddr,
		patched:   parent.patched,
	}
}

// Generation returns the current write-generation counter. It is incremented
// on each SetTime or Freeze call and can be used by callers to confirm a write landed.
func (h *Handle) Generation() uint32 {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.gen
}

// SetTime updates the fake time to target in advancing mode. After this call
// time.Now() in the target process returns target + elapsed_real_time.
// The offset conversion happens immediately before the write to minimise drift.
func (h *Handle) SetTime(target time.Time) error {
	sec, nsec := diffSecNsec(target, time.Now())
	return h.writeState(sec, nsec, trampoline.MaskEnabled)
}

// Freeze sets the target process's clock to target and stops it advancing.
// After this call every invocation of clock_gettime returns exactly target
// until SetTime or Freeze is called again.
func (h *Handle) Freeze(target time.Time) error {
	sec := target.Unix()
	nsec := int64(target.Nanosecond())
	return h.writeState(sec, nsec, trampoline.MaskFrozen)
}

// Uninstall takes the hooks out of the target: each patched vDSO entry point
// goes back to exactly the bytes it had before, and the trampoline page is
// unmapped. After it the process reads the clock through the kernel's own vDSO
// fast path again, which Reset alone never restores (a reset trampoline still
// turns every CLOCK_REALTIME read into a real syscall).
//
// Every thread of the target is stopped while this runs, and any thread that is
// inside the trampoline is stepped out of it first. If the process has already
// exited there is nothing to remove and Uninstall succeeds. The Handle is
// unusable afterwards: SetTime and Freeze return ErrUninstalled.
//
// Only this process is affected. Children that forked while the hooks were
// installed keep their own copy of them until they exit or are uninstalled
// through their own Handle.
func (h *Handle) Uninstall() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.uninstalled {
		return nil
	}
	tr := procmem.NewTracer()
	defer tr.Close()
	if err := tr.Seize(h.PID); err != nil {
		if errors.Is(err, unix.ESRCH) {
			h.uninstalled = true
			return nil
		}
		return fmt.Errorf("inject: uninstall pid %d: %w", h.PID, err)
	}
	defer tr.Detach() //nolint:errcheck // runs before tr.Close: LIFO

	page := h.StateAddr - uintptr(trampoline.StateOffset)
	ranges := []procmem.AddrRange{{Start: page, End: page + pageSize}}
	for _, ph := range h.patched {
		ranges = append(ranges, procmem.AddrRange{Start: ph.addr, End: ph.addr + patchSpan})
	}
	// No thread may be inside the trampoline or the bytes we are about to
	// restore when they change under it.
	if err := tr.StepOutOf(ranges...); err != nil {
		return fmt.Errorf("inject: uninstall pid %d: %w", h.PID, err)
	}
	for _, ph := range h.patched {
		if err := tr.PokeText(ph.addr, ph.orig[:]); err != nil {
			return fmt.Errorf("inject: uninstall pid %d: restore vDSO bytes at 0x%x: %w", h.PID, ph.addr, err)
		}
	}
	// Nothing can reach the page now. Unmapping it is tidiness, not safety, so
	// a failure here leaves a harmless 4 KiB behind rather than failing the call.
	remoteSyscall(tr, h.PID, h.patched[0].addr, syscall.SYS_MUNMAP, uint64(page), uint64(pageSize)) //nolint:errcheck
	h.uninstalled = true
	return nil
}

// injectFollowChild is the FollowChild variant of the core inject path.
func injectFollowChild(cmd *exec.Cmd, sec, nsec int64, mask uint64) (*Handle, error) {
	tr := procmem.NewTracer()
	defer tr.Close()
	// Wait for the child's exec-entry SIGTRAP before reading maps. There is a
	// race between cmd.Start() returning and the child completing execve; reading
	// /proc/<pid>/maps before the exec-stop may catch the address space mid-
	// replacement, where [vdso] is not yet visible.
	pid, err := tr.StartAndFollowChild(cmd)
	if err != nil {
		return nil, fmt.Errorf("inject: StartAndFollowChild: %w", err)
	}
	info, err := vdso.Locate(pid)
	if err != nil {
		tr.Detach() //nolint:errcheck
		return nil, fmt.Errorf("inject: vdso.Locate: %w", err)
	}
	defer tr.Detach() //nolint:errcheck // runs before tr.Close: LIFO
	return injectWithTracer(tr, pid, info, sec, nsec, mask)
}

// injectCore attaches to pid, writes the trampoline with the given state values,
// patches the vDSO, and detaches. InjectAtTime and InjectFrozen are the preferred callers.
// vDSO maps are read after SEIZE+INTERRUPT (process stopped) to avoid a race where
// the process is still mid-execve and the [vdso] mapping is not yet visible.
// Every thread of the target is stopped while the patch goes in (see
// procmem.Tracer.Seize), not just the one whose id is pid.
func injectCore(pid int, sec, nsec int64, mask uint64) (*Handle, error) {
	tr := procmem.NewTracer()
	defer tr.Close()
	if err := tr.Seize(pid); err != nil {
		return nil, fmt.Errorf("inject: Seize pid %d: %w", pid, err)
	}
	defer tr.Detach() //nolint:errcheck // runs before tr.Close: LIFO
	info, err := vdso.Locate(pid)
	if err != nil {
		return nil, fmt.Errorf("inject: vdso.Locate: %w", err)
	}
	return injectWithTracer(tr, pid, info, sec, nsec, mask)
}

// injectWithTracer is the core injection sequence. tr must already be attached to pid.
// mask selects advancing (MaskEnabled) or freeze (MaskFrozen) mode. It patches
// all three wall-clock vDSO entry points -- clock_gettime, gettimeofday, and
// time -- to jump into the corresponding stub in one shared trampoline page,
// so every wall-clock read in the target process observes the same fake
// time regardless of which of the three functions it calls. (PostgreSQL's
// GetCurrentTimestamp and glibc/bash's own wall-clock reads call
// gettimeofday, not clock_gettime.)
// Separated from injectCore so tests can supply a FollowChild-based tracer instead of
// one that required PTRACE_ATTACH.
//
// The target is expected to be stopped on every thread that could run the code
// being patched (Seize does that for a running process; a process stopped at
// its exec has only one thread). Before any byte is overwritten, every stopped
// thread whose instruction pointer is inside a patched range is stepped out of
// it, because a thread resumed in the middle of bytes that changed would decode
// garbage.
//
// Injecting into a process that already carries this package's hooks (a second
// Inject of the same pid, or an agent re-injecting after a restart) reuses the
// existing trampoline page and only rewrites its state, instead of mapping and
// patching a new one on top.
func injectWithTracer(tr *procmem.Tracer, pid int, info *vdso.VDSOInfo, sec, nsec int64, mask uint64) (*Handle, error) {
	// Signals suppressed while driving the tracee are re-sent on the way out,
	// whether or not the injection succeeded.
	defer tr.RequeueSignals() //nolint:errcheck

	hooks := [3]struct {
		name        string
		vdsoAddr    uintptr
		entryOffset int
	}{
		{"clock_gettime", info.ClockGettimeAddr, trampoline.ClockGettimeEntryOffset},
		{"gettimeofday", info.GettimeofdayAddr, trampoline.GettimeofdayEntryOffset},
		{"time", info.TimeAddr, trampoline.TimeEntryOffset},
	}

	if page, ok := existingTrampoline(pid, info); ok {
		return reuseTrampoline(pid, info, page, sec, nsec, mask)
	}

	ranges := make([]procmem.AddrRange, len(hooks))
	for i, hook := range hooks {
		ranges[i] = procmem.AddrRange{Start: hook.vdsoAddr, End: hook.vdsoAddr + patchSpan}
	}
	if err := tr.StepOutOf(ranges...); err != nil {
		return nil, fmt.Errorf("inject: moving threads out of the vDSO patch range: %w", err)
	}

	// Find a free page in the target's address space within ±2 GB of the vDSO
	// entry points. All three hooked functions live within the same few-KB vDSO
	// mapping, so proximity to one guarantees proximity to all. The process is
	// ptrace-stopped so /proc/<pid>/maps is stable. We then use
	// MAP_FIXED_NOREPLACE to guarantee the allocation lands there.
	newPage, err := mapTrampolinePage(tr, pid, info.ClockGettimeAddr)
	if err != nil {
		return nil, err
	}

	// Build the payload with the initial state values pre-written.
	payload := make([]byte, len(trampoline.Payload))
	copy(payload, trampoline.Payload)
	copy(payload[trampoline.StateOffset:], trampoline.EncodeState(sec, nsec, mask, 0))

	// Write the trampoline into the new rwx page.
	if _, err := procmem.WriteMem(pid, newPage, payload); err != nil {
		return nil, fmt.Errorf("inject: write payload: %w", err)
	}

	var patched [3]patchedHook
	rollback := func(upTo int) {
		for _, ph := range patched[:upTo] {
			tr.PokeText(ph.addr, ph.orig[:]) //nolint:errcheck
		}
	}
	for i, hook := range hooks {
		target := newPage + uintptr(hook.entryOffset)

		// Compute the 5-byte JMP rel32 displacement.
		// E9 <disp32> jumps to RIP+disp, where RIP = vdsoAddr+5 (after the JMP itself).
		disp := int64(target) - int64(hook.vdsoAddr+hookLen)
		if disp != int64(int32(disp)) {
			rollback(i)
			return nil, fmt.Errorf("inject: %s: JMP rel32 displacement %d overflows int32 "+
				"(vDSO 0x%x, trampoline 0x%x are too far apart; try hint-based mmap)",
				hook.name, disp, hook.vdsoAddr, target)
		}

		// Save the original 5 bytes at the vDSO entry for future uninstall.
		var orig [hookLen]byte
		if _, err := procmem.ReadMem(pid, hook.vdsoAddr, orig[:]); err != nil {
			rollback(i)
			return nil, fmt.Errorf("inject: %s: save original vDSO bytes: %w", hook.name, err)
		}

		// Overwrite the vDSO entry with our JMP.
		var jmp [hookLen]byte
		jmp[0] = 0xE9
		binary.LittleEndian.PutUint32(jmp[1:], uint32(int32(disp)))
		if err := tr.PokeText(hook.vdsoAddr, jmp[:]); err != nil {
			rollback(i)
			return nil, fmt.Errorf("inject: %s: PokeText JMP: %w", hook.name, err)
		}

		patched[i] = patchedHook{addr: hook.vdsoAddr, orig: orig}
	}

	return &Handle{
		PID:       pid,
		StateAddr: newPage + uintptr(trampoline.StateOffset),
		patched:   patched,
	}, nil
}

// hookTarget returns where the JMP rel32 at addr in pid goes, or ok=false if the
// bytes there are not a JMP rel32.
func hookTarget(pid int, addr uintptr) (target uintptr, ok bool) {
	var b [hookLen]byte
	if _, err := procmem.ReadMem(pid, addr, b[:]); err != nil || b[0] != 0xE9 {
		return 0, false
	}
	disp := int64(int32(binary.LittleEndian.Uint32(b[1:])))
	return uintptr(int64(addr) + hookLen + disp), true
}

// existingTrampoline reports whether all three vDSO entry points of pid already
// jump into one page that holds this build's trampoline code, and if so where
// that page starts. The page is recognised by its contents, not by who put it
// there, so a trampoline installed by an earlier run of the agent counts.
func existingTrampoline(pid int, info *vdso.VDSOInfo) (page uintptr, ok bool) {
	for i, hook := range []struct {
		addr   uintptr
		offset int
	}{
		{info.ClockGettimeAddr, trampoline.ClockGettimeEntryOffset},
		{info.GettimeofdayAddr, trampoline.GettimeofdayEntryOffset},
		{info.TimeAddr, trampoline.TimeEntryOffset},
	} {
		target, isJmp := hookTarget(pid, hook.addr)
		if !isJmp {
			return 0, false
		}
		p := target - uintptr(hook.offset)
		if i == 0 {
			page = p
		} else if p != page {
			return 0, false
		}
	}
	code := make([]byte, trampoline.StateOffset)
	if _, err := procmem.ReadMem(pid, page, code); err != nil {
		return 0, false
	}
	if !bytes.Equal(code, trampoline.Payload[:trampoline.StateOffset]) {
		return 0, false
	}
	return page, true
}

// pristineEntry returns the first hookLen bytes of the vDSO function at
// entryAddr in the target as the kernel ships them. The vDSO image is the same
// in every process on a boot, so this process's own, never-patched copy is the
// reference; that is what lets Uninstall restore the right bytes even for a
// process that was injected by someone else, or injected more than once.
func pristineEntry(info *vdso.VDSOInfo, entryAddr uintptr) ([hookLen]byte, error) {
	var b [hookLen]byte
	own, err := vdso.Locate(os.Getpid())
	if err != nil {
		return b, fmt.Errorf("inject: locating this process's vDSO: %w", err)
	}
	if own.End-own.Start != info.End-info.Start {
		return b, fmt.Errorf("inject: the target's vDSO (%d bytes) differs from this process's (%d bytes)",
			info.End-info.Start, own.End-own.Start)
	}
	if _, err := procmem.ReadMem(os.Getpid(), own.Start+(entryAddr-info.Start), b[:]); err != nil {
		return b, fmt.Errorf("inject: reading this process's vDSO: %w", err)
	}
	return b, nil
}

// reuseTrampoline adopts a trampoline that is already installed in pid: it
// rewrites the state in place and builds a Handle that knows the original vDSO
// bytes, without mapping or patching anything.
func reuseTrampoline(pid int, info *vdso.VDSOInfo, page uintptr, sec, nsec int64, mask uint64) (*Handle, error) {
	h := &Handle{PID: pid, StateAddr: page + uintptr(trampoline.StateOffset)}
	for i, addr := range []uintptr{info.ClockGettimeAddr, info.GettimeofdayAddr, info.TimeAddr} {
		orig, err := pristineEntry(info, addr)
		if err != nil {
			return nil, err
		}
		h.patched[i] = patchedHook{addr: addr, orig: orig}
	}
	if _, err := procmem.WriteMem(pid, h.StateAddr, trampoline.EncodeState(sec, nsec, mask, 0)); err != nil {
		return nil, fmt.Errorf("inject: rewriting the existing trampoline's state: %w", err)
	}
	return h, nil
}

// writeState writes new trampoline state via process_vm_writev without stopping
// the target process. mask controls advancing vs freeze mode.
func (h *Handle) writeState(sec, nsec int64, mask uint64) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.uninstalled {
		return ErrUninstalled
	}
	h.gen++
	encoded := trampoline.EncodeState(sec, nsec, mask, h.gen)
	if _, err := procmem.WriteMem(h.PID, h.StateAddr, encoded); err != nil {
		h.gen-- // revert so the next call retries with the same generation
		return fmt.Errorf("inject: writeState: %w", err)
	}
	return nil
}

// diffSecNsec returns the (sec, nsec) such that base + (sec·s + nsec·ns) ≈ target.
// nsec is always in (-1e9, 1e9); the trampoline normalises it to [0, 1e9).
func diffSecNsec(target, base time.Time) (sec, nsec int64) {
	d := target.Sub(base)
	sec = int64(d / time.Second)
	nsec = int64(d % time.Second)
	return
}

// ---------------------------------------------------------------------------
// findNearbyGap — locate a free page within ±2 GB of the vDSO entry point.
// ---------------------------------------------------------------------------

// findNearbyGap reads /proc/<pid>/maps (while the process is ptrace-stopped,
// so the map is stable) and returns the lowest page-aligned address within
// ±2 GB of near that is not currently mapped.  This address is then passed to
// remoteMmap with MAP_FIXED_NOREPLACE so the trampoline lands in JMP-rel32
// reach of the vDSO.
func findNearbyGap(pid int, near uintptr) (uintptr, error) {
	return findNearbyGapFrom(pid, near, 0)
}

// findNearbyGapFrom is findNearbyGap restricted to addresses at or above
// minAddr, so a caller whose first choice turned out to be taken can ask for
// the next one.
func findNearbyGapFrom(pid int, near, minAddr uintptr) (uintptr, error) {
	const window = uintptr(1 << 31) // 2 GB search radius

	// Compute lo/hi with underflow/overflow protection.
	lo := near - window
	if lo > near {
		lo = pageSize // unsigned underflow — clamp near zero
	}
	if lo < minAddr {
		lo = minAddr
	}
	hi := near + window
	if hi < near {
		hi = ^uintptr(0) - pageSize // unsigned overflow — clamp near top
	}

	f, err := os.Open(fmt.Sprintf("/proc/%d/maps", pid))
	if err != nil {
		return 0, fmt.Errorf("findNearbyGap: open maps: %w", err)
	}
	defer f.Close()

	// Walk the sorted map entries and search each gap between consecutive spans.
	prev := lo
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 1 {
			continue
		}
		dash := strings.IndexByte(fields[0], '-')
		if dash < 0 {
			continue
		}
		s := parseHexAddr(fields[0][:dash])
		e := parseHexAddr(fields[0][dash+1:])

		// Skip spans entirely below our window.
		if e <= lo {
			prev = e
			continue
		}
		// Stop once we're entirely above our window.
		if s >= hi {
			break
		}

		// Gap between prev and s (clipped to [lo, hi]).
		gapStart := prev
		gapEnd := s
		if gapStart < lo {
			gapStart = lo
		}
		if gapEnd > hi {
			gapEnd = hi
		}
		// Align gapStart up to a page boundary.
		gapStart = (gapStart + pageSize - 1) &^ (pageSize - 1)
		if gapStart+pageSize <= gapEnd {
			return gapStart, nil
		}

		if e > prev {
			prev = e
		}
	}
	if err := sc.Err(); err != nil {
		return 0, fmt.Errorf("findNearbyGap: scan: %w", err)
	}

	// Gap after the last span within the window.
	gapStart := prev
	if gapStart < lo {
		gapStart = lo
	}
	gapStart = (gapStart + pageSize - 1) &^ (pageSize - 1)
	if gapStart >= lo && gapStart+pageSize <= hi {
		return gapStart, nil
	}

	return 0, fmt.Errorf("findNearbyGap: no free page within ±2 GB of 0x%x in pid %d", near, pid)
}

func parseHexAddr(s string) uintptr {
	v, _ := strconv.ParseUint(s, 16, 64)
	return uintptr(v)
}

// ---------------------------------------------------------------------------
// remote system calls — run one syscall on the target's own behalf.
// ---------------------------------------------------------------------------

// errAddrInUse reports that the page remoteMmap was asked to map at a fixed
// address was already taken.
var errAddrInUse = errors.New("inject: address is already mapped in the target")

// maxMapAttempts bounds how many candidate pages mapTrampolinePage will try.
const maxMapAttempts = 16

// remoteSyscall makes the tracee run one system call itself and returns the raw
// result: the value, or a negative errno. patchAddr is temporarily overwritten
// with a syscall+int3 stub (the vDSO entry point is a convenient executable
// page), the tracee runs until it hits the int3, and the original bytes and
// every register are then restored.
//
// The tracee's other threads must already be stopped, since the stub is shared
// code. orig_rax is set to -1 for the call so that, if the tracee was stopped in
// the middle of a blocking syscall, the kernel does not try to restart that one
// in place of ours.
func remoteSyscall(tr *procmem.Tracer, pid int, patchAddr uintptr, nr uint64, args ...uint64) (uint64, error) {
	if len(args) > 6 {
		return 0, fmt.Errorf("remoteSyscall: %d arguments, at most 6", len(args))
	}
	origRegs, err := tr.GetRegs()
	if err != nil {
		return 0, fmt.Errorf("remoteSyscall: GetRegs: %w", err)
	}

	origBytes := make([]byte, patchSpan)
	if _, err := procmem.ReadMem(pid, patchAddr, origBytes); err != nil {
		return 0, fmt.Errorf("remoteSyscall: save original bytes: %w", err)
	}

	restored := false
	defer func() {
		if !restored {
			tr.PokeText(patchAddr, origBytes) //nolint:errcheck
			tr.SetRegs(origRegs)              //nolint:errcheck
		}
	}()

	if err := tr.PokeText(patchAddr, []byte{0x0F, 0x05, 0xCC}); err != nil {
		return 0, fmt.Errorf("remoteSyscall: poke stub: %w", err)
	}

	regs := *origRegs
	regs.Rip = uint64(patchAddr)
	regs.Rax = nr
	regs.Orig_rax = ^uint64(0)
	argRegs := []*uint64{&regs.Rdi, &regs.Rsi, &regs.Rdx, &regs.R10, &regs.R8, &regs.R9}
	for i, a := range args {
		*argRegs[i] = a
	}
	if err := tr.SetRegs(&regs); err != nil {
		return 0, fmt.Errorf("remoteSyscall: SetRegs: %w", err)
	}
	if err := tr.ContUntilTrap(); err != nil {
		return 0, fmt.Errorf("remoteSyscall: %w", err)
	}
	post, err := tr.GetRegs()
	if err != nil {
		return 0, fmt.Errorf("remoteSyscall: GetRegs post: %w", err)
	}
	result := post.Rax

	if err := tr.PokeText(patchAddr, origBytes); err != nil {
		return 0, fmt.Errorf("remoteSyscall: restore bytes: %w", err)
	}
	if err := tr.SetRegs(origRegs); err != nil {
		return 0, fmt.Errorf("remoteSyscall: restore regs: %w", err)
	}
	restored = true
	return result, nil
}

// remoteMmap makes the target process call mmap on its own behalf and returns
// the address of the fresh rwx page.  patchAddr (typically ClockGettimeAddr) is
// temporarily overwritten with a syscall+int3 stub.
//
// If fixedAddr is non-zero, MAP_FIXED_NOREPLACE is added to the mmap flags and
// fixedAddr is used as the target address; if that page is already mapped the
// result is errAddrInUse and nothing is disturbed. Pass fixedAddr=0 to let the
// kernel choose freely (used by tests that don't need proximity to the vDSO).
//
// MAP_FIXED, which silently replaces whatever is mapped there, is used in
// exactly one case: when the kernel rejects MAP_FIXED_NOREPLACE with EBADF —
// observed on WSL2 for exec-stopped processes, likely a mandatory seccomp
// restriction that blocks the non-replace variant. That is safe because
// findNearbyGap just confirmed the address is free and every thread of the
// stopped process is stopped, so the address space cannot change. EEXIST is
// never retried that way: it means the page is taken, and replacing it would
// destroy someone's memory.
func remoteMmap(tr *procmem.Tracer, pid int, patchAddr, fixedAddr uintptr) (uintptr, error) {
	prot := uint64(unix.PROT_READ | unix.PROT_WRITE | unix.PROT_EXEC)
	base := uint64(unix.MAP_PRIVATE | unix.MAP_ANONYMOUS)
	call := func(flags uint64) (uint64, error) {
		// fd = -1 is ignored when MAP_ANONYMOUS is set.
		return remoteSyscall(tr, pid, patchAddr, syscall.SYS_MMAP, uint64(fixedAddr), uint64(pageSize), prot, flags, ^uint64(0), 0)
	}

	if fixedAddr == 0 {
		res, err := call(base)
		if err != nil {
			return 0, err
		}
		if int64(res) < 0 {
			return 0, fmt.Errorf("remoteMmap: mmap failed: %w", syscall.Errno(-int64(res)))
		}
		return uintptr(res), nil
	}

	res, err := call(base | uint64(unix.MAP_FIXED_NOREPLACE))
	if err != nil {
		return 0, err
	}
	if int64(res) >= 0 {
		if uintptr(res) != fixedAddr {
			// A kernel too old to know MAP_FIXED_NOREPLACE treats it as a hint and
			// may place the page elsewhere, out of the JMP's reach. Give it back.
			remoteSyscall(tr, pid, patchAddr, syscall.SYS_MUNMAP, res, uint64(pageSize)) //nolint:errcheck
			return 0, errAddrInUse
		}
		return uintptr(res), nil
	}
	switch errno := syscall.Errno(-int64(res)); errno {
	case syscall.EEXIST:
		return 0, errAddrInUse
	case syscall.EBADF:
		res, err = call(base | uint64(unix.MAP_FIXED))
		if err != nil {
			return 0, err
		}
		if int64(res) < 0 {
			return 0, fmt.Errorf("remoteMmap: mmap failed: %w", syscall.Errno(-int64(res)))
		}
		return uintptr(res), nil
	default:
		return 0, fmt.Errorf("remoteMmap: mmap failed: %w", errno)
	}
}

// mapTrampolinePage maps one rwx page in the target within JMP rel32 reach of
// near, trying the next free address if the first choice turns out to be taken.
func mapTrampolinePage(tr *procmem.Tracer, pid int, near uintptr) (uintptr, error) {
	var from uintptr
	for attempt := 0; attempt < maxMapAttempts; attempt++ {
		fixedAddr, err := findNearbyGapFrom(pid, near, from)
		if err != nil {
			return 0, fmt.Errorf("inject: findNearbyGap: %w", err)
		}
		page, err := remoteMmap(tr, pid, near, fixedAddr)
		if err == nil {
			return page, nil
		}
		if !errors.Is(err, errAddrInUse) {
			return 0, fmt.Errorf("inject: remoteMmap: %w", err)
		}
		from = fixedAddr + pageSize
	}
	return 0, fmt.Errorf("inject: no free page near the vDSO after %d attempts", maxMapAttempts)
}
