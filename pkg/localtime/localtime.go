//go:build linux

// Package localtime injects fake time into local (non-Kubernetes) processes.
// It wraps pkg/inject for use in Go tests and CLI tooling without requiring a
// running Kubernetes cluster or agent daemon.
package localtime

import (
	"errors"
	"fmt"
	"os/exec"
	"sync"
	"syscall"
	"time"

	"epochd/pkg/inject"
)

// Handle holds an active time injection for a single process.
type Handle struct {
	h *inject.Handle
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
		return nil, fmt.Errorf("localtime: Start: %w", err)
	}
	h, err := inject.InjectAtTimeFollowChild(cmd.Process.Pid, target)
	if err != nil {
		cmd.Process.Kill() //nolint:errcheck
		cmd.Wait()         //nolint:errcheck
		return nil, fmt.Errorf("localtime: inject: %w", err)
	}
	return &Handle{h: h}, nil
}

// Attach injects fake time into an already-running process.
// Requires CAP_SYS_PTRACE and /proc/sys/kernel/yama/ptrace_scope ≤ 1.
func Attach(pid int, target time.Time) (*Handle, error) {
	h, err := inject.InjectAtTime(pid, target)
	if err != nil {
		return nil, fmt.Errorf("localtime: Attach pid %d: %w", pid, err)
	}
	return &Handle{h: h}, nil
}

// SetTime updates the fake time without stopping the process (process_vm_writev only).
func (h *Handle) SetTime(target time.Time) error {
	return h.h.SetTime(target)
}

// Reset snaps the process back to the real clock. Equivalent to SetTime(time.Now()).
func (h *Handle) Reset() error {
	return h.h.SetTime(time.Now())
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
	target  time.Time
}

// NewSession creates an empty session with the given initial target time.
func NewSession(target time.Time) *Session {
	return &Session{target: target}
}

// Start starts cmd with fake time and adds the resulting handle to the session.
func (s *Session) Start(cmd *exec.Cmd) error {
	h, err := Start(cmd, s.target)
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
	h, err := Attach(pid, s.target)
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
			errs[i] = h.SetTime(target)
		}(i, h)
	}
	wg.Wait()

	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("localtime: SetTime: %w", err)
	}
	s.mu.Lock()
	s.target = target
	s.mu.Unlock()
	return nil
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
