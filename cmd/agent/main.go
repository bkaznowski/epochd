//go:build linux

// Command agent is the node-level gRPC daemon that performs vDSO clock
// injection on behalf of the epochd controller.
//
// It runs as a privileged DaemonSet pod with hostPID: true and CAP_SYS_PTRACE,
// which allows it to attach to any process on the node.
//
// The controller reaches each agent directly by pod IP:
//
//	grpc://<node-agent-pod-ip>:9100
//
// Usage:
//
//	agent [--listen=:9100]
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/bkaznowski/epochd/pkg/agentpb"
	"github.com/bkaznowski/epochd/pkg/inject"
	"github.com/bkaznowski/epochd/pkg/k8sresolve"
	applog "github.com/bkaznowski/epochd/pkg/log"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/reflection"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func main() {
	listen := flag.String("listen", ":9100", "gRPC listen address")
	flag.Parse()

	logger := applog.New().With("component", "agent")

	lis, err := net.Listen("tcp", *listen)
	if err != nil {
		logger.Error("listen", "addr", *listen, "err", err)
		os.Exit(1)
	}

	svc := newServer(logger)
	srv := grpc.NewServer()
	agentpb.RegisterAgentServiceServer(srv, svc)
	reflection.Register(srv) // lets grpcurl/grpc-health-probe work without a proto file

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go func() {
		<-ctx.Done()
		logger.Info("received shutdown signal")
		srv.GracefulStop()
	}()

	logger.Info("listening", "addr", *listen)
	if err := srv.Serve(lis); err != nil {
		logger.Error("serve", "err", err)
		os.Exit(1)
	}

	// GracefulStop has drained in-flight RPCs; now reset all injected handles so
	// target processes are not left running with a stale fake clock.
	drainCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	svc.drain(drainCtx)
	logger.Info("shutdown complete")
}

// ---------------------------------------------------------------------------
// Server implementation
// ---------------------------------------------------------------------------

type handleEntry struct {
	handle      *inject.Handle
	lastTarget  time.Time
	containerID string // bare container ID, for Status
	// resetFn overrides reset behaviour; nil means call handle.SetTime(time.Now()).
	// Populated by tests to avoid needing a real PID for process_vm_writev.
	resetFn func() error
}

func (e *handleEntry) resetNow() error {
	if e.resetFn != nil {
		return e.resetFn()
	}
	return e.handle.SetTime(time.Now())
}

type server struct {
	agentpb.UnimplementedAgentServiceServer

	mu      sync.RWMutex
	handles map[string]*handleEntry
	log     *slog.Logger
}

func newServer(logger *slog.Logger) *server {
	return &server{
		handles: make(map[string]*handleEntry),
		log:     logger,
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
		if err := e.resetNow(); err != nil {
			s.log.Warn("drain reset failed", "err", err)
		}
	}
}

// Inject installs the clock hook in the container and returns a handle ID.
func (s *server) Inject(ctx context.Context, req *agentpb.InjectRequest) (*agentpb.InjectResponse, error) {
	if req.ContainerId == "" {
		return nil, status.Error(codes.InvalidArgument, "container_id is required")
	}
	if req.TargetTime == nil {
		return nil, status.Error(codes.InvalidArgument, "target_time is required")
	}

	target := req.TargetTime.AsTime()
	freeze := req.Freeze

	pid, err := k8sresolve.LookupPID(req.ContainerId)
	if err != nil {
		return nil, status.Errorf(codes.NotFound, "resolve container %q: %v", req.ContainerId, err)
	}

	var h *inject.Handle
	if freeze {
		h, err = inject.InjectFrozen(pid, target)
	} else {
		// InjectAtTime attaches via PTRACE_ATTACH, writes the trampoline, patches
		// the vDSO, and detaches. It converts target→offset right before writing.
		h, err = inject.InjectAtTime(pid, target)
	}
	if err != nil {
		return nil, status.Errorf(codes.Internal, "inject pid %d: %v", pid, err)
	}

	id := newHandleID()
	s.mu.Lock()
	s.handles[id] = &handleEntry{
		handle:      h,
		lastTarget:  target,
		containerID: req.ContainerId,
	}
	s.mu.Unlock()

	mode := "advancing"
	if freeze {
		mode = "frozen"
	}
	s.log.Info("inject",
		"container", shortID(req.ContainerId),
		"pid", pid,
		"target", target.UTC().Format(time.RFC3339),
		"mode", mode,
		"handle", id[:8])

	return &agentpb.InjectResponse{HandleId: id}, nil
}

// SetTime updates the fake time on an already-injected process.
// This is a single process_vm_writev — the target is never stopped.
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

	if freeze {
		err = entry.handle.Freeze(target)
	} else {
		err = entry.handle.SetTime(target)
	}
	if err != nil {
		return nil, status.Errorf(codes.Internal, "SetTime: %v", err)
	}

	s.mu.Lock()
	entry.lastTarget = target
	s.mu.Unlock()

	return &agentpb.SetTimeResponse{}, nil
}

// Reset snaps the target process back to the real clock.
func (s *server) Reset(ctx context.Context, req *agentpb.ResetRequest) (*agentpb.ResetResponse, error) {
	if req.HandleId == "" {
		return nil, status.Error(codes.InvalidArgument, "handle_id is required")
	}

	entry, err := s.lookupHandle(req.HandleId)
	if err != nil {
		return nil, err
	}

	now := time.Now()
	if err := entry.handle.SetTime(now); err != nil {
		return nil, status.Errorf(codes.Internal, "Reset: %v", err)
	}

	s.mu.Lock()
	entry.lastTarget = now
	s.mu.Unlock()

	s.log.Info("reset", "handle", req.HandleId[:8], "pid", entry.handle.PID)
	return &agentpb.ResetResponse{}, nil
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

	s.mu.RLock()
	lastTarget := entry.lastTarget
	s.mu.RUnlock()

	return &agentpb.StatusResponse{
		HandleId:       req.HandleId,
		LastTargetTime: timestamppb.New(lastTarget),
		StateAddr:      fmt.Sprintf("0x%x", entry.handle.StateAddr),
		Generation:     entry.handle.Generation(),
		Pid:            int32(entry.handle.PID),
	}, nil
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func (s *server) lookupHandle(id string) (*handleEntry, error) {
	s.mu.RLock()
	entry := s.handles[id]
	s.mu.RUnlock()
	if entry == nil {
		return nil, status.Errorf(codes.NotFound, "handle %q not found", id)
	}
	return entry, nil
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
