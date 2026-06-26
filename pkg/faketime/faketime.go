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
