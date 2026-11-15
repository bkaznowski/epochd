//go:build linux

// Package faketime injects fake time into local (non-Kubernetes) processes.
// It wraps pkg/inject for use in Go tests and CLI tooling without requiring a
// running Kubernetes cluster or agent daemon.
package faketime

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"

	"github.com/bkaznowski/epochd/pkg/inject"
	"github.com/bkaznowski/epochd/pkg/preload"
	"github.com/bkaznowski/epochd/pkg/procmem"
	"golang.org/x/sys/unix"
)

// Handle holds an active time injection for a single process.
//
// A Handle is backed by one of two mechanisms. The default is a ptrace-injected
// vDSO trampoline (h is set). Handles from StartPreload, StartFrozenPreload and
// Sessions created with WithMode(ModePreload) use an LD_PRELOAD library instead
// (pre is set); the process and every descendant that keeps its environment
// share that one clock. All the methods below behave the same either way.
type Handle struct {
	h   *inject.Handle  // vDSO backend; nil for preload handles
	pre *preload.Handle // preload backend; nil for vDSO handles
	pid int             // preload handles only: inject.Handle carries its own PID
	mu  sync.Mutex
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

func newPreloadHandle(pre *preload.Handle, pid int, target time.Time, frozen bool) *Handle {
	if frozen {
		return &Handle{pre: pre, pid: pid, frozenAt: target, frozen: true}
	}
	return &Handle{pre: pre, pid: pid, offset: time.Until(target)}
}

// effectiveTime returns the fake time the process currently sees.
// Caller must hold h.mu.
func (h *Handle) effectiveTime() time.Time {
	if h.frozen {
		return h.frozenAt
	}
	return time.Now().Add(h.offset)
}

// backend returns the vDSO injection handle currently behind h. Exec
// re-injection (ChildTracker) swaps it for a fresh one under h.mu, so every
// reader must go through here rather than touching h.h directly.
func (h *Handle) backend() *inject.Handle {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.h
}

// PID returns the OS process ID of the process this Handle controls.
func (h *Handle) PID() int {
	if h.pre != nil {
		return h.pid
	}
	return h.backend().PID
}

// IsFrozen reports whether the handle is currently in frozen mode.
func (h *Handle) IsFrozen() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.frozen
}

// IsAlive reports whether the process is still running. It sends signal 0,
// which checks for the process's existence without delivering a signal.
func (h *Handle) IsAlive() bool { return syscall.Kill(h.PID(), 0) == nil }

// EffectiveTime returns the fake time the process currently sees. For advancing
// mode this is time.Now() plus the stored offset; for frozen mode it is the
// pinned instant.
func (h *Handle) EffectiveTime() time.Time {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.effectiveTime()
}

// Start starts cmd with fake time injected from the moment the process begins.
// It sets cmd.SysProcAttr to enable ptrace, then uses the FollowChild path to
// start cmd and inject before the process executes any user code.
// No elevated permissions required. The caller must not call cmd.Start() before
// calling Start: Start calls it, on the same OS thread that drives the ptrace
// requests injection needs — required for correctness, since PTRACE_TRACEME
// ties the tracee's tracer to the exact thread that performed the fork.
func Start(cmd *exec.Cmd, target time.Time) (*Handle, error) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Ptrace = true
	h, err := inject.InjectAtTimeFollowChild(cmd, target)
	if err != nil {
		if cmd.Process != nil {
			cmd.Process.Kill() //nolint:errcheck
			cmd.Wait()         //nolint:errcheck
		}
		return nil, fmt.Errorf("faketime: inject: %w", err)
	}
	return newAdvancingHandle(h, target), nil
}

// StartFrozen starts cmd with the clock frozen at target. Unlike Start, the
// process sees the same timestamp on every call to clock_gettime until
// SetTime or Freeze is called. The caller must not call cmd.Start() before
// calling StartFrozen; see Start's doc comment for why.
func StartFrozen(cmd *exec.Cmd, target time.Time) (*Handle, error) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Ptrace = true
	h, err := inject.InjectFrozenFollowChild(cmd, target)
	if err != nil {
		if cmd.Process != nil {
			cmd.Process.Kill() //nolint:errcheck
			cmd.Wait()         //nolint:errcheck
		}
		return nil, fmt.Errorf("faketime: inject frozen: %w", err)
	}
	return newFrozenHandle(h, target), nil
}

// StartPreload is like Start but fakes the clock with LD_PRELOAD instead of
// ptrace and a vDSO patch. No ptrace, no elevated permissions, and every
// descendant of cmd that keeps its environment inherits the clock across fork
// and exec, so no ChildTracker is needed.
//
// The trade-off is coverage: it only works for dynamically linked non-Go
// binaries, because the Go runtime reads the vDSO directly and static binaries
// load no dynamic loader. Such binaries are refused with an error wrapping
// preload.ErrNotInterposable rather than silently left on real time. A child
// started with a scrubbed environment also keeps real time.
//
// Call Close on the returned Handle after cmd and its descendants have exited.
func StartPreload(cmd *exec.Cmd, target time.Time) (*Handle, error) {
	return startPreload(cmd, target, false)
}

// StartFrozenPreload is like StartPreload with the clock stopped at target.
func StartFrozenPreload(cmd *exec.Cmd, target time.Time) (*Handle, error) {
	return startPreload(cmd, target, true)
}

func startPreload(cmd *exec.Cmd, target time.Time, frozen bool) (*Handle, error) {
	var pre *preload.Handle
	var err error
	if frozen {
		pre, err = preload.StartFrozen(cmd, target)
	} else {
		pre, err = preload.Start(cmd, target)
	}
	if err != nil {
		return nil, fmt.Errorf("faketime: preload: %w", err)
	}
	return newPreloadHandle(pre, cmd.Process.Pid, target, frozen), nil
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

// Close releases the resources behind a preload Handle (its shared state and
// the temporary copy of the library). Call it only after the process and all
// its descendants have exited: children that exec later load the library by
// path. It is a no-op for vDSO handles, which hold nothing to release.
func (h *Handle) Close() error {
	if h.pre == nil {
		return nil
	}
	return h.pre.Close()
}

// PreloadAttached reports how many processes have loaded the preload library for
// this handle's clock. ok is false for a vDSO handle, which has no such library.
// A count of zero once the command has run means LD_PRELOAD never took effect for
// it (see preload.Handle.Attached for the usual causes) and it saw real time.
func (h *Handle) PreloadAttached() (n int, ok bool) {
	if h.pre == nil {
		return 0, false
	}
	return h.pre.Attached(), true
}

// SetTime updates the fake time without stopping the process (process_vm_writev
// for the vDSO backend, a shared-memory write for the preload backend).
func (h *Handle) SetTime(target time.Time) error {
	var err error
	if h.pre != nil {
		err = h.pre.SetTime(target)
	} else {
		err = h.backend().SetTime(target)
	}
	if err != nil {
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
	var err error
	if h.pre != nil {
		err = h.pre.Freeze(target)
	} else {
		err = h.backend().Freeze(target)
	}
	if err != nil {
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

// Mode selects how a Session fakes the clock for a process it starts. A session
// can use both modes at once: WithMode sets the default, WithModeFilter decides
// per process, and Session.StartMode decides per call. One session clock still
// drives them all, whichever mode each process runs in.
type Mode int

const (
	// ModeVDSO fakes the clock with ptrace and a patched vDSO. It works on any
	// binary, including Go and statically linked ones, and on processes that are
	// already running (Session.Attach), but needs ptrace and, to follow
	// descendants, a tracer kept attached (see WithTracking). This is the
	// default.
	ModeVDSO Mode = iota

	// ModePreload fakes the clock with an LD_PRELOAD library. No ptrace and no
	// elevated permissions; every descendant that keeps its environment inherits
	// the clock across fork and exec, so WithTracking and WithTrackingFilter have
	// nothing to do and are ignored. It only works for dynamically linked non-Go
	// binaries (Session.Start returns an error wrapping preload.ErrNotInterposable
	// for anything else) and only for processes the session starts, so
	// Session.Attach returns an error. Call Session.Close after the processes
	// have exited to release the shared state and temporary files.
	ModePreload
)

// String returns the mode's name ("vdso" or "preload").
func (m Mode) String() string {
	switch m {
	case ModeVDSO:
		return "vdso"
	case ModePreload:
		return "preload"
	}
	return fmt.Sprintf("Mode(%d)", int(m))
}

// checkMode returns an error for a Mode value that is neither defined constant,
// so a typo or a zero-value mix-up fails loudly instead of silently picking one.
func checkMode(m Mode) error {
	if m != ModeVDSO && m != ModePreload {
		return fmt.Errorf("faketime: unknown %v", m)
	}
	return nil
}

// WithMode selects the session's default mode, used for every process that
// neither WithModeFilter nor Session.StartMode decides otherwise. Without this
// option the default is ModeVDSO, matching prior behaviour.
func WithMode(m Mode) SessionOption {
	return func(s *Session) { s.mode = m }
}

// ModeFilter decides, for a single process being added to a Session, which Mode
// fakes its clock. path is the resolved executable path: for Session.Start it
// is cmd.Path; for Session.Attach it is read from /proc/<pid>/exe and may be
// empty if it could not be read.
//
// A filter that uses LD_PRELOAD where it works and falls back to ptrace where it
// does not:
//
//	faketime.WithModeFilter(func(path string) faketime.Mode {
//		if preload.CheckInterposable(path) != nil { // Go or static binary
//			return faketime.ModeVDSO
//		}
//		return faketime.ModePreload
//	})
//
// Be aware that CheckInterposable inspects only the binary itself. A shell
// script that goes on to exec a Go binary passes it, and that grandchild would
// stay on real time under ModePreload; start such launchers with ModeVDSO and
// WithTracking instead.
type ModeFilter func(path string) Mode

// WithModeFilter makes the mode a per-process decision: filter is consulted on
// every call to Session.Start and Session.Attach, and its result decides how
// that process is faked. It takes precedence over WithMode, and is itself
// overridden by an explicit Session.StartMode. WithFakeTimeFilter still runs
// first, so a process it excludes gets no fake time in either mode.
//
// A session with both kinds of process behaves as one clock: SetTime, Freeze,
// Advance, Reset, WaitPID, Prune and Close all work across the mix.
// WithTracking and WithTrackingFilter apply only to the processes that end up
// in ModeVDSO.
func WithModeFilter(filter ModeFilter) SessionOption {
	return func(s *Session) { s.modeFilter = filter }
}

// WithTracking enables automatic fake-time injection into any processes spawned
// via fork, vfork, or exec while the session is active. When this option is
// used, Session.Start internally calls StartWithTracking and Session.Close must
// be called when the session is no longer needed to stop the watch goroutine
// and detach ptrace.
//
// This is a single session-wide on/off switch: every process added via
// Session.Start/Session.Attach gets tracking. To decide tracking on a
// per-process basis instead, use WithTrackingFilter.
func WithTracking() SessionOption {
	return func(s *Session) { s.tracking = true }
}

// TrackingFilter decides, for a single process being added to a Session,
// whether it should be started/attached with automatic child-process
// tracking (fork/exec descendants auto-injected) or as a plain, untracked
// handle. path is the resolved executable path — for Session.Start this is
// cmd.Path; for Session.Attach it is read from /proc/<pid>/exe and may be
// empty if it could not be read.
//
// Returning true makes the session call StartWithTracking/AttachWithTracking
// for this process. Returning false makes it call the plain Start/Attach:
// only this one process is faked, its forks and execs are not observed.
type TrackingFilter func(path string) bool

// WithTrackingFilter makes tracking a per-process decision instead of the
// single session-wide on/off switch WithTracking provides: filter is
// consulted on every call to Session.Start and Session.Attach, and its
// return value decides whether that specific process is started/attached
// with tracking. Takes precedence over WithTracking if both are given.
func WithTrackingFilter(filter TrackingFilter) SessionOption {
	return func(s *Session) { s.trackingFilter = filter }
}

// readBinaryPath best-effort reads the resolved executable path of a running
// process from /proc/<pid>/exe. Used by Session.Attach to feed a
// TrackingFilter or FakeTimeFilter. Returns "" if it could not be read (e.g.
// the process has already exited).
func readBinaryPath(pid int) string {
	path, _ := os.Readlink(fmt.Sprintf("/proc/%d/exe", pid))
	return path
}

// FakeTimeFilter decides, for a single process being added to a Session,
// whether it should receive fake time at all. path is the resolved
// executable path — for Session.Start this is cmd.Path; for Session.Attach it
// is read from /proc/<pid>/exe and may be empty if it could not be read.
//
// Returning true injects fake time as usual (still subject to the session's
// TrackingFilter/WithTracking for whether descendants get auto-tracked).
// Returning false skips injection entirely: Session.Start just starts cmd
// normally (no ptrace, real clock, no Handle added) and Session.Attach leaves
// the already-running process untouched (no Handle added). Either way the
// process is not under fake-time control and Session.SetTime/Freeze/Reset
// never touch it.
type FakeTimeFilter func(path string) bool

// WithFakeTimeFilter restricts which processes added to the session receive
// fake time at all, on a per-process basis; see FakeTimeFilter's
// documentation for exact semantics. Without this option, every process
// added via Start/Attach gets fake time, matching prior behaviour. Checked
// before TrackingFilter/WithTracking, which only matter for processes this
// filter accepts.
func WithFakeTimeFilter(filter FakeTimeFilter) SessionOption {
	return func(s *Session) { s.fakeTimeFilter = filter }
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
	offset         time.Duration
	frozenAt       time.Time
	frozen         bool
	tracking       bool
	trackingFilter TrackingFilter
	fakeTimeFilter FakeTimeFilter
	mode           Mode
	modeFilter     ModeFilter
	trackers       []*ChildTracker
	cmdWaiters     map[int]*cmdWaiter // lazily populated by WaitPID, keyed by pid
}

// cmdWaiter caches a single cmd.Wait() call's result behind a sync.Once, so
// WaitPID is safe to call concurrently or more than once for the same
// untracked pid — matching the tracked path's ergonomics, where Wait reads
// from an already-closed channel and so has the same property for free.
type cmdWaiter struct {
	once sync.Once
	ws   unix.WaitStatus
	err  error
}

// NewSession creates an empty session with the given initial target time.
// Pass WithTracking() to enable automatic injection into forked/exec'd
// children for every process, WithTrackingFilter to decide per process, or
// WithFakeTimeFilter to decide per process whether it gets fake time at all.
// Pass WithMode(ModePreload) to fake the clock with LD_PRELOAD instead of
// ptrace; see Mode for what that changes.
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

// Start starts cmd and adds the resulting handle to the session. Whether cmd
// gets fake time at all is decided by the session's FakeTimeFilter if one was
// given via WithFakeTimeFilter; when it does, whether it also gets automatic
// fork/exec tracking is decided by TrackingFilter if given via
// WithTrackingFilter, otherwise by the session-wide WithTracking on/off switch.
// Use Session.WaitPID(cmd.Process.Pid), not cmd.Wait(), to learn when cmd
// exits — it works the same way whether or not cmd ends up tracked, so
// callers don't need to know or care which applies; see WaitPID's doc
// comment for why calling cmd.Wait() directly should be avoided.
//
// The Mode that fakes cmd's clock is the one WithModeFilter returns for
// cmd.Path if a filter was given, otherwise the session default from WithMode
// (ModeVDSO unless set). Use StartMode to choose for one call explicitly.
func (s *Session) Start(cmd *exec.Cmd) error {
	return s.start(cmd, nil)
}

// StartMode is like Start but fakes cmd's clock with mode, overriding both
// WithModeFilter and the session default for this one process. This is how a
// caller who knows better than any filter picks a mode per process, for
// example ModePreload for a Postgres server and ModeVDSO for a Go service in the
// same session. WithFakeTimeFilter still applies first: a process it excludes
// is started on real time whatever mode is requested.
func (s *Session) StartMode(cmd *exec.Cmd, mode Mode) error {
	return s.start(cmd, &mode)
}

// start is Start and StartMode: explicit, when non-nil, wins over the filter
// and the session default.
func (s *Session) start(cmd *exec.Cmd, explicit *Mode) error {
	s.mu.Lock()
	target := s.effectiveTarget()
	frozen := s.frozen
	tracking := s.tracking
	trackingFilter := s.trackingFilter
	fakeTimeFilter := s.fakeTimeFilter
	mode := s.mode
	modeFilter := s.modeFilter
	s.mu.Unlock()

	if fakeTimeFilter != nil && !fakeTimeFilter(cmd.Path) {
		if err := cmd.Start(); err != nil {
			return fmt.Errorf("faketime: Session.Start: %w", err)
		}
		s.mu.Lock()
		s.cmds = append(s.cmds, cmd)
		s.mu.Unlock()
		return nil
	}

	switch {
	case explicit != nil:
		mode = *explicit
	case modeFilter != nil:
		mode = modeFilter(cmd.Path)
	}
	if err := checkMode(mode); err != nil {
		return fmt.Errorf("faketime: Session.Start: %w", err)
	}

	// Preload mode needs no tracer: descendants inherit the clock through the
	// environment, so the tracking switch and filter are irrelevant here.
	if mode == ModePreload {
		h, err := startPreload(cmd, target, frozen)
		if err != nil {
			return err
		}
		s.mu.Lock()
		s.handles = append(s.handles, h)
		s.cmds = append(s.cmds, cmd)
		s.mu.Unlock()
		return nil
	}

	if trackingFilter != nil {
		tracking = trackingFilter(cmd.Path)
	}

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
// Whether pid gets fake time at all is decided by the session's
// FakeTimeFilter if one was given via WithFakeTimeFilter; when it does,
// whether it also gets automatic fork/exec tracking is decided by
// TrackingFilter if given via WithTrackingFilter, otherwise by the
// session-wide WithTracking on/off switch. Both filters read pid's path from
// /proc/<pid>/exe.
// Requires CAP_SYS_PTRACE and ptrace_scope <= 1 when tracking is active.
func (s *Session) Attach(pid int) error {
	s.mu.Lock()
	target := s.effectiveTarget()
	frozen := s.frozen
	tracking := s.tracking
	trackingFilter := s.trackingFilter
	fakeTimeFilter := s.fakeTimeFilter
	mode := s.mode
	modeFilter := s.modeFilter
	s.mu.Unlock()

	var path string
	if trackingFilter != nil || fakeTimeFilter != nil || modeFilter != nil {
		path = readBinaryPath(pid)
	}

	if fakeTimeFilter != nil && !fakeTimeFilter(path) {
		return nil
	}

	if modeFilter != nil {
		mode = modeFilter(path)
	}
	if err := checkMode(mode); err != nil {
		return fmt.Errorf("faketime: Session.Attach pid %d: %w", pid, err)
	}

	// LD_PRELOAD is read by the dynamic loader at exec, so a process that is
	// already running cannot be given the library. In a mixed session this is
	// the mode filter's cue to return ModeVDSO for processes it expects to attach.
	if mode == ModePreload {
		return fmt.Errorf("faketime: Session.Attach pid %d: ModePreload cannot attach to a running process; use ModeVDSO (WithModeFilter can pick it per process)", pid)
	}

	if trackingFilter != nil {
		tracking = trackingFilter(path)
	}

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

// EffectiveTime returns the fake time the session's tracked processes
// currently see: for advancing mode this is time.Now() plus the session's
// offset; for frozen mode it is the pinned instant. This is also the target
// new processes are injected at via Start/Attach.
func (s *Session) EffectiveTime() time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.effectiveTarget()
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
//
// For a session created with WithMode(ModePreload) it also releases each
// process's shared clock state and temporary library copy, and is required for
// that reason. Call it after the processes (and their descendants) have exited;
// the fake clock is gone once it returns, so call Reset first if you still want
// the processes on real time.
func (s *Session) Close() error {
	s.mu.Lock()
	trackers := make([]*ChildTracker, len(s.trackers))
	copy(trackers, s.trackers)
	var preloaded []*Handle
	for _, h := range s.handles {
		if h.pre != nil {
			preloaded = append(preloaded, h)
		}
	}
	s.mu.Unlock()

	var preErrs []error
	for _, h := range preloaded {
		preErrs = append(preErrs, h.Close())
	}
	if len(trackers) == 0 {
		return errors.Join(preErrs...)
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
	return errors.Join(append(errs, preErrs...)...)
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

	mu           sync.Mutex
	tracer       *procmem.Tracer
	parentPID    int
	parentGone   bool            // set once the parent's exit has been observed
	parentExitWS unix.WaitStatus // valid once parentExitCh is closed
	parentExitCh chan struct{}   // closed once the parent's exit has been observed; see Wait
	children     map[int]*Handle // childPID → Handle
	pendingStop  map[int]bool    // children waiting for their initial ptrace stop
	strays       map[int]bool    // children (also in children) found by sweepStrays, not ptrace-attached
	done         chan struct{}
	wg           sync.WaitGroup
	loopErr      error
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

// Wait blocks until the tracked parent process exits and returns its final
// wait status, or returns an error if the tracker is closed first without
// having observed an exit.
//
// Callers must use this — not cmd.Wait() — to learn when a process started
// with tracking enabled has exited. ChildTracker's own background loop
// already reaps the parent's exit via wait4 as part of tracking fork/vfork/
// exec events; a second, external wait4/waitid call for the same pid (e.g.
// from cmd.Wait(), one goroutine per process is a common pattern in
// multi-service integration tests) races it for the same one-time kernel
// notification. Depending on which call the kernel happens to deliver it to,
// either cmd.Wait() blocks forever (having missed the exit, since the
// tracker's loop already reaped it) or the tracker's own bookkeeping misses
// it — and in between, the two calls can also race for other wait-reportable
// transitions on that pid (e.g. a stop from an unrelated signal delivery),
// which can make cmd.Wait() return an incorrect result for what was never
// actually process termination.
func (c *ChildTracker) Wait() (unix.WaitStatus, error) {
	select {
	case <-c.parentExitCh:
		c.mu.Lock()
		defer c.mu.Unlock()
		return c.parentExitWS, nil
	case <-c.done:
		return unix.WaitStatus(0), fmt.Errorf("faketime: ChildTracker.Wait: tracker closed before pid %d was observed to exit", c.parentPID)
	}
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
	stray := []bool{false} // parallel to handles
	for pid, h := range c.children {
		handles = append(handles, h)
		childPIDs = append(childPIDs, pid)
		stray = append(stray, c.strays[pid])
	}
	c.mu.Unlock()

	errs := make([]error, len(handles))
	var wg sync.WaitGroup
	for i, h := range handles {
		wg.Add(1)
		go func(i int, h *Handle) {
			defer wg.Done()
			errs[i] = fn(h)
			// A process the tracker follows can be caught between its exec and
			// the watch loop re-hooking it, which takes a few milliseconds; the
			// hooks are verified before every write, so the write is refused
			// rather than landing in the new program's memory. Try again once
			// the loop has put the hooks back. A stray is not re-hooked by an
			// event, so it is dropped below and picked up by the next sweep.
			for try := 0; try < 10 && !stray[i] && errors.Is(errs[i], inject.ErrHooksLost); try++ {
				time.Sleep(20 * time.Millisecond)
				errs[i] = fn(h)
			}
		}(i, h)
	}
	wg.Wait()

	// Clear ESRCH on the parent and prune dead children.
	c.mu.Lock()
	if errors.Is(errs[0], unix.ESRCH) {
		errs[0] = nil
	}
	for i, pid := range childPIDs {
		switch {
		case errors.Is(errs[i+1], unix.ESRCH):
			delete(c.children, pid)
			delete(c.strays, pid)
			errs[i+1] = nil
		case errors.Is(errs[i+1], inject.ErrHooksLost) && c.strays[pid]:
			// A stray that exec'd. It was never ptrace-attached, so nothing
			// re-hooked it; forget it and let the next sweep inject it afresh.
			delete(c.children, pid)
			delete(c.strays, pid)
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
// processes the target spawns via fork or vfork. Use the returned
// ChildTracker's Wait method to learn when pid exits — see ChildTracker.Wait's
// doc comment for why any other wait-family call for pid should be avoided.
// Requires CAP_SYS_PTRACE and ptrace_scope <= 1.
func AttachWithTracking(pid int, target time.Time) (*ChildTracker, error) {
	ih, tr, err := inject.InjectAtTimeKeepTracer(pid, target)
	if err != nil {
		return nil, fmt.Errorf("faketime: AttachWithTracking pid %d: %w", pid, err)
	}
	return newChildTracker(newAdvancingHandle(ih, target), tr, pid), nil
}

// AttachFrozenWithTracking attaches to an already-running process with the
// clock frozen at target and returns a ChildTracker for its descendants. Use
// the returned ChildTracker's Wait method to learn when pid exits — see
// ChildTracker.Wait's doc comment for why.
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
// via fork or vfork. No elevated permissions required. The caller must not
// call cmd.Start() before calling StartWithTracking; see Start's doc comment
// for why. Likewise, do not call cmd.Wait() to learn when the process exits
// — use the returned ChildTracker's Wait method instead; see its doc comment
// for why.
func StartWithTracking(cmd *exec.Cmd, target time.Time) (*ChildTracker, error) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Ptrace = true
	ih, tr, err := inject.InjectAtTimeFollowChildKeepTracer(cmd, target)
	if err != nil {
		if cmd.Process != nil {
			cmd.Process.Kill() //nolint:errcheck
			cmd.Wait()         //nolint:errcheck
		}
		return nil, fmt.Errorf("faketime: StartWithTracking: %w", err)
	}
	return newChildTracker(newAdvancingHandle(ih, target), tr, cmd.Process.Pid), nil
}

// StartFrozenWithTracking starts cmd with the clock frozen at target and
// returns a ChildTracker that automatically injects fake time into any
// processes spawned via fork or vfork. The caller must not call cmd.Start()
// before calling StartFrozenWithTracking; see Start's doc comment for why.
// Likewise, do not call cmd.Wait() to learn when the process exits — use the
// returned ChildTracker's Wait method instead; see its doc comment for why.
func StartFrozenWithTracking(cmd *exec.Cmd, target time.Time) (*ChildTracker, error) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Ptrace = true
	ih, tr, err := inject.InjectFrozenFollowChildKeepTracer(cmd, target)
	if err != nil {
		if cmd.Process != nil {
			cmd.Process.Kill() //nolint:errcheck
			cmd.Wait()         //nolint:errcheck
		}
		return nil, fmt.Errorf("faketime: StartFrozenWithTracking: %w", err)
	}
	return newChildTracker(newFrozenHandle(ih, target), tr, cmd.Process.Pid), nil
}

func newChildTracker(h *Handle, tr *procmem.Tracer, parentPID int) *ChildTracker {
	ct := &ChildTracker{
		Handle:       h,
		tracer:       tr,
		parentPID:    parentPID,
		parentExitCh: make(chan struct{}),
		children:     make(map[int]*Handle),
		pendingStop:  make(map[int]bool),
		strays:       make(map[int]bool),
		done:         make(chan struct{}),
	}
	ct.wg.Add(1)
	go ct.watchLoop()
	return ct
}

// trackedPIDs returns a snapshot of every pid this tracker is currently
// responsible for observing: the parent (unless its exit has already been
// observed) plus every child discovered so far.
func (c *ChildTracker) trackedPIDs() []int {
	c.mu.Lock()
	defer c.mu.Unlock()
	pids := make([]int, 0, 1+len(c.children))
	if !c.parentGone {
		pids = append(pids, c.parentPID)
	}
	for pid := range c.children {
		pids = append(pids, pid)
	}
	return pids
}

func (c *ChildTracker) watchLoop() {
	defer c.wg.Done()
	defer c.cleanup()

	var nextSweep time.Time
	for {
		select {
		case <-c.done:
			return
		default:
		}

		// Children that forked from a thread the tracer is not attached to
		// produce no event; find them here (see sweepStrays).
		if !time.Now().Before(nextSweep) {
			began := time.Now()
			c.sweepStrays()
			wait := 10 * time.Since(began)
			if wait < strayScanInterval {
				wait = strayScanInterval
			}
			nextSweep = time.Now().Add(wait)
		}

		pids := c.trackedPIDs()
		if len(pids) == 0 {
			// Parent and every discovered child have exited — nothing left
			// to observe.
			return
		}

		// Poll each tracked pid individually (WaitPIDNonBlocking) instead of
		// a single wildcard wait4(-1, ...) (WaitAnyNonBlocking). A session
		// with WithTracking creates one ChildTracker — and one dedicated
		// tracer OS thread — per process, all forked by the same caller
		// goroutine rather than by each tracker's own thread. WNOTHREAD only
		// restricts a -1 wait to children of the calling thread specifically,
		// which none of these tracees are, so it does not actually prevent
		// one tracker's wildcard wait from observing (and consuming, via
		// ContPID/SetOptionsPID) a wait-status transition that belongs to a
		// completely different tracker's tracee — corrupting both: the
		// rightful tracker misses the event it was waiting for, and the
		// tracee gets resumed by code that knows nothing about it. An
		// exact-pid wait cannot have this ambiguity: every ptrace event this
		// tracker cares about always arrives on a pid already in this set —
		// the parent, an already-known child, or (via a fork event's
		// GetEventMsgPID, itself delivered on an already-known pid) a child
		// just added to it — so there is no need for a wildcard wait at all.
		found := false
		for _, pid := range pids {
			ws, ok, err := c.tracer.WaitPIDNonBlocking(pid)
			if err != nil {
				if errors.Is(err, syscall.ECHILD) {
					// Already reaped by a previous iteration; the next
					// trackedPIDs() snapshot will drop it.
					continue
				}
				c.mu.Lock()
				c.loopErr = err
				c.mu.Unlock()
				continue
			}
			if !ok {
				continue
			}
			found = true
			c.handleEvent(pid, ws)
		}

		if !found {
			// No events pending on any tracked pid — avoid busy-spinning.
			time.Sleep(5 * time.Millisecond)
		}
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
			c.parentGone = true
			c.parentExitWS = ws
			if c.loopErr == nil {
				c.loopErr = fmt.Errorf("faketime: parent process %d exited unexpectedly", pid)
			}
			c.mu.Unlock()
			close(c.parentExitCh)
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

		// The child inherits the trampoline via fork — build a Handle for it,
		// based on the Handle of whichever process actually just forked (pid),
		// not unconditionally the top-level parent's. Forking preserves the
		// FORKING process's own current address space, which only matches the
		// top-level parent's original one if pid is that parent or a
		// descendant that has never exec'd — once anything in the chain execs,
		// handleExec re-injects a fresh trampoline at a new address recorded on
		// that process's own Handle, and any later fork from it must build its
		// child's Handle from that current one instead.
		parentH := c.Handle
		if pid != c.parentPID {
			c.mu.Lock()
			if h, ok := c.children[pid]; ok {
				parentH = h
			}
			c.mu.Unlock()
		}

		parentH.mu.Lock()
		childIH := inject.ChildHandle(parentH.h, childPID)
		var childH *Handle
		if parentH.frozen {
			childH = newFrozenHandle(childIH, parentH.frozenAt)
		} else {
			childH = newAdvancingHandle(childIH, parentH.effectiveTime())
		}
		parentH.mu.Unlock()

		c.mu.Lock()
		c.children[childPID] = childH
		c.pendingStop[childPID] = true
		delete(c.strays, childPID) // attached from birth after all
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
		if !c.strays[pid] { // strays were never attached
			childPIDs = append(childPIDs, pid)
		}
	}
	c.mu.Unlock()

	c.tracer.DetachAll(childPIDs) //nolint:errcheck
	c.tracer.InterruptDetach()    //nolint:errcheck
	// Nothing is attached any more; release the tracer's goroutine and the OS
	// thread it pins, which otherwise live for the rest of the process.
	c.tracer.Close()
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
	nPrimary := len(primary)
	errs := make([]error, len(all))
	var wg sync.WaitGroup
	for i, h := range all {
		wg.Add(1)
		go func(i int, h *Handle) {
			defer wg.Done()
			errs[i] = fn(h)
			// A tracker child caught between its exec and being re-hooked: give
			// the tracker's watch loop the few milliseconds it needs (see
			// ChildTracker.applyAll). For a handle nobody tracks, hooks lost to
			// an exec are a real error and are reported.
			for try := 0; try < 10 && i >= nPrimary && errors.Is(errs[i], inject.ErrHooksLost); try++ {
				time.Sleep(20 * time.Millisecond)
				errs[i] = fn(h)
			}
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
		// A tracker child that has gone, or a stray that exec'd and lost its
		// hooks, is for the tracker to deal with (see sweepStrays), not an error.
		if e := errs[len(primary)+i]; errors.Is(e, unix.ESRCH) || errors.Is(e, inject.ErrHooksLost) {
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
// whose parent process has exited are also closed and removed. A preload handle
// whose process has exited is kept while any descendant still has its library
// mapped, since that descendant is still on the handle's clock.
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
	var deadPreload []*Handle // leader gone; descendants may still run on its clock
	live := make([]*Handle, 0, len(s.handles))
	for _, h := range s.handles {
		switch {
		case deadSet[h]:
			// dead tracker parent — already counted above
		case liveTrackerHandles[h]:
			live = append(live, h) // alive tracker parent, keep
		case h.IsAlive():
			live = append(live, h)
		case h.pre != nil:
			deadPreload = append(deadPreload, h) // decided below, outside the lock
		default:
			n++ // dead standalone handle
		}
	}
	s.handles = live
	s.mu.Unlock()

	// A preload handle's clock outlives the process it was started for: a
	// daemonised child, or a worker the leader left behind, keeps reading it and
	// may still exec programs that load the library by path. Pruning such a
	// handle would delete the library under them and drop the only way to move
	// their clock, so it stays until nothing is using it. Scanning /proc is done
	// without the session lock held.
	var inUse []*Handle
	for _, h := range deadPreload {
		if h.pre.InUse() {
			inUse = append(inUse, h)
			continue
		}
		n++
		// Otherwise the temp dir would leak until process exit, since
		// Session.Close only sees handles still in the session.
		h.Close() //nolint:errcheck
	}
	if len(inUse) > 0 {
		s.mu.Lock()
		s.handles = append(s.handles, inUse...)
		s.mu.Unlock()
	}

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

// WaitPID blocks until pid — the top-level pid from a previous call to
// Start (not a forked descendant) — exits, and returns its final wait
// status. Works the same way regardless of whether tracking is enabled for
// pid, so callers do not need to know or care which applies: it delegates
// to the ChildTracker's own Wait if pid is tracked (see ChildTracker.Wait
// for why that matters — calling cmd.Wait() yourself instead races the
// tracker's internal wait4-based bookkeeping for the same pid and can
// misbehave); otherwise it safely wraps cmd.Wait() itself, caching the
// result behind a sync.Once so it's just as safe to call concurrently or
// more than once for the same pid as the tracked path is.
//
// Either way, callers should use WaitPID instead of calling cmd.Wait()
// directly on a cmd added to this Session via Start.
//
// Returns an error if pid was never added to this session via Start (or was,
// but its cmd/tracker can no longer be found — e.g. after Prune).
func (s *Session) WaitPID(pid int) (unix.WaitStatus, error) {
	s.mu.Lock()
	var ct *ChildTracker
	for _, t := range s.trackers {
		if t.parentPID == pid {
			ct = t
			break
		}
	}
	if ct != nil {
		s.mu.Unlock()
		return ct.Wait()
	}

	var cmd *exec.Cmd
	for _, c := range s.cmds {
		if c.Process != nil && c.Process.Pid == pid {
			cmd = c
			break
		}
	}
	if cmd == nil {
		s.mu.Unlock()
		return unix.WaitStatus(0), fmt.Errorf("faketime: Session.WaitPID: pid %d not found in this session", pid)
	}
	if s.cmdWaiters == nil {
		s.cmdWaiters = make(map[int]*cmdWaiter)
	}
	cw, ok := s.cmdWaiters[pid]
	if !ok {
		cw = &cmdWaiter{}
		s.cmdWaiters[pid] = cw
	}
	s.mu.Unlock()

	cw.once.Do(func() {
		waitErr := cmd.Wait()
		if cmd.ProcessState == nil {
			cw.err = fmt.Errorf("faketime: Session.WaitPID: pid %d: wait did not produce a process state: %w", pid, waitErr)
			return
		}
		sysWS, ok := cmd.ProcessState.Sys().(syscall.WaitStatus)
		if !ok {
			cw.err = fmt.Errorf("faketime: Session.WaitPID: pid %d: unexpected process state type %T", pid, cmd.ProcessState.Sys())
			return
		}
		cw.ws = unix.WaitStatus(sysWS)
	})
	return cw.ws, cw.err
}
