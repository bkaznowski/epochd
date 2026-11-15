//go:build linux

// Package procmem provides ptrace-based process memory access primitives.
package procmem

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

// ErrTracerClosed is returned by every Tracer method called after Close.
var ErrTracerClosed = errors.New("procmem: tracer is closed")

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
//
// The pinned goroutine, and the OS thread it holds, live until Close is called:
// a Tracer that is simply dropped leaks both. Call Close once the Tracer has
// detached from everything it should release.
type Tracer struct {
	ch   chan func()
	quit chan struct{}
	once sync.Once
	pid  int

	// tgid is the thread-group leader that threads and held belong to. They are
	// only meaningful while pid == tgid: SetTracee can point pid at another
	// process (a child that just exec'd), whose threads are not these.
	tgid    int
	threads []int        // the other seized threads of tgid, when Seize stopped a whole group
	held    []heldSignal // signals pulled off threads while we manipulated them; see RequeueSignals
}

// heldSignal is a signal that was caught in a signal-delivery-stop while the
// Tracer was driving a thread, and suppressed so the thread could finish what
// the Tracer needed. It is re-sent later so the tracee still receives it.
type heldSignal struct {
	tid int
	sig unix.Signal
}

// NewTracer creates a Tracer with its pinned OS thread already running.
func NewTracer() *Tracer {
	t := &Tracer{ch: make(chan func()), quit: make(chan struct{})}
	go t.loop()
	return t
}

func (t *Tracer) loop() {
	runtime.LockOSThread()
	for {
		select {
		case fn := <-t.ch:
			fn()
		case <-t.quit:
			// Returning while locked terminates the thread, which is the point:
			// it must not be handed back to the scheduler still bound to ptrace
			// relationships it no longer serves.
			return
		}
	}
}

// Close stops the Tracer's pinned goroutine and releases its OS thread. It
// does not detach any tracee: do that first (Detach, InterruptDetach,
// DetachAll) for anything that should keep running. After Close every method
// returns ErrTracerClosed. Close is safe to call more than once.
func (t *Tracer) Close() {
	t.once.Do(func() { close(t.quit) })
}

// run sends fn to the pinned OS thread and blocks until it returns. It reports
// false, without running fn, if the Tracer has been closed.
func (t *Tracer) run(fn func()) bool {
	done := make(chan struct{})
	select {
	case t.ch <- func() { fn(); close(done) }:
		<-done
		return true
	case <-t.quit:
		return false
	}
}

// do runs fn on the pinned OS thread and returns its error, or ErrTracerClosed.
func (t *Tracer) do(fn func() error) error {
	var err error
	if !t.run(func() { err = fn() }) {
		return ErrTracerClosed
	}
	return err
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
	t.tgid = pid
	t.threads = nil
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
	return t.do(func() error { return t.followChildLocked(pid) })
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
	err := t.do(func() error {
		if startErr := cmd.Start(); startErr != nil {
			return startErr
		}
		pid = cmd.Process.Pid
		return t.followChildLocked(pid)
	})
	return pid, err
}

// Attach calls PTRACE_ATTACH and waits for the tracee to stop.
func (t *Tracer) Attach(pid int) error {
	return t.do(func() error {
		if e := unix.PtraceAttach(pid); e != nil {
			return fmt.Errorf("procmem: PTRACE_ATTACH pid %d: %w", pid, e)
		}
		var ws unix.WaitStatus
		if _, e := unix.Wait4(pid, &ws, 0, nil); e != nil {
			return fmt.Errorf("procmem: wait after PTRACE_ATTACH pid %d: %w", pid, e)
		}
		t.pid = pid
		t.tgid = pid
		t.threads = nil
		return nil
	})
}

// seizeStop attaches to one thread via PTRACE_SEIZE, stops it with
// PTRACE_INTERRUPT and waits for the stop.
func seizeStop(tid int) error {
	if e := unix.PtraceSeize(tid); e != nil {
		return fmt.Errorf("procmem: PTRACE_SEIZE pid %d: %w", tid, e)
	}
	if e := unix.PtraceInterrupt(tid); e != nil {
		unix.PtraceDetach(tid) //nolint:errcheck
		return fmt.Errorf("procmem: PTRACE_INTERRUPT after SEIZE pid %d: %w", tid, e)
	}
	var ws unix.WaitStatus
	if _, e := unix.Wait4(tid, &ws, unix.WALL, nil); e != nil {
		unix.PtraceDetach(tid) //nolint:errcheck
		return fmt.Errorf("procmem: wait after SEIZE+INTERRUPT pid %d: %w", tid, e)
	}
	return nil
}

// listThreads returns the thread ids of the thread group led by pid.
func listThreads(pid int) ([]int, error) {
	entries, err := os.ReadDir("/proc/" + strconv.Itoa(pid) + "/task")
	if err != nil {
		return nil, err
	}
	tids := make([]int, 0, len(entries))
	for _, e := range entries {
		if tid, err := strconv.Atoi(e.Name()); err == nil {
			tids = append(tids, tid)
		}
	}
	return tids, nil
}

// Seize attaches to pid via PTRACE_SEIZE and immediately stops it with
// PTRACE_INTERRUPT. Unlike Attach (PTRACE_ATTACH), SEIZE does not deliver
// SIGSTOP to the tracee's thread group, avoiding group-stop races during
// subsequent ptrace operations. PTRACE_INTERRUPT works reliably on
// SEIZE-attached processes, which makes InterruptDetach safe to call even
// while the tracee is running.
//
// A ptrace attachment covers one thread, so Seize stops the whole thread group:
// pid itself, which stays the Tracer's primary tracee, and every other thread
// in /proc/<pid>/task. It rescans until a pass finds no thread it has not
// already stopped, because a running thread can clone before it is stopped
// itself. Without this, the threads left running can execute code the caller
// is about to rewrite. Release them with Detach (or ReleaseThreads to keep
// only the leader attached).
func (t *Tracer) Seize(pid int) error {
	return t.do(func() error {
		if err := seizeStop(pid); err != nil {
			return err
		}
		t.pid = pid
		t.tgid = pid
		t.threads = nil
		t.held = nil

		seized := map[int]bool{pid: true}
		for {
			tids, err := listThreads(pid)
			if err != nil {
				t.releaseThreadsLocked()
				unix.PtraceDetach(pid) //nolint:errcheck
				return fmt.Errorf("procmem: listing threads of %d: %w", pid, err)
			}
			fresh := false
			for _, tid := range tids {
				if seized[tid] {
					continue
				}
				fresh = true
				seized[tid] = true
				if err := seizeStop(tid); err != nil {
					if errors.Is(err, unix.ESRCH) {
						continue // the thread exited between the listing and the seize
					}
					t.releaseThreadsLocked()
					unix.PtraceDetach(pid) //nolint:errcheck
					return err
				}
				t.threads = append(t.threads, tid)
			}
			if !fresh {
				return nil
			}
		}
	})
}

// releaseThreadsLocked detaches every seized thread other than the leader,
// which resumes them. Must run on the pinned thread.
func (t *Tracer) releaseThreadsLocked() {
	for _, tid := range t.threads {
		unix.PtraceDetach(tid) //nolint:errcheck
	}
	t.threads = nil
}

// ReleaseThreads detaches every thread Seize stopped except the primary
// tracee, which resumes them. Use it once the whole group no longer needs to
// stay stopped but the leader still has to remain attached.
func (t *Tracer) ReleaseThreads() error {
	return t.do(func() error {
		t.releaseThreadsLocked()
		return nil
	})
}

// Threads returns the thread ids the Tracer currently has stopped for its
// primary tracee's thread group, the primary tracee first.
func (t *Tracer) Threads() []int {
	var out []int
	t.run(func() { out = t.stoppedLocked() })
	return out
}

// stoppedLocked lists the stopped threads that belong to the primary tracee.
func (t *Tracer) stoppedLocked() []int {
	out := []int{t.pid}
	if t.pid == t.tgid {
		out = append(out, t.threads...)
	}
	return out
}

// Detach calls PTRACE_DETACH, allowing the tracee to resume. Threads stopped by
// Seize resume too, and any signals held back meanwhile are re-sent first.
func (t *Tracer) Detach() error {
	return t.do(func() error {
		t.requeueLocked()
		t.releaseThreadsLocked()
		if e := unix.PtraceDetach(t.pid); e != nil {
			return fmt.Errorf("procmem: PTRACE_DETACH pid %d: %w", t.pid, e)
		}
		t.pid = 0
		return nil
	})
}

// GetRegs reads the tracee's general-purpose registers.
func (t *Tracer) GetRegs() (*unix.PtraceRegs, error) {
	var regs unix.PtraceRegs
	err := t.do(func() error {
		if e := unix.PtraceGetRegs(t.pid, &regs); e != nil {
			return fmt.Errorf("procmem: PTRACE_GETREGS: %w", e)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &regs, nil
}

// GetRegsPID reads the general-purpose registers of an arbitrary stopped
// thread, such as one of the threads Seize stopped, without changing the
// primary tracee.
func (t *Tracer) GetRegsPID(tid int) (*unix.PtraceRegs, error) {
	var regs unix.PtraceRegs
	err := t.do(func() error {
		if e := unix.PtraceGetRegs(tid, &regs); e != nil {
			return fmt.Errorf("procmem: PTRACE_GETREGS pid %d: %w", tid, e)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &regs, nil
}

// SetRegs writes the tracee's general-purpose registers.
func (t *Tracer) SetRegs(r *unix.PtraceRegs) error {
	return t.do(func() error {
		if e := unix.PtraceSetRegs(t.pid, r); e != nil {
			return fmt.Errorf("procmem: PTRACE_SETREGS: %w", e)
		}
		return nil
	})
}

// SingleStep resumes the tracee for exactly one instruction, then stops it again.
func (t *Tracer) SingleStep() error {
	return t.do(func() error {
		if e := unix.PtraceSingleStep(t.pid); e != nil {
			return fmt.Errorf("procmem: PTRACE_SINGLESTEP: %w", e)
		}
		return nil
	})
}

// Cont resumes the tracee. sig is forwarded as a signal (0 for none).
func (t *Tracer) Cont(sig int) error {
	return t.do(func() error {
		if e := unix.PtraceCont(t.pid, sig); e != nil {
			return fmt.Errorf("procmem: PTRACE_CONT: %w", e)
		}
		return nil
	})
}

// Wait blocks until the tracee stops or exits and returns its wait status.
func (t *Tracer) Wait() (unix.WaitStatus, error) {
	var ws unix.WaitStatus
	err := t.do(func() error {
		if _, e := unix.Wait4(t.pid, &ws, 0, nil); e != nil {
			return fmt.Errorf("procmem: wait4 pid %d: %w", t.pid, e)
		}
		return nil
	})
	return ws, err
}

// errStopTimeout is returned when a thread driven by the Tracer does not stop
// when it should, typically because the instruction it was resumed over is a
// blocking system call.
var errStopTimeout = errors.New("procmem: thread did not stop in time")

const (
	// trapWait bounds how long ContUntilTrap lets injected code run.
	trapWait = 10 * time.Second
	// stepWait bounds a single PTRACE_SINGLESTEP.
	stepWait = 2 * time.Second
	// interruptGrace is how long to wait for a thread to stop after it has been
	// PTRACE_INTERRUPTed for overrunning its time.
	interruptGrace = 2 * time.Second
)

// waitStopBounded waits for tid's next wait status, giving up after d. A tracee
// that overruns is interrupted, so that it ends up stopped rather than running
// on with nobody watching it, and errStopTimeout is returned.
func waitStopBounded(tid int, d time.Duration) (unix.WaitStatus, error) {
	var ws unix.WaitStatus
	deadline := time.Now().Add(d)
	for {
		p, err := unix.Wait4(tid, &ws, unix.WNOHANG|unix.WALL, nil)
		if err != nil {
			return ws, err
		}
		if p == tid {
			return ws, nil
		}
		if time.Now().After(deadline) {
			break
		}
		time.Sleep(200 * time.Microsecond)
	}
	unix.PtraceInterrupt(tid) //nolint:errcheck
	grace := time.Now().Add(interruptGrace)
	for time.Now().Before(grace) {
		if p, err := unix.Wait4(tid, &ws, unix.WNOHANG|unix.WALL, nil); err == nil && p == tid {
			break
		}
		time.Sleep(time.Millisecond)
	}
	return ws, errStopTimeout
}

// maxTrapSignals bounds how many unrelated signals ContUntilTrap will absorb
// while waiting for the SIGTRAP it is after, so a tracee stuck re-raising a
// fault cannot spin it forever.
const maxTrapSignals = 64

// ContUntilTrap resumes the primary tracee and waits for it to stop on a
// SIGTRAP, the way an int3 the caller planted reports itself. Any other signal
// that arrives first (the Go runtime's SIGURG preemption, SIGCHLD, a timer) is
// suppressed so the tracee can reach the trap, and held to be re-sent by
// RequeueSignals or Detach so it is not lost. Faults that would only repeat
// (SIGSEGV, SIGBUS, SIGILL, SIGFPE) and the tracee dying are errors.
func (t *Tracer) ContUntilTrap() error {
	return t.do(func() error {
		for i := 0; i < maxTrapSignals; i++ {
			if e := unix.PtraceCont(t.pid, 0); e != nil {
				return fmt.Errorf("procmem: PTRACE_CONT: %w", e)
			}
			ws, e := waitStopBounded(t.pid, trapWait)
			if e != nil {
				return fmt.Errorf("procmem: waiting for tracee %d to reach the trap: %w", t.pid, e)
			}
			switch {
			case ws.Exited() || ws.Signaled():
				return fmt.Errorf("procmem: tracee %d died while running injected code (status 0x%08x)", t.pid, uint32(ws))
			case !ws.Stopped():
				return fmt.Errorf("procmem: unexpected wait status 0x%08x for pid %d", uint32(ws), t.pid)
			}
			sig := ws.StopSignal()
			if event := int(ws) >> 16; event != 0 {
				continue // a ptrace event stop (e.g. PTRACE_EVENT_STOP): just keep going
			}
			switch sig {
			case unix.SIGTRAP:
				return nil
			case unix.SIGSEGV, unix.SIGBUS, unix.SIGILL, unix.SIGFPE:
				return fmt.Errorf("procmem: tracee %d took %v while running injected code", t.pid, sig)
			default:
				t.held = append(t.held, heldSignal{t.pid, sig})
			}
		}
		return fmt.Errorf("procmem: tracee %d took more than %d unrelated signals before reaching the trap", t.pid, maxTrapSignals)
	})
}

// AddrRange is the half-open address interval [Start, End).
type AddrRange struct{ Start, End uintptr }

func inAny(ranges []AddrRange, addr uintptr) bool {
	for _, r := range ranges {
		if addr >= r.Start && addr < r.End {
			return true
		}
	}
	return false
}

// maxStepsOutOf bounds how far StepOutOf will walk one thread.
const maxStepsOutOf = 256

// StepOutOf single-steps every stopped thread whose instruction pointer lies
// inside one of ranges until it is outside them all. Call it before rewriting
// code those threads might be in the middle of: a thread resumed with its
// instruction pointer inside bytes that changed under it would decode garbage.
// Signals that surface during the steps are held (see RequeueSignals).
func (t *Tracer) StepOutOf(ranges ...AddrRange) error {
	return t.do(func() error {
		for _, tid := range t.stoppedLocked() {
			if err := t.stepOutOfLocked(tid, ranges); err != nil {
				return err
			}
		}
		return nil
	})
}

func (t *Tracer) stepOutOfLocked(tid int, ranges []AddrRange) error {
	for i := 0; i < maxStepsOutOf; i++ {
		var regs unix.PtraceRegs
		if e := unix.PtraceGetRegs(tid, &regs); e != nil {
			if errors.Is(e, unix.ESRCH) && tid != t.pid {
				return nil // a secondary thread exited: nothing left to protect
			}
			return fmt.Errorf("procmem: PTRACE_GETREGS thread %d: %w", tid, e)
		}
		if !inAny(ranges, uintptr(regs.Rip)) {
			return nil
		}
		if e := unix.PtraceSingleStep(tid); e != nil {
			return fmt.Errorf("procmem: PTRACE_SINGLESTEP thread %d: %w", tid, e)
		}
		ws, e := waitStopBounded(tid, stepWait)
		if e != nil {
			return fmt.Errorf("procmem: stepping thread %d out of the patch range: %w", tid, e)
		}
		switch {
		case ws.Exited() || ws.Signaled():
			if tid == t.pid {
				return fmt.Errorf("procmem: tracee %d died while being stepped out of a patch range", tid)
			}
			return nil
		case !ws.Stopped():
			return fmt.Errorf("procmem: unexpected wait status 0x%08x for thread %d", uint32(ws), tid)
		}
		if int(ws)>>16 == 0 && ws.StopSignal() != unix.SIGTRAP {
			// A signal-delivery-stop: the instruction has not run yet. Suppress the
			// signal (the next step resumes with none) and hold it for later.
			t.held = append(t.held, heldSignal{tid, ws.StopSignal()})
		}
	}
	return fmt.Errorf("procmem: thread %d did not leave the patch range after %d steps", tid, maxStepsOutOf)
}

// RequeueSignals re-sends, with tgkill, every signal that ContUntilTrap or
// StepOutOf suppressed so the tracee still receives it once it resumes.
func (t *Tracer) RequeueSignals() error {
	return t.do(func() error {
		t.requeueLocked()
		return nil
	})
}

func (t *Tracer) requeueLocked() {
	for _, h := range t.held {
		unix.Tgkill(t.tgid, h.tid, h.sig) //nolint:errcheck
	}
	t.held = nil
}

// PokeText writes buf into the tracee's address space using PTRACE_POKETEXT.
// Unlike WriteMem, this is permitted on read-only-but-executable pages (e.g. the
// vDSO), which is how debuggers set breakpoints and how we patch clock_gettime.
// Requires an active ptrace attachment.
func (t *Tracer) PokeText(addr uintptr, buf []byte) error {
	return t.do(func() error {
		if _, e := unix.PtracePokeText(t.pid, addr, buf); e != nil {
			return fmt.Errorf("procmem: PTRACE_POKETEXT at 0x%x: %w", addr, e)
		}
		return nil
	})
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
	return t.do(func() error {
		if e := unix.PtraceSetOptions(t.pid, opts); e != nil {
			return fmt.Errorf("procmem: PTRACE_SETOPTIONS pid %d: %w", t.pid, e)
		}
		return nil
	})
}

// SetOptionsPID sets PTRACE_O_* options on an arbitrary ptrace-stopped PID
// without changing the Tracer's primary tracee (t.pid).
func (t *Tracer) SetOptionsPID(pid, opts int) error {
	return t.do(func() error {
		if e := unix.PtraceSetOptions(pid, opts); e != nil {
			return fmt.Errorf("procmem: PTRACE_SETOPTIONS pid %d: %w", pid, e)
		}
		return nil
	})
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
//
// Also passes WUNTRACED, WCONTINUED, and WALL/__WALL: heavily multi-threaded
// tracees (e.g. Redpanda's Seastar reactor, one thread per shard) rely on
// wait4 reporting stop/continue transitions for clone-created threads, not
// just SIGCHLD-signaling children — the default filtering otherwise applied
// to non-ptrace waits.
func (t *Tracer) WaitAnyNonBlocking() (int, unix.WaitStatus, error) {
	var ws unix.WaitStatus
	var pid int
	err := t.do(func() error {
		p, e := unix.Wait4(-1, &ws, unix.WNOHANG|unix.WNOTHREAD|unix.WUNTRACED|unix.WCONTINUED|unix.WALL, nil)
		if e != nil {
			return fmt.Errorf("procmem: wait4(-1, WNOHANG|WNOTHREAD): %w", e)
		}
		pid = p
		return nil
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
//
// Also passes WUNTRACED, WCONTINUED, and WALL/__WALL — see WaitAnyNonBlocking
// for why: pid may be a clone-created thread of a heavily multi-threaded
// tracee, not just a SIGCHLD-signaling child.
func (t *Tracer) WaitPIDNonBlocking(pid int) (unix.WaitStatus, bool, error) {
	var ws unix.WaitStatus
	var ok bool
	err := t.do(func() error {
		p, e := unix.Wait4(pid, &ws, unix.WNOHANG|unix.WUNTRACED|unix.WCONTINUED|unix.WALL, nil)
		if e != nil {
			return fmt.Errorf("procmem: wait4(%d, WNOHANG): %w", pid, e)
		}
		ok = p == pid
		return nil
	})
	return ws, ok, err
}

// GetEventMsgPID retrieves the ptrace event message from an arbitrary
// ptrace-stopped PID. After PTRACE_EVENT_FORK or PTRACE_EVENT_VFORK,
// this returns the newly created child's PID.
func (t *Tracer) GetEventMsgPID(pid int) (uint, error) {
	var msg uint
	err := t.do(func() error {
		m, e := unix.PtraceGetEventMsg(pid)
		if e != nil {
			return fmt.Errorf("procmem: PTRACE_GETEVENTMSG pid %d: %w", pid, e)
		}
		msg = m
		return nil
	})
	return msg, err
}

// ContPID resumes an arbitrary ptrace-stopped PID on the pinned OS thread.
// sig is forwarded to the resumed process (0 for no signal).
func (t *Tracer) ContPID(pid, sig int) error {
	return t.do(func() error {
		if e := unix.PtraceCont(pid, sig); e != nil {
			return fmt.Errorf("procmem: PTRACE_CONT pid %d: %w", pid, e)
		}
		return nil
	})
}

// needsSigstopFallback reports whether a failed PTRACE_INTERRUPT means the
// tracee was attached with PTRACE_ATTACH or PTRACE_TRACEME rather than
// PTRACE_SEIZE (EIO), the one case where SIGSTOP is the right way to stop it.
// Any other error, ESRCH above all, means the process is gone or is not ours;
// signalling that pid could stop an unrelated process that reused it.
func needsSigstopFallback(err error) bool {
	return errors.Is(err, unix.EIO)
}

// stopWait bounds how long InterruptDetach and DetachAll wait for a tracee to
// report the stop they asked for, so a tracee that never reports it cannot hang
// the caller. If it elapses they proceed to the detach regardless.
const stopWait = 2 * time.Second

// waitForStop reaps the next wait status of pid, giving up after stopWait.
func waitForStop(pid int) {
	deadline := time.Now().Add(stopWait)
	for time.Now().Before(deadline) {
		var ws unix.WaitStatus
		p, err := unix.Wait4(pid, &ws, unix.WNOHANG|unix.WALL, nil)
		if err != nil || p == pid {
			return
		}
		time.Sleep(time.Millisecond)
	}
}

// interruptStop asks pid to stop and waits for it. It reports false when the
// process is gone, in which case there is nothing to detach.
func interruptStop(pid int) bool {
	if e := unix.PtraceInterrupt(pid); e != nil {
		switch {
		case needsSigstopFallback(e):
			// PTRACE_INTERRUPT only works for PTRACE_SEIZE-attached tracees.
			// Fall back to SIGSTOP for PTRACE_ATTACH / PTRACE_TRACEME.
			unix.Kill(pid, unix.SIGSTOP) //nolint:errcheck
		case isNoProcess(e):
			return false
		}
	}
	waitForStop(pid)
	return true
}

// InterruptDetach stops the current tracee and detaches. For PTRACE_SEIZE-based
// tracees PTRACE_INTERRUPT is used; for PTRACE_ATTACH or PTRACE_TRACEME tracees
// (where PTRACE_INTERRUPT returns EIO) we fall back to SIGSTOP.
func (t *Tracer) InterruptDetach() error {
	return t.do(func() error {
		var err error
		if interruptStop(t.pid) {
			if e := unix.PtraceDetach(t.pid); e != nil && !isNoProcess(e) {
				err = fmt.Errorf("procmem: PTRACE_DETACH pid %d: %w", t.pid, e)
			}
		}
		t.pid = 0
		return err
	})
}

// DetachAll interrupts and detaches from each PID in the list on the pinned
// OS thread. Errors for processes that no longer exist are silently ignored.
// Uses the same PTRACE_INTERRUPT → SIGSTOP fallback as InterruptDetach.
func (t *Tracer) DetachAll(pids []int) error {
	return t.do(func() error {
		var err error
		for _, pid := range pids {
			if !interruptStop(pid) {
				continue
			}
			if e := unix.PtraceDetach(pid); e != nil && !isNoProcess(e) {
				err = errors.Join(err, fmt.Errorf("procmem: PTRACE_DETACH pid %d: %w", pid, e))
			}
		}
		return err
	})
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
