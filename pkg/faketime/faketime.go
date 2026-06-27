//go:build linux

// Package faketime injects fake time into local (non-Kubernetes) processes.
// It wraps pkg/inject for use in Go tests and CLI tooling without requiring a
// running Kubernetes cluster or agent daemon.
package faketime

import (
	"errors"
	"fmt"
	"os/exec"
	"sync"
	"syscall"
	"time"

	"github.com/bkaznowski/epochd/pkg/inject"
	"github.com/bkaznowski/epochd/pkg/procmem"
	"golang.org/x/sys/unix"
)

// Handle holds an active time injection for a single process.
type Handle struct {
	h  *inject.Handle
	mu sync.Mutex
	// For advancing mode: fake_time = time.Now() + offset.
	// For frozen mode: fake_time = frozenAt (constant).
	offset   time.Duration
	frozenAt time.Time
	frozen   bool
}

func newAdvancingHandle(h *inject.Handle, target time.Time) *Handle {
	return &Handle{h: h, offset: time.Until(target)}
}

func newFrozenHandle(h *inject.Handle, target time.Time) *Handle {
	return &Handle{h: h, frozenAt: target, frozen: true}
}

// effectiveTime returns the fake time the process currently sees.
func (h *Handle) effectiveTime() time.Time {
	if h.frozen {
		return h.frozenAt
	}
	return time.Now().Add(h.offset)
}

// Start starts cmd with fake time injected from the moment the process begins.
// It sets cmd.SysProcAttr to enable ptrace, calls cmd.Start(), then uses the
// FollowChild path to inject before the process executes any user code.
// No elevated permissions required. The caller must not call cmd.Start() before
// calling Start.
func Start(cmd *exec.Cmd, target time.Time) (*Handle, error) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Ptrace = true
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("faketime: Start: %w", err)
	}
	h, err := inject.InjectAtTimeFollowChild(cmd.Process.Pid, target)
	if err != nil {
		cmd.Process.Kill() //nolint:errcheck
		cmd.Wait()         //nolint:errcheck
		return nil, fmt.Errorf("faketime: inject: %w", err)
	}
	return newAdvancingHandle(h, target), nil
}

// StartFrozen starts cmd with the clock frozen at target. Unlike Start, the
// process sees the same timestamp on every call to clock_gettime until
// SetTime or Freeze is called.
func StartFrozen(cmd *exec.Cmd, target time.Time) (*Handle, error) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Ptrace = true
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("faketime: StartFrozen: %w", err)
	}
	h, err := inject.InjectFrozenFollowChild(cmd.Process.Pid, target)
	if err != nil {
		cmd.Process.Kill() //nolint:errcheck
		cmd.Wait()         //nolint:errcheck
		return nil, fmt.Errorf("faketime: inject frozen: %w", err)
	}
	return newFrozenHandle(h, target), nil
}

// Attach injects fake time into an already-running process.
// Requires CAP_SYS_PTRACE and /proc/sys/kernel/yama/ptrace_scope ≤ 1.
func Attach(pid int, target time.Time) (*Handle, error) {
	h, err := inject.InjectAtTime(pid, target)
	if err != nil {
		return nil, fmt.Errorf("faketime: Attach pid %d: %w", pid, err)
	}
	return newAdvancingHandle(h, target), nil
}

// AttachFrozen injects a frozen clock into an already-running process.
// Requires CAP_SYS_PTRACE and /proc/sys/kernel/yama/ptrace_scope <= 1.
func AttachFrozen(pid int, target time.Time) (*Handle, error) {
	h, err := inject.InjectFrozen(pid, target)
	if err != nil {
		return nil, fmt.Errorf("faketime: AttachFrozen pid %d: %w", pid, err)
	}
	return newFrozenHandle(h, target), nil
}

// SetTime updates the fake time without stopping the process (process_vm_writev only).
func (h *Handle) SetTime(target time.Time) error {
	if err := h.h.SetTime(target); err != nil {
		return err
	}
	h.mu.Lock()
	h.offset = time.Until(target)
	h.frozenAt = time.Time{}
	h.frozen = false
	h.mu.Unlock()
	return nil
}

// Freeze freezes the process's clock at target. Every subsequent call to
// clock_gettime in the target process returns exactly target.
func (h *Handle) Freeze(target time.Time) error {
	if err := h.h.Freeze(target); err != nil {
		return err
	}
	h.mu.Lock()
	h.frozenAt = target
	h.offset = 0
	h.frozen = true
	h.mu.Unlock()
	return nil
}

// Advance shifts the current fake time by d (may be negative to rewind).
// For advancing handles the stored offset grows by d; for frozen handles the
// frozen point shifts by d. The clock mode (advancing or frozen) is preserved.
func (h *Handle) Advance(d time.Duration) error {
	h.mu.Lock()
	frozen := h.frozen
	target := h.effectiveTime().Add(d)
	h.mu.Unlock()
	if frozen {
		return h.Freeze(target)
	}
	return h.SetTime(target)
}

// Reset snaps the process back to the real clock. Equivalent to SetTime(time.Now()).
func (h *Handle) Reset() error {
	return h.SetTime(time.Now())
}

// ---------------------------------------------------------------------------
// Session
// ---------------------------------------------------------------------------

// Session manages fake time for a group of processes that share the same
// target clock. Processes are added via Start or Attach; all handles are
// updated concurrently on SetTime to minimise the inter-process race window.
type Session struct {
	mu      sync.Mutex
	handles []*Handle
	cmds    []*exec.Cmd // commands added via Start; waited on by testing helpers
	// For advancing mode: effective target = time.Now() + offset.
	// For frozen mode: effective target = frozenAt (constant).
	offset   time.Duration
	frozenAt time.Time
	frozen   bool
}

// NewSession creates an empty session with the given initial target time.
func NewSession(target time.Time) *Session {
	return &Session{offset: time.Until(target)}
}

// effectiveTarget returns the current effective fake time for new injections.
func (s *Session) effectiveTarget() time.Time {
	if s.frozen {
		return s.frozenAt
	}
	return time.Now().Add(s.offset)
}

// Start starts cmd with fake time and adds the resulting handle to the session.
func (s *Session) Start(cmd *exec.Cmd) error {
	s.mu.Lock()
	target := s.effectiveTarget()
	frozen := s.frozen
	s.mu.Unlock()

	var h *Handle
	var err error
	if frozen {
		h, err = StartFrozen(cmd, target)
	} else {
		h, err = Start(cmd, target)
	}
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.handles = append(s.handles, h)
	s.cmds = append(s.cmds, cmd)
	s.mu.Unlock()
	return nil
}

// Attach attaches to an already-running process and adds it to the session.
func (s *Session) Attach(pid int) error {
	s.mu.Lock()
	target := s.effectiveTarget()
	frozen := s.frozen
	s.mu.Unlock()

	var h *Handle
	var err error
	if frozen {
		h, err = AttachFrozen(pid, target)
	} else {
		h, err = Attach(pid, target)
	}
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.handles = append(s.handles, h)
	s.mu.Unlock()
	return nil
}

// SetTime updates the fake time for all processes in the session concurrently.
// Per-process errors are collected and returned joined; a partial failure leaves
// successful handles at the new target.
func (s *Session) SetTime(target time.Time) error {
	return s.applyAll(func(h *Handle) error { return h.SetTime(target) }, func() {
		s.offset = time.Until(target)
		s.frozenAt = time.Time{}
		s.frozen = false
	})
}

// Freeze freezes the clock at target for all processes in the session concurrently.
func (s *Session) Freeze(target time.Time) error {
	return s.applyAll(func(h *Handle) error { return h.Freeze(target) }, func() {
		s.frozenAt = target
		s.offset = 0
		s.frozen = true
	})
}

// Advance shifts the session's clock by d (may be negative to rewind).
// For advancing sessions the offset grows by d; for frozen sessions the frozen
// point shifts by d. The clock mode is preserved. All processes are updated
// concurrently.
func (s *Session) Advance(d time.Duration) error {
	s.mu.Lock()
	frozen := s.frozen
	target := s.effectiveTarget().Add(d)
	s.mu.Unlock()
	if frozen {
		return s.Freeze(target)
	}
	return s.SetTime(target)
}

// Reset snaps all processes back to the real clock.
func (s *Session) Reset() error {
	return s.SetTime(time.Now())
}

// Len returns the number of handles currently in the session.
func (s *Session) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.handles)
}

// ---------------------------------------------------------------------------
// ChildTracker
// ---------------------------------------------------------------------------

// ChildTracker watches a process for fork and vfork events and automatically
// injects fake time into each child process as it is created. Because fork
// copies the parent's address space, the child inherits the trampoline page and
// vDSO patch; no new injection is needed — only a Handle pointing to the
// child's copy of the state struct is created.
//
// Obtain a ChildTracker via StartWithTracking or StartFrozenWithTracking.
type ChildTracker struct {
	// Handle is the parent process's fake-time handle.
	Handle *Handle

	mu          sync.Mutex
	tracer      *procmem.Tracer
	parentPID   int
	children    map[int]*Handle // childPID → Handle
	pendingStop map[int]bool    // children waiting for their initial ptrace stop
	done        chan struct{}
	wg          sync.WaitGroup
	loopErr     error
}

// Children returns Handles for all child processes currently tracked.
func (c *ChildTracker) Children() []*Handle {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]*Handle, 0, len(c.children))
	for _, h := range c.children {
		out = append(out, h)
	}
	return out
}

// Err returns the first error encountered by the background watch loop, if any.
func (c *ChildTracker) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.loopErr
}

// Close stops the fork watcher, resets all tracked children to the real clock,
// and detaches ptrace from the parent and all children. The parent process
// continues running after Close returns.
func (c *ChildTracker) Close() error {
	close(c.done)
	c.wg.Wait()
	c.mu.Lock()
	err := c.loopErr
	c.mu.Unlock()
	return err
}

// StartWithTracking starts cmd with advancing fake time and returns a
// ChildTracker that automatically injects fake time into any processes spawned
// via fork or vfork. No elevated permissions required.
func StartWithTracking(cmd *exec.Cmd, target time.Time) (*ChildTracker, error) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Ptrace = true
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("faketime: StartWithTracking: %w", err)
	}
	ih, tr, err := inject.InjectAtTimeFollowChildKeepTracer(cmd.Process.Pid, target)
	if err != nil {
		cmd.Process.Kill() //nolint:errcheck
		cmd.Wait()         //nolint:errcheck
		return nil, fmt.Errorf("faketime: StartWithTracking: %w", err)
	}
	return newChildTracker(newAdvancingHandle(ih, target), tr, cmd.Process.Pid), nil
}

// StartFrozenWithTracking starts cmd with the clock frozen at target and
// returns a ChildTracker that automatically injects fake time into any
// processes spawned via fork or vfork.
func StartFrozenWithTracking(cmd *exec.Cmd, target time.Time) (*ChildTracker, error) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Ptrace = true
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("faketime: StartFrozenWithTracking: %w", err)
	}
	ih, tr, err := inject.InjectFrozenFollowChildKeepTracer(cmd.Process.Pid, target)
	if err != nil {
		cmd.Process.Kill() //nolint:errcheck
		cmd.Wait()         //nolint:errcheck
		return nil, fmt.Errorf("faketime: StartFrozenWithTracking: %w", err)
	}
	return newChildTracker(newFrozenHandle(ih, target), tr, cmd.Process.Pid), nil
}

func newChildTracker(h *Handle, tr *procmem.Tracer, parentPID int) *ChildTracker {
	ct := &ChildTracker{
		Handle:      h,
		tracer:      tr,
		parentPID:   parentPID,
		children:    make(map[int]*Handle),
		pendingStop: make(map[int]bool),
		done:        make(chan struct{}),
	}
	ct.wg.Add(1)
	go ct.watchLoop()
	return ct
}

func (c *ChildTracker) watchLoop() {
	defer c.wg.Done()
	defer c.cleanup()

	for {
		select {
		case <-c.done:
			return
		default:
		}

		pid, ws, err := c.tracer.WaitAnyNonBlocking()
		if err != nil {
			// ECHILD means no more traced children — normal exit when parent dies.
			if !errors.Is(err, syscall.ECHILD) {
				c.mu.Lock()
				c.loopErr = err
				c.mu.Unlock()
			}
			return
		}
		if pid == 0 {
			// No events pending — avoid busy-spinning.
			time.Sleep(5 * time.Millisecond)
			continue
		}

		c.handleEvent(pid, ws)
	}
}

func (c *ChildTracker) handleEvent(pid int, ws unix.WaitStatus) {
	if ws.Exited() || ws.Signaled() {
		c.mu.Lock()
		delete(c.children, pid)
		delete(c.pendingStop, pid)
		c.mu.Unlock()
		if pid == c.parentPID {
			// Parent exited — no more fork events to expect.
			c.mu.Lock()
			if c.loopErr == nil {
				c.loopErr = fmt.Errorf("faketime: parent process %d exited unexpectedly", pid)
			}
			c.mu.Unlock()
		}
		return
	}

	if !ws.Stopped() {
		return
	}

	// Check for fork, vfork, or exec ptrace events.
	ptraceEvent := (int(ws) >> 16) & 0xFF
	isSIGTRAP := ws.StopSignal() == syscall.SIGTRAP
	isFork := isSIGTRAP && (ptraceEvent == unix.PTRACE_EVENT_FORK || ptraceEvent == unix.PTRACE_EVENT_VFORK)
	isExec := isSIGTRAP && ptraceEvent == unix.PTRACE_EVENT_EXEC

	if isFork {
		childPIDMsg, err := c.tracer.GetEventMsgPID(pid)
		if err != nil {
			c.tracer.ContPID(pid, 0) //nolint:errcheck
			return
		}
		childPID := int(childPIDMsg) //nolint:gosec — PID fits in int on all supported arches

		// The child inherits the trampoline via fork — build a Handle for it.
		c.Handle.mu.Lock()
		childIH := inject.ChildHandle(c.Handle.h, childPID)
		var childH *Handle
		if c.Handle.frozen {
			childH = newFrozenHandle(childIH, c.Handle.frozenAt)
		} else {
			childH = newAdvancingHandle(childIH, c.Handle.effectiveTime())
		}
		c.Handle.mu.Unlock()

		c.mu.Lock()
		c.children[childPID] = childH
		c.pendingStop[childPID] = true
		c.mu.Unlock()

		c.tracer.ContPID(pid, 0) //nolint:errcheck
		return
	}

	if isExec {
		c.handleExec(pid)
		return
	}

	// Child's initial ptrace stop (auto-attached by PTRACE_O_TRACEFORK).
	c.mu.Lock()
	isPending := c.pendingStop[pid]
	if isPending {
		delete(c.pendingStop, pid)
	}
	c.mu.Unlock()

	if isPending {
		// Set options on the child so its own fork/exec events are tracked.
		c.tracer.SetOptionsPID(pid, //nolint:errcheck
			unix.PTRACE_O_TRACEFORK|unix.PTRACE_O_TRACEVFORK|unix.PTRACE_O_TRACEEXEC)
		c.tracer.ContPID(pid, 0) //nolint:errcheck
		return
	}

	// Forward non-ptrace signals; suppress SIGTRAP (it's a ptrace event, not a real signal).
	sig := 0
	if ws.StopSignal() != syscall.SIGTRAP {
		sig = int(ws.StopSignal())
	}
	c.tracer.ContPID(pid, sig) //nolint:errcheck
}

// handleExec re-injects fake time into pid after it called exec(). pid must be
// stopped at a PTRACE_EVENT_EXEC ptrace-stop. Handles both self-exec
// (pid == c.parentPID, e.g. PEX bootstrap) and fork+exec (pid is a child).
func (c *ChildTracker) handleExec(pid int) {
	// Identify which handle belongs to this PID and what time it should show.
	var target time.Time
	var frozen bool

	if pid == c.parentPID {
		c.Handle.mu.Lock()
		target = c.Handle.effectiveTime()
		frozen = c.Handle.frozen
		c.Handle.mu.Unlock()
	} else {
		c.mu.Lock()
		childH, ok := c.children[pid]
		c.mu.Unlock()
		if !ok {
			// Unknown PID — not tracked; just resume.
			c.tracer.ContPID(pid, 0) //nolint:errcheck
			return
		}
		childH.mu.Lock()
		target = childH.effectiveTime()
		frozen = childH.frozen
		childH.mu.Unlock()
	}

	// Re-inject into the fresh address space. The Tracer temporarily switches
	// its primary tracee to pid and restores it to c.parentPID on return.
	var newIH *inject.Handle
	var err error
	if frozen {
		newIH, err = inject.ReInjectFrozenAfterExec(c.tracer, c.parentPID, pid, target)
	} else {
		newIH, err = inject.ReInjectAtTimeAfterExec(c.tracer, c.parentPID, pid, target)
	}
	if err != nil {
		c.mu.Lock()
		if c.loopErr == nil {
			c.loopErr = fmt.Errorf("faketime: re-inject after exec pid %d: %w", pid, err)
		}
		c.mu.Unlock()
		c.tracer.ContPID(pid, 0) //nolint:errcheck
		return
	}

	// Swap the inject.Handle so future SetTime/Freeze calls write to the new
	// trampoline page in the exec'd address space.
	if pid == c.parentPID {
		c.Handle.mu.Lock()
		c.Handle.h = newIH
		c.Handle.mu.Unlock()
	} else {
		c.mu.Lock()
		childH := c.children[pid]
		c.mu.Unlock()
		childH.mu.Lock()
		childH.h = newIH
		childH.mu.Unlock()
	}

	c.tracer.ContPID(pid, 0) //nolint:errcheck
}

func (c *ChildTracker) cleanup() {
	// Reset all child handles to the real clock (best effort).
	for _, h := range c.Children() {
		h.Reset() //nolint:errcheck
	}

	// Detach from children, then the parent.
	c.mu.Lock()
	childPIDs := make([]int, 0, len(c.children))
	for pid := range c.children {
		childPIDs = append(childPIDs, pid)
	}
	c.mu.Unlock()

	c.tracer.DetachAll(childPIDs) //nolint:errcheck
	c.tracer.InterruptDetach()    //nolint:errcheck
}

// applyAll calls fn on every handle concurrently and then, if no errors
// occurred, calls updateState under the session lock to commit the new state.
// Per-handle errors are joined; a partial failure leaves the session state
// unchanged so the next call retries with the old values.
func (s *Session) applyAll(fn func(*Handle) error, updateState func()) error {
	s.mu.Lock()
	handles := make([]*Handle, len(s.handles))
	copy(handles, s.handles)
	s.mu.Unlock()

	errs := make([]error, len(handles))
	var wg sync.WaitGroup
	for i, h := range handles {
		wg.Add(1)
		go func(i int, h *Handle) {
			defer wg.Done()
			errs[i] = fn(h)
		}(i, h)
	}
	wg.Wait()

	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("faketime: applyAll: %w", err)
	}
	s.mu.Lock()
	updateState()
	s.mu.Unlock()
	return nil
}
