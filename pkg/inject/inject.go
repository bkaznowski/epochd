//go:build linux

// Package inject ties vDSO discovery, ptrace, and the trampoline payload into
// the public injection API.
package inject

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/bkaznowski/epochd/pkg/procmem"
	"github.com/bkaznowski/epochd/pkg/trampoline"
	"github.com/bkaznowski/epochd/pkg/vdso"
	"golang.org/x/sys/unix"
)

// Handle represents an active time-offset injection in a target process.
// Obtain one via InjectAtTime or InjectFrozen; update with SetTime or Freeze.
type Handle struct {
	PID       int
	StateAddr uintptr // address of the trampoline's mutable state struct in the target
	origBytes [5]byte // original vDSO clock_gettime bytes before our JMP patch
	gen       uint32  // monotonically incremented on every SetOffset/SetTime/Freeze call
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

// InjectAtTimeFollowChild injects the trampoline into a child process that was
// started with SysProcAttr{Ptrace: true}. The child called PTRACE_TRACEME
// before exec and is stopped on its initial SIGTRAP; this function collects
// that stop without issuing PTRACE_ATTACH. No elevated permissions required.
func InjectAtTimeFollowChild(pid int, target time.Time) (*Handle, error) {
	sec, nsec := diffSecNsec(target, time.Now())
	return injectFollowChild(pid, sec, nsec, trampoline.MaskEnabled)
}

// InjectFrozenFollowChild is like InjectAtTimeFollowChild but starts the clock
// in freeze mode so time never advances.
func InjectFrozenFollowChild(pid int, target time.Time) (*Handle, error) {
	sec := target.Unix()
	nsec := int64(target.Nanosecond())
	return injectFollowChild(pid, sec, nsec, trampoline.MaskFrozen)
}

// InjectAtTimeFollowChildKeepTracer is like InjectAtTimeFollowChild but keeps
// ptrace active on the parent with PTRACE_O_TRACEFORK|PTRACE_O_TRACEVFORK set
// and resumes the parent before returning. The caller owns the Tracer and must
// drive the event loop, calling tr.InterruptDetach() when done.
func InjectAtTimeFollowChildKeepTracer(pid int, target time.Time) (*Handle, *procmem.Tracer, error) {
	sec, nsec := diffSecNsec(target, time.Now())
	return injectFollowChildKeepTracer(pid, sec, nsec, trampoline.MaskEnabled)
}

// InjectFrozenFollowChildKeepTracer is like InjectFrozenFollowChild but keeps
// ptrace active on the parent with PTRACE_O_TRACEFORK|PTRACE_O_TRACEVFORK set.
func InjectFrozenFollowChildKeepTracer(pid int, target time.Time) (*Handle, *procmem.Tracer, error) {
	sec := target.Unix()
	nsec := int64(target.Nanosecond())
	return injectFollowChildKeepTracer(pid, sec, nsec, trampoline.MaskFrozen)
}

func injectFollowChildKeepTracer(pid int, sec, nsec int64, mask uint64) (*Handle, *procmem.Tracer, error) {
	tr := procmem.NewTracer()
	if err := tr.FollowChild(pid); err != nil {
		return nil, nil, fmt.Errorf("inject: FollowChild pid %d: %w", pid, err)
	}
	info, err := vdso.Locate(pid)
	if err != nil {
		tr.Detach() //nolint:errcheck
		return nil, nil, fmt.Errorf("inject: vdso.Locate: %w", err)
	}
	h, err := injectWithTracer(tr, pid, info.ClockGettimeAddr, sec, nsec, mask)
	if err != nil {
		tr.Detach() //nolint:errcheck
		return nil, nil, err
	}
	if err := tr.SetOptions(unix.PTRACE_O_TRACEFORK | unix.PTRACE_O_TRACEVFORK | unix.PTRACE_O_TRACEEXEC); err != nil {
		tr.Detach() //nolint:errcheck
		return nil, nil, fmt.Errorf("inject: SetOptions: %w", err)
	}
	// Resume the parent so it can run and fork.
	if err := tr.Cont(0); err != nil {
		tr.Detach() //nolint:errcheck
		return nil, nil, fmt.Errorf("inject: Cont parent: %w", err)
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
	return injectWithTracer(tr, pid, info.ClockGettimeAddr, sec, nsec, mask)
}

// ChildHandle returns a Handle for a process that forked from parent's target.
// fork() copies the parent's address space, so the child inherits the trampoline
// page and the vDSO JMP patch at the same virtual addresses. No new injection is
// needed; only the PID differs.
func ChildHandle(parent *Handle, childPID int) *Handle {
	return &Handle{
		PID:       childPID,
		StateAddr: parent.StateAddr,
		origBytes: parent.origBytes,
	}
}

// Generation returns the current write-generation counter. It is incremented
// on each SetTime or Freeze call and can be used by callers to confirm a write landed.
func (h *Handle) Generation() uint32 { return h.gen }

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

// injectFollowChild is the FollowChild variant of the core inject path.
func injectFollowChild(pid int, sec, nsec int64, mask uint64) (*Handle, error) {
	tr := procmem.NewTracer()
	// Wait for the child's exec-entry SIGTRAP before reading maps. There is a
	// race between cmd.Start() returning and the child completing execve; reading
	// /proc/<pid>/maps before the exec-stop may catch the address space mid-
	// replacement, where [vdso] is not yet visible.
	if err := tr.FollowChild(pid); err != nil {
		return nil, fmt.Errorf("inject: FollowChild pid %d: %w", pid, err)
	}
	info, err := vdso.Locate(pid)
	if err != nil {
		tr.Detach() //nolint:errcheck
		return nil, fmt.Errorf("inject: vdso.Locate: %w", err)
	}
	defer tr.Detach() //nolint:errcheck
	return injectWithTracer(tr, pid, info.ClockGettimeAddr, sec, nsec, mask)
}

// injectCore attaches to pid, writes the trampoline with the given state values,
// patches the vDSO, and detaches. InjectAtTime and InjectFrozen are the preferred callers.
func injectCore(pid int, sec, nsec int64, mask uint64) (*Handle, error) {
	info, err := vdso.Locate(pid)
	if err != nil {
		return nil, fmt.Errorf("inject: vdso.Locate: %w", err)
	}
	tr := procmem.NewTracer()
	if err := tr.Attach(pid); err != nil {
		return nil, fmt.Errorf("inject: Attach pid %d: %w", pid, err)
	}
	defer tr.Detach() //nolint:errcheck
	return injectWithTracer(tr, pid, info.ClockGettimeAddr, sec, nsec, mask)
}

// injectWithTracer is the core injection sequence. tr must already be attached to pid.
// mask selects advancing (MaskEnabled) or freeze (MaskFrozen) mode.
// Separated from injectCore so tests can supply a FollowChild-based tracer instead of
// one that required PTRACE_ATTACH.
func injectWithTracer(tr *procmem.Tracer, pid int, cgtAddr uintptr, sec, nsec int64, mask uint64) (*Handle, error) {
	// Find a free page in the target's address space within ±2 GB of the vDSO
	// entry point.  The process is ptrace-stopped so /proc/<pid>/maps is stable.
	// We then use MAP_FIXED_NOREPLACE to guarantee the allocation lands there.
	fixedAddr, err := findNearbyGap(pid, cgtAddr)
	if err != nil {
		return nil, fmt.Errorf("inject: findNearbyGap: %w", err)
	}

	newPage, err := remoteMmap(tr, pid, cgtAddr, fixedAddr)
	if err != nil {
		return nil, fmt.Errorf("inject: remoteMmap: %w", err)
	}

	// Build the payload with the initial state values pre-written.
	payload := make([]byte, len(trampoline.Payload))
	copy(payload, trampoline.Payload)
	copy(payload[trampoline.StateOffset:], trampoline.EncodeState(sec, nsec, mask, 0))

	// Write the trampoline into the new rwx page.
	if _, err := procmem.WriteMem(pid, newPage, payload); err != nil {
		return nil, fmt.Errorf("inject: write payload: %w", err)
	}

	// Compute the 5-byte JMP rel32 displacement.
	// E9 <disp32> jumps to RIP+disp, where RIP = cgtAddr+5 (after the JMP itself).
	disp := int64(newPage) - int64(cgtAddr+5)
	if disp != int64(int32(disp)) {
		return nil, fmt.Errorf("inject: JMP rel32 displacement %d overflows int32 "+
			"(vDSO 0x%x, trampoline 0x%x are too far apart; try hint-based mmap)", disp, cgtAddr, newPage)
	}

	// Save the original 5 bytes at the vDSO entry for future uninstall.
	var orig [5]byte
	if _, err := procmem.ReadMem(pid, cgtAddr, orig[:]); err != nil {
		return nil, fmt.Errorf("inject: save original vDSO bytes: %w", err)
	}

	// Overwrite the vDSO clock_gettime entry with our JMP.
	var jmp [5]byte
	jmp[0] = 0xE9
	binary.LittleEndian.PutUint32(jmp[1:], uint32(int32(disp)))
	if err := tr.PokeText(cgtAddr, jmp[:]); err != nil {
		return nil, fmt.Errorf("inject: PokeText JMP: %w", err)
	}

	return &Handle{
		PID:       pid,
		StateAddr: newPage + uintptr(trampoline.StateOffset),
		origBytes: orig,
	}, nil
}

// writeState writes new trampoline state via process_vm_writev without stopping
// the target process. mask controls advancing vs freeze mode.
func (h *Handle) writeState(sec, nsec int64, mask uint64) error {
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
	const (
		window   = uintptr(1 << 31) // 2 GB search radius
		pageSize = uintptr(4096)
	)

	// Compute lo/hi with underflow/overflow protection.
	lo := near - window
	if lo > near {
		lo = pageSize // unsigned underflow — clamp near zero
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
// remoteMmap — phase 3 primitive, kept in this file as it's used only here.
// ---------------------------------------------------------------------------

// remoteMmap makes the target process call mmap on its own behalf and returns
// the address of the fresh rwx page.  patchAddr (typically ClockGettimeAddr) is
// temporarily overwritten with a syscall+int3 stub.
//
// If fixedAddr is non-zero, MAP_FIXED_NOREPLACE is added to the mmap flags and
// fixedAddr is used as the target address; the kernel will return EEXIST if
// that page is already mapped.  Pass fixedAddr=0 to let the kernel choose freely
// (used by tests that don't need proximity to the vDSO).
func remoteMmap(tr *procmem.Tracer, pid int, patchAddr, fixedAddr uintptr) (uintptr, error) {
	origRegs, err := tr.GetRegs()
	if err != nil {
		return 0, fmt.Errorf("remoteMmap: GetRegs: %w", err)
	}

	origBytes := make([]byte, 8)
	if _, err := procmem.ReadMem(pid, patchAddr, origBytes); err != nil {
		return 0, fmt.Errorf("remoteMmap: save original bytes: %w", err)
	}

	restored := false
	defer func() {
		if !restored {
			tr.PokeText(patchAddr, origBytes) //nolint:errcheck
			tr.SetRegs(origRegs)              //nolint:errcheck
		}
	}()

	if err := tr.PokeText(patchAddr, []byte{0x0F, 0x05, 0xCC}); err != nil {
		return 0, fmt.Errorf("remoteMmap: poke trampoline: %w", err)
	}

	flags := uint64(unix.MAP_PRIVATE | unix.MAP_ANONYMOUS)
	addr := uint64(0)
	if fixedAddr != 0 {
		flags |= unix.MAP_FIXED_NOREPLACE
		addr = uint64(fixedAddr)
	}

	regs := *origRegs
	regs.Rip = uint64(patchAddr)
	regs.Rax = uint64(syscall.SYS_MMAP)
	regs.Rdi = addr
	regs.Rsi = 4096
	regs.Rdx = uint64(unix.PROT_READ | unix.PROT_WRITE | unix.PROT_EXEC)
	regs.R10 = flags
	regs.R8 = ^uint64(0) // fd = -1
	regs.R9 = 0
	if err := tr.SetRegs(&regs); err != nil {
		return 0, fmt.Errorf("remoteMmap: SetRegs: %w", err)
	}

	if err := tr.Cont(0); err != nil {
		return 0, fmt.Errorf("remoteMmap: Cont: %w", err)
	}
	ws, err := tr.Wait()
	if err != nil {
		return 0, fmt.Errorf("remoteMmap: Wait: %w", err)
	}
	if !ws.Stopped() || ws.StopSignal() != syscall.SIGTRAP {
		return 0, fmt.Errorf("remoteMmap: expected SIGTRAP, got 0x%08x", uint32(ws))
	}

	postRegs, err := tr.GetRegs()
	if err != nil {
		return 0, fmt.Errorf("remoteMmap: GetRegs (post): %w", err)
	}
	result := uintptr(postRegs.Rax)
	if int64(result) < 0 {
		return 0, fmt.Errorf("remoteMmap: mmap failed: %w", syscall.Errno(-int64(result)))
	}

	if err := tr.PokeText(patchAddr, origBytes); err != nil {
		return 0, fmt.Errorf("remoteMmap: restore bytes: %w", err)
	}
	if err := tr.SetRegs(origRegs); err != nil {
		return 0, fmt.Errorf("remoteMmap: restore regs: %w", err)
	}
	restored = true
	return result, nil
}
