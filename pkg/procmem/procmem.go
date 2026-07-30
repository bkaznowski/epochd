//go:build linux

// Package procmem provides ptrace-based process memory access primitives.
package procmem

import (
	"errors"
	"fmt"
	"os/exec"
	"runtime"

	"golang.org/x/sys/unix"
)

// Tracer wraps ptrace for a single tracee. All ptrace calls are dispatched to a
// dedicated goroutine that calls runtime.LockOSThread at startup and never
// releases it, satisfying the Linux kernel requirement that every ptrace call for
// a given tracee come from the same OS thread that issued PTRACE_ATTACH.
//
// This requirement is exact-thread, not just same-thread-group: PTRACE_TRACEME
// records the tracee's tracer as the specific task that was its parent at the
// moment TRACEME ran, i.e. whichever OS thread performed the fork. A wait4 for
// that tracee's initial stop succeeds from any thread in the process (TRACEME
// leaves the tracee's real parent and ptrace-parent identical, so ordinary
// thread-group-wide wait eligibility applies) — but a subsequent ptrace
// request such as PTRACE_GETREGS does not: it is checked against that exact
// task, and fails with ESRCH from any other thread, even one in the same
// process. So the fork itself must happen on this Tracer's pinned thread too;
// see StartAndFollowChild.
type Tracer struct {
	ch  chan func()
	pid int
}

// NewTracer creates a Tracer with its pinned OS thread already running.
func NewTracer() *Tracer {
	t := &Tracer{ch: make(chan func())}
	go t.loop()
	return t
}

func (t *Tracer) loop() {
	runtime.LockOSThread()
	for fn := range t.ch {
		fn()
	}
}

// run sends fn to the pinned OS thread and blocks until it returns.
func (t *Tracer) run(fn func()) {
	done := make(chan struct{})
	t.ch <- func() { fn(); close(done) }
	<-done
}

// followChildLocked waits for pid's initial post-execve ptrace stop and
// records it as this Tracer's tracee. Must be called from the pinned thread
// (i.e. from within run) — see StartAndFollowChild and FollowChild.
func (t *Tracer) followChildLocked(pid int) error {
	var ws unix.WaitStatus
	if _, e := unix.Wait4(pid, &ws, 0, nil); e != nil {
		return fmt.Errorf("procmem: wait for ptrace child %d: %w", pid, e)
	}
	if !ws.Stopped() {
		if ws.Exited() {
			return fmt.Errorf("procmem: ptrace child %d exited (code %d) before stopping; check exec permissions and ptrace_scope", pid, ws.ExitStatus())
		}
		return fmt.Errorf("procmem: ptrace child %d did not stop as expected (status 0x%08x)", pid, uint32(ws))
	}
	t.pid = pid
	return nil
}

// FollowChild sets up the Tracer for a child that was started with
// SysProcAttr{Ptrace: true}. The child calls PTRACE_TRACEME before exec and
// then stops on SIGTRAP; this method collects that stop without issuing
// PTRACE_ATTACH. Use this in tests and any context where you own the child
// process. Use Attach for attaching to an already-running process.
//
// The caller must have started pid from this same Tracer's pinned thread —
// via StartAndFollowChild — not via a plain cmd.Start() on some other
// goroutine. See the Tracer doc comment for why: a wait4 for the initial stop
// works from any thread, but this Tracer's later ptrace requests (GetRegs,
// PokeText, ...) require the fork to have happened on its own pinned thread.
func (t *Tracer) FollowChild(pid int) error {
	var err error
	t.run(func() { err = t.followChildLocked(pid) })
	return err
}

// StartAndFollowChild starts cmd (which must already have SysProcAttr.Ptrace
// set) and waits for its initial post-execve ptrace stop, all on this
// Tracer's own pinned OS thread. The caller must not call cmd.Start() itself.
//
// This is the correct way to start a traced child: PTRACE_TRACEME ties the
// tracee's tracer to the exact thread that performed the fork, and every
// subsequent ptrace request this Tracer issues (GetRegs, PokeText, Cont, ...)
// must come from that same thread. Calling cmd.Start() separately, on
// whatever thread the caller's goroutine happens to be scheduled on, and only
// later creating a Tracer to follow the result, binds the fork to a different
// thread than the one that will drive those later requests — a mismatch that
// intermittently (not always) makes them fail with ESRCH, since it depends on
// whether the Go scheduler happens to reuse the same underlying OS thread.
func (t *Tracer) StartAndFollowChild(cmd *exec.Cmd) (int, error) {
	var pid int
	var err error
	t.run(func() {
		if startErr := cmd.Start(); startErr != nil {
			err = startErr
			return
		}
		pid = cmd.Process.Pid
		err = t.followChildLocked(pid)
	})
	return pid, err
}

// Attach calls PTRACE_ATTACH and waits for the tracee to stop.
func (t *Tracer) Attach(pid int) error {
	var err error
	t.run(func() {
		if e := unix.PtraceAttach(pid); e != nil {
			err = fmt.Errorf("procmem: PTRACE_ATTACH pid %d: %w", pid, e)
			return
		}
		var ws unix.WaitStatus
		if _, e := unix.Wait4(pid, &ws, 0, nil); e != nil {
			err = fmt.Errorf("procmem: wait after PTRACE_ATTACH pid %d: %w", pid, e)
			return
		}
		t.pid = pid
	})
	return err
}

// Seize attaches to pid via PTRACE_SEIZE and immediately stops it with
// PTRACE_INTERRUPT. Unlike Attach (PTRACE_ATTACH), SEIZE does not deliver
// SIGSTOP to the tracee's thread group, avoiding group-stop races during
// subsequent ptrace operations. PTRACE_INTERRUPT works reliably on
// SEIZE-attached processes, which makes InterruptDetach safe to call even
// while the tracee is running.
func (t *Tracer) Seize(pid int) error {
	var err error
	t.run(func() {
		if e := unix.PtraceSeize(pid); e != nil {
			err = fmt.Errorf("procmem: PTRACE_SEIZE pid %d: %w", pid, e)
			return
		}
		if e := unix.PtraceInterrupt(pid); e != nil {
			unix.PtraceDetach(pid) //nolint:errcheck
			err = fmt.Errorf("procmem: PTRACE_INTERRUPT after SEIZE pid %d: %w", pid, e)
			return
		}
		var ws unix.WaitStatus
		if _, e := unix.Wait4(pid, &ws, 0, nil); e != nil {
			unix.PtraceDetach(pid) //nolint:errcheck
			err = fmt.Errorf("procmem: wait after SEIZE+INTERRUPT pid %d: %w", pid, e)
			return
		}
		t.pid = pid
	})
	return err
}

// Detach calls PTRACE_DETACH, allowing the tracee to resume.
func (t *Tracer) Detach() error {
	var err error
	t.run(func() {
		if e := unix.PtraceDetach(t.pid); e != nil {
			err = fmt.Errorf("procmem: PTRACE_DETACH pid %d: %w", t.pid, e)
			return
		}
		t.pid = 0
	})
	return err
}

// GetRegs reads the tracee's general-purpose registers.
func (t *Tracer) GetRegs() (*unix.PtraceRegs, error) {
	var regs unix.PtraceRegs
	var err error
	t.run(func() {
		if e := unix.PtraceGetRegs(t.pid, &regs); e != nil {
			err = fmt.Errorf("procmem: PTRACE_GETREGS: %w", e)
		}
	})
	if err != nil {
		return nil, err
	}
	return &regs, nil
}

// SetRegs writes the tracee's general-purpose registers.
func (t *Tracer) SetRegs(r *unix.PtraceRegs) error {
	var err error
	t.run(func() {
		if e := unix.PtraceSetRegs(t.pid, r); e != nil {
			err = fmt.Errorf("procmem: PTRACE_SETREGS: %w", e)
		}
	})
	return err
}

// SingleStep resumes the tracee for exactly one instruction, then stops it again.
func (t *Tracer) SingleStep() error {
	var err error
	t.run(func() {
		if e := unix.PtraceSingleStep(t.pid); e != nil {
			err = fmt.Errorf("procmem: PTRACE_SINGLESTEP: %w", e)
		}
	})
	return err
}

// Cont resumes the tracee. sig is forwarded as a signal (0 for none).
func (t *Tracer) Cont(sig int) error {
	var err error
	t.run(func() {
		if e := unix.PtraceCont(t.pid, sig); e != nil {
			err = fmt.Errorf("procmem: PTRACE_CONT: %w", e)
		}
	})
	return err
}

// Wait blocks until the tracee stops or exits and returns its wait status.
func (t *Tracer) Wait() (unix.WaitStatus, error) {
	var ws unix.WaitStatus
	var err error
	t.run(func() {
		if _, e := unix.Wait4(t.pid, &ws, 0, nil); e != nil {
			err = fmt.Errorf("procmem: wait4 pid %d: %w", t.pid, e)
		}
	})
	return ws, err
}

// PokeText writes buf into the tracee's address space using PTRACE_POKETEXT.
// Unlike WriteMem, this is permitted on read-only-but-executable pages (e.g. the
// vDSO), which is how debuggers set breakpoints and how we patch clock_gettime.
// Requires an active ptrace attachment.
func (t *Tracer) PokeText(addr uintptr, buf []byte) error {
	var err error
	t.run(func() {
		if _, e := unix.PtracePokeText(t.pid, addr, buf); e != nil {
			err = fmt.Errorf("procmem: PTRACE_POKETEXT at 0x%x: %w", addr, e)
		}
	})
	return err
}

// SetTracee changes the PID that Tracer methods (GetRegs, SetRegs, PokeText,
// Cont, Wait) operate on. Used when temporarily injecting into a process other
// than the primary tracee (e.g. a child that just exec'd). The caller is
// responsible for restoring the original PID when done.
func (t *Tracer) SetTracee(pid int) {
	t.run(func() { t.pid = pid })
}

// SetOptions sets PTRACE_O_* options on the current tracee.
// Must be called while the tracee is ptrace-stopped.
func (t *Tracer) SetOptions(opts int) error {
	var err error
	t.run(func() {
		if e := unix.PtraceSetOptions(t.pid, opts); e != nil {
			err = fmt.Errorf("procmem: PTRACE_SETOPTIONS pid %d: %w", t.pid, e)
		}
	})
	return err
}

// SetOptionsPID sets PTRACE_O_* options on an arbitrary ptrace-stopped PID
// without changing the Tracer's primary tracee (t.pid).
func (t *Tracer) SetOptionsPID(pid, opts int) error {
	var err error
	t.run(func() {
		if e := unix.PtraceSetOptions(pid, opts); e != nil {
			err = fmt.Errorf("procmem: PTRACE_SETOPTIONS pid %d: %w", pid, e)
		}
	})
	return err
}

// WaitAnyNonBlocking checks for a stop event from any traced child without
// blocking. Returns pid=0 if no events are pending, or syscall.ECHILD if
// there are no traced children.
//
// Uses WNOTHREAD to restrict reaping to tracees of this Tracer's own pinned
// OS thread. Without it, wait4(-1, ...) reaps children across every thread in
// the calling thread group by default (Linux only skips other threads when
// __WNOTHREAD is set) — so when a process hosts multiple independent Tracers
// concurrently (e.g. one ChildTracker per process added to a faketime
// Session), one Tracer's wait4 can steal a ptrace-stop that actually belongs
// to a different Tracer's tracee. The stealing Tracer doesn't recognize the
// pid, its PTRACE_CONT fallback fails silently with ESRCH (ptrace ops are
// still correctly restricted to the registered tracer thread), and the
// rightful owner never observes the event again — leaving that tracee
// permanently ptrace-stopped. This was observed as sessions with many
// concurrently tracked process trees (e.g. Postgres, Redpanda, and a mix of
// Go/Python services all under one Session) silently hanging.
func (t *Tracer) WaitAnyNonBlocking() (int, unix.WaitStatus, error) {
	var ws unix.WaitStatus
	var pid int
	var err error
	t.run(func() {
		p, e := unix.Wait4(-1, &ws, unix.WNOHANG|unix.WNOTHREAD, nil)
		if e != nil {
			err = fmt.Errorf("procmem: wait4(-1, WNOHANG|WNOTHREAD): %w", e)
			return
		}
		pid = p
	})
	return pid, ws, err
}

// WaitPIDNonBlocking checks for a stop or exit event on one specific,
// already-known pid without blocking. ok is false if no event is pending for
// pid right now.
//
// Unlike WaitAnyNonBlocking, this can never observe an event belonging to a
// different pid, so it carries none of that method's cross-Tracer hazard: it
// is safe to call concurrently from many independent Tracers (e.g. one
// ChildTracker per process in a faketime Session using WithTracking, all
// forked by the same caller and so all real children of the same thread) even
// though WNOTHREAD only restricts wait4(-1, ...) to children of the calling
// thread specifically, not children of the calling thread's whole process —
// an exact-pid wait has no such ambiguity to exploit, because the kernel can
// only ever report status for the pid actually asked for.
func (t *Tracer) WaitPIDNonBlocking(pid int) (unix.WaitStatus, bool, error) {
	var ws unix.WaitStatus
	var ok bool
	var err error
	t.run(func() {
		p, e := unix.Wait4(pid, &ws, unix.WNOHANG, nil)
		if e != nil {
			err = fmt.Errorf("procmem: wait4(%d, WNOHANG): %w", pid, e)
			return
		}
		ok = p == pid
	})
	return ws, ok, err
}

// GetEventMsgPID retrieves the ptrace event message from an arbitrary
// ptrace-stopped PID. After PTRACE_EVENT_FORK or PTRACE_EVENT_VFORK,
// this returns the newly created child's PID.
func (t *Tracer) GetEventMsgPID(pid int) (uint, error) {
	var msg uint
	var err error
	t.run(func() {
		m, e := unix.PtraceGetEventMsg(pid)
		if e != nil {
			err = fmt.Errorf("procmem: PTRACE_GETEVENTMSG pid %d: %w", pid, e)
			return
		}
		msg = m
	})
	return msg, err
}

// ContPID resumes an arbitrary ptrace-stopped PID on the pinned OS thread.
// sig is forwarded to the resumed process (0 for no signal).
func (t *Tracer) ContPID(pid, sig int) error {
	var err error
	t.run(func() {
		if e := unix.PtraceCont(pid, sig); e != nil {
			err = fmt.Errorf("procmem: PTRACE_CONT pid %d: %w", pid, e)
		}
	})
	return err
}

// InterruptDetach stops the current tracee and detaches. For PTRACE_SEIZE-based
// tracees PTRACE_INTERRUPT is used; for PTRACE_ATTACH or PTRACE_TRACEME tracees
// (where PTRACE_INTERRUPT returns EIO) we fall back to SIGSTOP.
func (t *Tracer) InterruptDetach() error {
	var err error
	t.run(func() {
		if e := unix.PtraceInterrupt(t.pid); e != nil {
			// PTRACE_INTERRUPT only works for PTRACE_SEIZE-attached tracees.
			// Fall back to SIGSTOP for PTRACE_ATTACH / PTRACE_TRACEME.
			unix.Kill(t.pid, unix.SIGSTOP) //nolint:errcheck
		}
		var ws unix.WaitStatus
		unix.Wait4(t.pid, &ws, 0, nil) //nolint:errcheck
		if e := unix.PtraceDetach(t.pid); e != nil && !isNoProcess(e) {
			err = fmt.Errorf("procmem: PTRACE_DETACH pid %d: %w", t.pid, e)
		}
		t.pid = 0
	})
	return err
}

// DetachAll interrupts and detaches from each PID in the list on the pinned
// OS thread. Errors for processes that no longer exist are silently ignored.
// Uses the same PTRACE_INTERRUPT → SIGSTOP fallback as InterruptDetach.
func (t *Tracer) DetachAll(pids []int) error {
	var err error
	t.run(func() {
		for _, pid := range pids {
			if e := unix.PtraceInterrupt(pid); e != nil {
				unix.Kill(pid, unix.SIGSTOP) //nolint:errcheck
			}
			var ws unix.WaitStatus
			unix.Wait4(pid, &ws, 0, nil) //nolint:errcheck
			if e := unix.PtraceDetach(pid); e != nil && !isNoProcess(e) {
				err = errors.Join(err, fmt.Errorf("procmem: PTRACE_DETACH pid %d: %w", pid, e))
			}
		}
	})
	return err
}

func isNoProcess(err error) bool {
	return errors.Is(err, unix.ESRCH)
}

// ReadMem reads len(buf) bytes from addr in pid using process_vm_readv.
// Does not require an active ptrace stop; needs CAP_SYS_PTRACE or an existing
// ptrace relationship with the target.
func ReadMem(pid int, addr uintptr, buf []byte) (int, error) {
	if len(buf) == 0 {
		return 0, nil
	}
	localIov := []unix.Iovec{{Base: &buf[0], Len: uint64(len(buf))}}
	remoteIov := []unix.RemoteIovec{{Base: addr, Len: len(buf)}}
	n, err := unix.ProcessVMReadv(pid, localIov, remoteIov, 0)
	if err != nil {
		return n, fmt.Errorf("procmem: process_vm_readv pid %d at 0x%x: %w", pid, addr, err)
	}
	return n, nil
}

// WriteMem writes buf into addr in pid using process_vm_writev.
// Fails on write-protected pages (e.g. r-xp mappings); use PokeText for those.
func WriteMem(pid int, addr uintptr, buf []byte) (int, error) {
	if len(buf) == 0 {
		return 0, nil
	}
	localIov := []unix.Iovec{{Base: &buf[0], Len: uint64(len(buf))}}
	remoteIov := []unix.RemoteIovec{{Base: addr, Len: len(buf)}}
	n, err := unix.ProcessVMWritev(pid, localIov, remoteIov, 0)
	if err != nil {
		return n, fmt.Errorf("procmem: process_vm_writev pid %d at 0x%x: %w", pid, addr, err)
	}
	return n, nil
}
