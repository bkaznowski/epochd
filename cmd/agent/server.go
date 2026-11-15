//go:build linux

package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/bkaznowski/epochd/pkg/agentpb"
	"github.com/bkaznowski/epochd/pkg/inject"

	"golang.org/x/sys/unix"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// ---------------------------------------------------------------------------
// What the server needs from the machine it runs on
// ---------------------------------------------------------------------------

// hook is the clock hook installed in one process.
type hook interface {
	Pid() int
	SetTime(target time.Time) error
	Freeze(target time.Time) error
	Uninstall() error
	Generation() uint32
	StateAddr() uintptr
}

// injector finds a container's processes and hooks them. The real one drives
// ptrace; tests substitute a fake so the server's logic can be exercised without
// touching real processes.
type injector interface {
	// pids lists the processes of a container, its init first.
	pids(containerID string) ([]int, error)
	// inject hooks pid's clock, reusing hooks it already carries.
	inject(pid int, target time.Time, freeze bool) (hook, error)
	// adopt returns a hook for a process that already carries one, or
	// inject.ErrNotInjected.
	adopt(pid int) (hook, error)
}

// ---------------------------------------------------------------------------
// Server implementation
// ---------------------------------------------------------------------------

// handleEntry is one Inject: every process of a container that was hooked for it.
type handleEntry struct {
	mu          sync.Mutex
	procs       []hook // injected processes, in the order they were hooked
	lastTarget  time.Time
	containerID string // bare container ID, for Status and for finding new processes
	// resetFn overrides reset behaviour; nil means set every process to the real time.
	// Populated by tests to avoid needing a real PID for process_vm_writev.
	resetFn func() error
}

func (e *handleEntry) resetNow() error {
	if e.resetFn != nil {
		return e.resetFn()
	}
	return e.apply(time.Now(), false, nil, nil)
}

// outcome of applying a change to one process.
type outcome int

const (
	applied outcome = iota
	gone            // the process exited: drop it
	failed          // it is alive but the change did not land
)

// classify decides what an error from a hook means for that process.
func classify(err error) outcome {
	switch {
	case err == nil:
		return applied
	case errors.Is(err, unix.ESRCH):
		return gone
	default:
		return failed
	}
}

// apply sets every process of the handle to target (frozen or advancing).
// Processes that have exited are dropped. A process that has exec'd, so no
// longer carries the hooks, is hooked afresh through reinject when one is given.
// It returns an error only if no process took the change, and errGone if no
// process is left at all. The caller holds e.mu.
func (e *handleEntry) apply(target time.Time, freeze bool, reinject func(pid int) (hook, error), log *slog.Logger) error {
	if len(e.procs) == 0 {
		return errGone
	}
	kept := e.procs[:0:0]
	var ok, bad int
	var firstErr error
	for _, h := range e.procs {
		err := set(h, target, freeze)
		if errors.Is(err, inject.ErrHooksLost) && reinject != nil {
			// The process is alive but replaced its address space. Hook it again.
			if nh, rerr := reinject(h.Pid()); rerr == nil {
				h, err = nh, nil
			} else {
				err = rerr
			}
		}
		switch classify(err) {
		case applied:
			ok++
			kept = append(kept, h)
		case gone:
			// exited: forget it
		default:
			bad++
			kept = append(kept, h)
			if firstErr == nil {
				firstErr = fmt.Errorf("pid %d: %w", h.Pid(), err)
			}
			if log != nil {
				log.Warn("clock update failed for a process", "pid", h.Pid(), "err", err)
			}
		}
	}
	e.procs = kept
	switch {
	case len(e.procs) == 0:
		return errGone
	case ok == 0 && bad > 0:
		return firstErr
	}
	return nil
}

func set(h hook, target time.Time, freeze bool) error {
	if freeze {
		return h.Freeze(target)
	}
	return h.SetTime(target)
}

// errGone means every process behind a handle has exited.
var errGone = errors.New("every process behind this handle has exited")

type server struct {
	agentpb.UnimplementedAgentServiceServer

	mu      sync.RWMutex
	handles map[string]*handleEntry
	log     *slog.Logger
	inj     injector
}

func newServer(logger *slog.Logger) *server {
	return newServerWith(logger, realInjector{})
}

func newServerWith(logger *slog.Logger, inj injector) *server {
	return &server{
		handles: make(map[string]*handleEntry),
		log:     logger,
		inj:     inj,
	}
}

// drain resets all active handles to the real clock. Called on shutdown so
// injected processes are not left running with a stale fake clock after the
// agent exits. Errors are logged but do not abort the remaining resets.
func (s *server) drain(ctx context.Context) {
	s.mu.RLock()
	snapshot := make([]*handleEntry, 0, len(s.handles))
	for _, e := range s.handles {
		snapshot = append(snapshot, e)
	}
	s.mu.RUnlock()

	if len(snapshot) == 0 {
		return
	}
	s.log.Info("resetting handles on shutdown", "count", len(snapshot))
	for _, e := range snapshot {
		if ctx.Err() != nil {
			s.log.Warn("drain timed out with handles remaining")
			return
		}
		e.mu.Lock()
		err := e.resetNow()
		e.mu.Unlock()
		if err != nil {
			s.log.Warn("drain reset failed", "err", err)
		}
	}
}

// Inject installs the clock hook in every process of the container and returns
// a handle ID for them.
func (s *server) Inject(ctx context.Context, req *agentpb.InjectRequest) (*agentpb.InjectResponse, error) {
	if req.ContainerId == "" {
		return nil, status.Error(codes.InvalidArgument, "container_id is required")
	}
	if req.TargetTime == nil {
		return nil, status.Error(codes.InvalidArgument, "target_time is required")
	}

	target := req.TargetTime.AsTime()
	freeze := req.Freeze

	pids, err := s.inj.pids(req.ContainerId)
	if err != nil {
		return nil, status.Errorf(codes.NotFound, "resolve container %q: %v", req.ContainerId, err)
	}

	entry := &handleEntry{lastTarget: target, containerID: req.ContainerId}
	var firstErr error
	for _, pid := range pids {
		// Attaches via PTRACE_SEIZE, writes the trampoline, patches the vDSO, and
		// detaches. It converts target→offset right before writing.
		h, err := s.inj.inject(pid, target, freeze)
		if err != nil {
			// A process can exit, or refuse, between listing and attaching; the
			// others are still worth hooking.
			s.log.Warn("inject failed for a process of the container", "container", shortID(req.ContainerId), "pid", pid, "err", err)
			if firstErr == nil {
				firstErr = fmt.Errorf("pid %d: %w", pid, err)
			}
			continue
		}
		entry.procs = append(entry.procs, h)
	}
	if len(entry.procs) == 0 {
		return nil, status.Errorf(codes.Internal, "inject container %s: %v", shortID(req.ContainerId), firstErr)
	}

	id := newHandleID()
	s.mu.Lock()
	s.handles[id] = entry
	s.mu.Unlock()

	mode := "advancing"
	if freeze {
		mode = "frozen"
	}
	s.log.Info("inject",
		"container", shortID(req.ContainerId),
		"pids", pidList(entry.procs),
		"target", target.UTC().Format(time.RFC3339),
		"mode", mode,
		"handle", id[:8])

	return &agentpb.InjectResponse{HandleId: id}, nil
}

// SetTime updates the fake time on every process behind the handle. Each is a
// single process_vm_writev; the targets are never stopped. Processes the
// container started since the last call are hooked first, so a worker forked
// after Inject does not keep a stale private copy of the clock.
func (s *server) SetTime(ctx context.Context, req *agentpb.SetTimeRequest) (*agentpb.SetTimeResponse, error) {
	if req.HandleId == "" {
		return nil, status.Error(codes.InvalidArgument, "handle_id is required")
	}
	if req.TargetTime == nil {
		return nil, status.Error(codes.InvalidArgument, "target_time is required")
	}

	entry, err := s.lookupHandle(req.HandleId)
	if err != nil {
		return nil, err
	}

	target := req.TargetTime.AsTime()
	freeze := req.Freeze

	entry.mu.Lock()
	defer entry.mu.Unlock()
	s.adoptNewProcesses(entry, target, freeze)
	if err := entry.apply(target, freeze, s.reinjector(target, freeze), s.log); err != nil {
		return nil, s.applyError(req.HandleId, "SetTime", err)
	}
	entry.lastTarget = target
	return &agentpb.SetTimeResponse{}, nil
}

// Reset snaps the target processes back to the real clock.
func (s *server) Reset(ctx context.Context, req *agentpb.ResetRequest) (*agentpb.ResetResponse, error) {
	if req.HandleId == "" {
		return nil, status.Error(codes.InvalidArgument, "handle_id is required")
	}

	entry, err := s.lookupHandle(req.HandleId)
	if err != nil {
		return nil, err
	}

	now := time.Now()
	entry.mu.Lock()
	defer entry.mu.Unlock()
	if err := entry.apply(now, false, s.reinjector(now, false), s.log); err != nil {
		return nil, s.applyError(req.HandleId, "Reset", err)
	}
	entry.lastTarget = now

	s.log.Info("reset", "handle", req.HandleId[:8], "pids", pidList(entry.procs))
	return &agentpb.ResetResponse{}, nil
}

// Release takes the hooks out of every process behind the handle and forgets
// the handle. Unlike Reset, which leaves the hook installed (so each clock read
// stays a real syscall), this gives the processes the kernel's own vDSO fast
// path back. If some process cannot be uninstalled the handle is kept, holding
// just those, so the call can be retried.
func (s *server) Release(ctx context.Context, req *agentpb.ReleaseRequest) (*agentpb.ReleaseResponse, error) {
	if req.HandleId == "" {
		return nil, status.Error(codes.InvalidArgument, "handle_id is required")
	}
	entry, err := s.lookupHandle(req.HandleId)
	if err != nil {
		return nil, err
	}

	entry.mu.Lock()
	defer entry.mu.Unlock()

	// A process that forked from a hooked one carries its own copy of the hook
	// and was never tracked. Find those too, or they would keep it.
	known := make(map[int]bool, len(entry.procs))
	for _, h := range entry.procs {
		known[h.Pid()] = true
	}
	if pids, err := s.inj.pids(entry.containerID); err == nil {
		for _, pid := range pids {
			if known[pid] {
				continue
			}
			if h, err := s.inj.adopt(pid); err == nil {
				entry.procs = append(entry.procs, h)
			}
		}
	}

	var remaining []hook
	var firstErr error
	for _, h := range entry.procs {
		if err := h.Uninstall(); err != nil {
			s.log.Warn("uninstall failed", "pid", h.Pid(), "err", err)
			remaining = append(remaining, h)
			if firstErr == nil {
				firstErr = fmt.Errorf("pid %d: %w", h.Pid(), err)
			}
		}
	}
	entry.procs = remaining
	if firstErr != nil {
		return nil, status.Errorf(codes.Internal, "Release: %v", firstErr)
	}

	s.mu.Lock()
	delete(s.handles, req.HandleId)
	s.mu.Unlock()
	s.log.Info("release", "handle", req.HandleId[:8])
	return &agentpb.ReleaseResponse{}, nil
}

// Status returns the current injection state for a handle.
func (s *server) Status(ctx context.Context, req *agentpb.StatusRequest) (*agentpb.StatusResponse, error) {
	if req.HandleId == "" {
		return nil, status.Error(codes.InvalidArgument, "handle_id is required")
	}

	entry, err := s.lookupHandle(req.HandleId)
	if err != nil {
		return nil, err
	}

	entry.mu.Lock()
	defer entry.mu.Unlock()
	if len(entry.procs) == 0 {
		return nil, status.Error(codes.NotFound, "no process is left behind this handle")
	}
	first := entry.procs[0]
	resp := &agentpb.StatusResponse{
		HandleId:       req.HandleId,
		LastTargetTime: timestamppb.New(entry.lastTarget),
		StateAddr:      fmt.Sprintf("0x%x", first.StateAddr()),
		Generation:     first.Generation(),
		Pid:            int32(first.Pid()),
	}
	for _, h := range entry.procs {
		resp.Pids = append(resp.Pids, int32(h.Pid()))
	}
	return resp, nil
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// adoptNewProcesses hooks processes the container has started since the handle
// was last touched. Anything that fails is skipped: it may have exited already,
// and the next call tries again. The caller holds entry.mu.
func (s *server) adoptNewProcesses(entry *handleEntry, target time.Time, freeze bool) {
	pids, err := s.inj.pids(entry.containerID)
	if err != nil {
		return // the container is gone; apply will report it
	}
	known := make(map[int]bool, len(entry.procs))
	for _, h := range entry.procs {
		known[h.Pid()] = true
	}
	for _, pid := range pids {
		if known[pid] {
			continue
		}
		if h, err := s.inj.inject(pid, target, freeze); err == nil {
			entry.procs = append(entry.procs, h)
			s.log.Info("hooked a process the container started", "container", shortID(entry.containerID), "pid", pid)
		}
	}
}

func (s *server) reinjector(target time.Time, freeze bool) func(pid int) (hook, error) {
	return func(pid int) (hook, error) { return s.inj.inject(pid, target, freeze) }
}

// applyError turns a failure of entry.apply into the gRPC status the controller
// understands. When every process has exited the handle is useless, so it is
// dropped and NOT_FOUND returned: the controller reads that as "inject this
// container again", which is the right response to a restarted container.
func (s *server) applyError(handleID, op string, err error) error {
	if errors.Is(err, errGone) {
		s.mu.Lock()
		delete(s.handles, handleID)
		s.mu.Unlock()
		return status.Errorf(codes.NotFound, "%s: handle %q: %v", op, handleID, err)
	}
	return status.Errorf(codes.Internal, "%s: %v", op, err)
}

func (s *server) lookupHandle(id string) (*handleEntry, error) {
	s.mu.RLock()
	entry := s.handles[id]
	s.mu.RUnlock()
	if entry == nil {
		return nil, status.Errorf(codes.NotFound, "handle %q not found", id)
	}
	return entry, nil
}

func pidList(procs []hook) []int {
	out := make([]int, len(procs))
	for i, h := range procs {
		out[i] = h.Pid()
	}
	return out
}

func newHandleID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(fmt.Sprintf("agent: rand.Read: %v", err))
	}
	return hex.EncodeToString(b[:])
}

func shortID(containerID string) string {
	// strip runtime:// prefix for logging
	for i, c := range containerID {
		if c == '/' && i > 0 && containerID[i-1] == '/' {
			containerID = containerID[i+1:]
			break
		}
	}
	if len(containerID) > 12 {
		return containerID[:12]
	}
	return containerID
}
