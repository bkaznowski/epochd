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
// Caller must hold h.mu.
func (h *Handle) effectiveTime() time.Time {
	if h.frozen {
		return h.frozenAt
	}
	return time.Now().Add(h.offset)
}

// PID returns the OS process ID of the process this Handle controls.
func (h *Handle) PID() int { return h.h.PID }

// IsFrozen reports whether the handle is currently in frozen mode.
func (h *Handle) IsFrozen() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.frozen
}

// IsAlive reports whether the process is still running. It sends signal 0,
// which checks for the process's existence without delivering a signal.
func (h *Handle) IsAlive() bool { return syscall.Kill(h.h.PID, 0) == nil }

// EffectiveTime returns the fake time the process currently sees. For advancing
// mode this is time.Now() plus the stored offset; for frozen mode it is the
// pinned instant.
func (h *Handle) EffectiveTime() time.Time {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.effectiveTime()
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
// Session options
// ---------------------------------------------------------------------------

// SessionOption configures a Session.
type SessionOption func(*Session)

// WithTracking enables automatic fake-time injection into any processes spawned
// via fork, vfork, or exec while the session is active. When this option is
// used, Session.Start internally calls StartWithTracking and Session.Close must
// be called when the session is no longer needed to stop the watch goroutine
// and detach ptrace.
func WithTracking() SessionOption {
	return func(s *Session) { s.tracking = true }
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
	tracking bool
	trackers []*ChildTracker
}

// NewSession creates an empty session with the given initial target time.
// Pass WithTracking() to enable automatic injection into forked/exec'd children.
func NewSession(target time.Time, opts ...SessionOption) *Session {
	s := &Session{offset: time.Until(target)}
	for _, o := range opts {
		o(s)
	}
	return s
}

// effectiveTarget returns the current effective fake time for new injections.
func (s *Session) effectiveTarget() time.Time {
	if s.frozen {
		return s.frozenAt
	}
	return time.Now().Add(s.offset)
}

// Start starts cmd with fake time and adds the resulting handle to the session.
// When the session was created with WithTracking, fork and exec events are
// automatically tracked and injected.
func (s *Session) Start(cmd *exec.Cmd) error {
	s.mu.Lock()
	target := s.effectiveTarget()
	frozen := s.frozen
	tracking := s.tracking
	s.mu.Unlock()

	if tracking {
		var ct *ChildTracker
		var err error
		if frozen {
			ct, err = StartFrozenWithTracking(cmd, target)
		} else {
			ct, err = StartWithTracking(cmd, target)
		}
		if err != nil {
			return err
		}
		s.mu.Lock()
		s.handles = append(s.handles, ct.Handle)
		s.trackers = append(s.trackers, ct)
		s.cmds = append(s.cmds, cmd)
		s.mu.Unlock()
		return nil
	}

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
// When the session was created with WithTracking, fork and exec events from the
// attached process are automatically tracked and injected.
// Requires CAP_SYS_PTRACE and ptrace_scope <= 1 when WithTracking is active.
func (s *Session) Attach(pid int) error {
	s.mu.Lock()
	target := s.effectiveTarget()
	frozen := s.frozen
	tracking := s.tracking
	s.mu.Unlock()

	if tracking {
		var ct *ChildTracker
		var err error
		if frozen {
			ct, err = AttachFrozenWithTracking(pid, target)
		} else {
			ct, err = AttachWithTracking(pid, target)
		}
		if err != nil {
			return err
		}
		s.mu.Lock()
		s.handles = append(s.handles, ct.Handle)
		s.trackers = append(s.trackers, ct)
		s.mu.Unlock()
		return nil
	}

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

// IsFrozen reports whether the session is currently in frozen mode.
func (s *Session) IsFrozen() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.frozen
}

// Len returns the number of handles currently in the session.
func (s *Session) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.handles)
}

// Close stops all child tracker goroutines, resets tracked children to the
// real clock, and detaches ptrace from every tracked process. It is only
// required when the session was created with WithTracking; for non-tracking
// sessions it is a no-op. The parent processes continue running after Close —
// call Reset before Close if you want to snap them back to the real clock.
func (s *Session) Close() error {
	s.mu.Lock()
	trackers := make([]*ChildTracker, len(s.trackers))
	copy(trackers, s.trackers)
	s.mu.Unlock()

	if len(trackers) == 0 {
		return nil
	}
	errs := make([]error, len(trackers))
	var wg sync.WaitGroup
	for i, ct := range trackers {
		wg.Add(1)
		go func(i int, ct *ChildTracker) {
			defer wg.Done()
			errs[i] = ct.Close()
		}(i, ct)
	}
	wg.Wait()
	return errors.Join(errs...)
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
// Obtain a ChildTracker via StartWithTracking, StartFrozenWithTracking,
// AttachWithTracking, or AttachFrozenWithTracking.
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

// IsFrozen reports whether the tracker is currently in frozen mode.
// It delegates to the parent handle, which is the source of truth for the
// current injection mode shared across the tracked process tree.
func (c *ChildTracker) IsFrozen() bool { return c.Handle.IsFrozen() }

// PIDs returns the process IDs of the parent and all currently tracked
// children. The parent PID is always first.
func (c *ChildTracker) PIDs() []int {
	c.mu.Lock()
	defer c.mu.Unlock()
	pids := make([]int, 0, 1+len(c.children))
	pids = append(pids, c.parentPID)
	for pid := range c.children {
		pids = append(pids, pid)
	}
	return pids
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

// applyAll calls fn on the parent handle and all currently tracked children
// concurrently. Children that have exited (ESRCH) are silently removed from
// the tracked set. ESRCH on the parent is also cleared — calling Reset or
// SetTime on an already-dead parent is not an error.
func (c *ChildTracker) applyAll(fn func(*Handle) error) error {
	c.mu.Lock()
	handles := make([]*Handle, 0, 1+len(c.children))
	handles = append(handles, c.Handle)
	childPIDs := make([]int, 0, len(c.children))
	for pid, h := range c.children {
		handles = append(handles, h)
		childPIDs = append(childPIDs, pid)
	}
	c.mu.Unlock()

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

	// Clear ESRCH on the parent and prune dead children.
	c.mu.Lock()
	if errors.Is(errs[0], unix.ESRCH) {
		errs[0] = nil
	}
	for i, pid := range childPIDs {
		if errors.Is(errs[i+1], unix.ESRCH) {
			delete(c.children, pid)
			errs[i+1] = nil
		}
	}
	c.mu.Unlock()

	return errors.Join(errs...)
}

// SetTime updates the parent and all tracked children to advancing mode at target.
func (c *ChildTracker) SetTime(target time.Time) error {
	return c.applyAll(func(h *Handle) error { return h.SetTime(target) })
}

// Freeze pins the parent and all tracked children at target.
func (c *ChildTracker) Freeze(target time.Time) error {
	return c.applyAll(func(h *Handle) error { return h.Freeze(target) })
}

// Advance shifts the parent's current fake time by d and applies the resulting
// target to all tracked children. Preserves the current mode of the parent.
func (c *ChildTracker) Advance(d time.Duration) error {
	c.Handle.mu.Lock()
	frozen := c.Handle.frozen
	target := c.Handle.effectiveTime().Add(d)
	c.Handle.mu.Unlock()
	if frozen {
		return c.Freeze(target)
	}
	return c.SetTime(target)
}

// Reset restores the parent and all tracked children to real time.
func (c *ChildTracker) Reset() error {
	return c.applyAll(func(h *Handle) error { return h.Reset() })
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

// AttachWithTracking attaches to an already-running process with advancing fake
// time and returns a ChildTracker that automatically injects fake time into any
// processes the target spawns via fork or vfork.
// Requires CAP_SYS_PTRACE and ptrace_scope <= 1.
func AttachWithTracking(pid int, target time.Time) (*ChildTracker, error) {
	ih, tr, err := inject.InjectAtTimeKeepTracer(pid, target)
	if err != nil {
		return nil, fmt.Errorf("faketime: AttachWithTracking pid %d: %w", pid, err)
	}
	return newChildTracker(newAdvancingHandle(ih, target), tr, pid), nil
}

// AttachFrozenWithTracking attaches to an already-running process with the
// clock frozen at target and returns a ChildTracker for its descendants.
// Requires CAP_SYS_PTRACE and ptrace_scope <= 1.
func AttachFrozenWithTracking(pid int, target time.Time) (*ChildTracker, error) {
	ih, tr, err := inject.InjectFrozenKeepTracer(pid, target)
	if err != nil {
		return nil, fmt.Errorf("faketime: AttachFrozenWithTracking pid %d: %w", pid, err)
	}
	return newChildTracker(newFrozenHandle(ih, target), tr, pid), nil
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
// Handles whose process has exited (ESRCH) are silently removed from the
// session; all other per-handle errors are joined and returned.
func (s *Session) applyAll(fn func(*Handle) error, updateState func()) error {
	s.mu.Lock()
	primary := make([]*Handle, len(s.handles))
	copy(primary, s.handles)
	var extra []*Handle
	for _, ct := range s.trackers {
		extra = append(extra, ct.Children()...)
	}
	s.mu.Unlock()

	all := append(primary, extra...)
	errs := make([]error, len(all))
	var wg sync.WaitGroup
	for i, h := range all {
		wg.Add(1)
		go func(i int, h *Handle) {
			defer wg.Done()
			errs[i] = fn(h)
		}(i, h)
	}
	wg.Wait()

	// Collect dead primary handles (ESRCH) and remove them. Tracker children
	// are managed by the watchLoop; just clear their ESRCH errors.
	var dead []*Handle
	for i, h := range primary {
		if errors.Is(errs[i], unix.ESRCH) {
			dead = append(dead, h)
			errs[i] = nil
		}
	}
	for i := range extra {
		if errors.Is(errs[len(primary)+i], unix.ESRCH) {
			errs[len(primary)+i] = nil
		}
	}
	if len(dead) > 0 {
		s.mu.Lock()
		live := make([]*Handle, 0, len(s.handles))
		for _, h := range s.handles {
			pruned := false
			for _, d := range dead {
				if d == h {
					pruned = true
					break
				}
			}
			if !pruned {
				live = append(live, h)
			}
		}
		s.handles = live
		s.mu.Unlock()
	}

	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("faketime: applyAll: %w", err)
	}
	s.mu.Lock()
	updateState()
	s.mu.Unlock()
	return nil
}

// Prune removes handles for processes that are no longer alive and returns the
// number of handles removed. For sessions created with WithTracking, trackers
// whose parent process has exited are also closed and removed.
func (s *Session) Prune() int {
	// Determine which trackers are dead outside the lock (IsAlive is cheap).
	s.mu.Lock()
	liveTrackers := make([]*ChildTracker, 0, len(s.trackers))
	var deadTrackers []*ChildTracker
	for _, ct := range s.trackers {
		if ct.Handle.IsAlive() {
			liveTrackers = append(liveTrackers, ct)
		} else {
			deadTrackers = append(deadTrackers, ct)
		}
	}
	s.trackers = liveTrackers

	// Build a set of handles to remove: dead tracker parents + dead standalone handles.
	deadSet := make(map[*Handle]bool, len(deadTrackers))
	for _, ct := range deadTrackers {
		deadSet[ct.Handle] = true
	}
	liveTrackerHandles := make(map[*Handle]bool, len(liveTrackers))
	for _, ct := range liveTrackers {
		liveTrackerHandles[ct.Handle] = true
	}

	n := len(deadTrackers)
	live := make([]*Handle, 0, len(s.handles))
	for _, h := range s.handles {
		switch {
		case deadSet[h]:
			// dead tracker parent — already counted above
		case liveTrackerHandles[h]:
			live = append(live, h) // alive tracker parent, keep
		case h.IsAlive():
			live = append(live, h)
		default:
			n++ // dead standalone handle
		}
	}
	s.handles = live
	s.mu.Unlock()

	// Close dead trackers outside the lock — Close waits for the watchLoop goroutine.
	for _, ct := range deadTrackers {
		ct.Close() //nolint:errcheck
	}
	return n
}

// PIDs returns the process IDs of all handles currently in the session,
// including tracker parent processes. The order is unspecified.
func (s *Session) PIDs() []int {
	s.mu.Lock()
	defer s.mu.Unlock()
	pids := make([]int, len(s.handles))
	for i, h := range s.handles {
		pids[i] = h.PID()
	}
	return pids
}
