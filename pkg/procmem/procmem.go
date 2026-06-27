//go:build linux

// Package procmem provides ptrace-based process memory access primitives.
package procmem

import (
	"errors"
	"fmt"
	"runtime"

	"golang.org/x/sys/unix"
)

// Tracer wraps ptrace for a single tracee. All ptrace calls are dispatched to a
// dedicated goroutine that calls runtime.LockOSThread at startup and never
// releases it, satisfying the Linux kernel requirement that every ptrace call for
// a given tracee come from the same OS thread that issued PTRACE_ATTACH.
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

// FollowChild sets up the Tracer for a child that was started with
// SysProcAttr{Ptrace: true}. The child calls PTRACE_TRACEME before exec and
// then stops on SIGTRAP; this method collects that stop without issuing
// PTRACE_ATTACH. Use this in tests and any context where you own the child
// process. Use Attach for attaching to an already-running process.
func (t *Tracer) FollowChild(pid int) error {
	var err error
	t.run(func() {
		var ws unix.WaitStatus
		if _, e := unix.Wait4(pid, &ws, 0, nil); e != nil {
			err = fmt.Errorf("procmem: wait for ptrace child %d: %w", pid, e)
			return
		}
		if !ws.Stopped() {
			if ws.Exited() {
				err = fmt.Errorf("procmem: ptrace child %d exited (code %d) before stopping; check exec permissions and ptrace_scope", pid, ws.ExitStatus())
			} else {
				err = fmt.Errorf("procmem: ptrace child %d did not stop as expected (status 0x%08x)", pid, uint32(ws))
			}
			return
		}
		t.pid = pid
	})
	return err
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

// WaitAnyNonBlocking checks for a stop event from any traced child without
// blocking. Returns pid=0 if no events are pending, or syscall.ECHILD if
// there are no traced children.
func (t *Tracer) WaitAnyNonBlocking() (int, unix.WaitStatus, error) {
	var ws unix.WaitStatus
	var pid int
	var err error
	t.run(func() {
		p, e := unix.Wait4(-1, &ws, unix.WNOHANG, nil)
		if e != nil {
			err = fmt.Errorf("procmem: wait4(-1, WNOHANG): %w", e)
			return
		}
		pid = p
	})
	return pid, ws, err
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

// InterruptDetach stops the current tracee via PTRACE_INTERRUPT and detaches.
// Safe to call when the tracee is running (not already stopped).
func (t *Tracer) InterruptDetach() error {
	var err error
	t.run(func() {
		unix.PtraceInterrupt(t.pid) //nolint:errcheck // may already be stopped/dead
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
func (t *Tracer) DetachAll(pids []int) error {
	var err error
	t.run(func() {
		for _, pid := range pids {
			unix.PtraceInterrupt(pid) //nolint:errcheck
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
