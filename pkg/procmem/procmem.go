//go:build linux

// Package procmem provides ptrace-based process memory access primitives.
package procmem

import (
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
