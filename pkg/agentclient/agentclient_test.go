package agentclient_test

import (
	"context"
	"errors"
	"fmt"
	"net"
	"testing"
	"time"

	"github.com/bkaznowski/epochd/pkg/agentclient"
	"github.com/bkaznowski/epochd/pkg/agentpb"
	"github.com/bkaznowski/epochd/pkg/auth"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// fakeAgent is a minimal AgentServiceServer for testing.
type fakeAgent struct {
	agentpb.UnimplementedAgentServiceServer

	injectHandleID string
	injectErr      error
	setTimeErr     error
	resetErr       error
	releaseErr     error
	statusResp     *agentpb.StatusResponse
	statusErr      error
	hang           time.Duration // how long Inject blocks before answering (0: not at all)
}

func (f *fakeAgent) Release(_ context.Context, _ *agentpb.ReleaseRequest) (*agentpb.ReleaseResponse, error) {
	if f.releaseErr != nil {
		return nil, f.releaseErr
	}
	return &agentpb.ReleaseResponse{}, nil
}

func (f *fakeAgent) Inject(ctx context.Context, _ *agentpb.InjectRequest) (*agentpb.InjectResponse, error) {
	if f.hang > 0 {
		select {
		case <-time.After(f.hang):
		case <-ctx.Done():
		}
	}
	if f.injectErr != nil {
		return nil, f.injectErr
	}
	return &agentpb.InjectResponse{HandleId: f.injectHandleID}, nil
}

func (f *fakeAgent) SetTime(_ context.Context, _ *agentpb.SetTimeRequest) (*agentpb.SetTimeResponse, error) {
	if f.setTimeErr != nil {
		return nil, f.setTimeErr
	}
	return &agentpb.SetTimeResponse{}, nil
}

func (f *fakeAgent) Reset(_ context.Context, _ *agentpb.ResetRequest) (*agentpb.ResetResponse, error) {
	if f.resetErr != nil {
		return nil, f.resetErr
	}
	return &agentpb.ResetResponse{}, nil
}

func (f *fakeAgent) Status(_ context.Context, _ *agentpb.StatusRequest) (*agentpb.StatusResponse, error) {
	if f.statusErr != nil {
		return nil, f.statusErr
	}
	if f.statusResp != nil {
		return f.statusResp, nil
	}
	return &agentpb.StatusResponse{
		HandleId:       "h1",
		LastTargetTime: timestamppb.New(time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)),
		StateAddr:      "0x7ffe1234",
		Generation:     3,
		Pid:            42,
	}, nil
}

// setupPool starts a real TCP gRPC server backed by agent and returns a Pool
// dialing it, plus the node IP string. The pool and server are cleaned up via t.Cleanup.
func setupPool(t *testing.T, agent *fakeAgent) (*agentclient.Pool, string) {
	t.Helper()
	return setupPoolWith(t, agent, nil)
}

// setupPoolWith is setupPool with server options (an auth interceptor, say) and
// pool options (a token, say).
func setupPoolWith(t *testing.T, agent *fakeAgent, serverOpts []grpc.ServerOption, poolOpts ...agentclient.PoolOption) (*agentclient.Pool, string) {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen: %v", err)
	}
	port := lis.Addr().(*net.TCPAddr).Port

	srv := grpc.NewServer(serverOpts...)
	agentpb.RegisterAgentServiceServer(srv, agent)
	go srv.Serve(lis) //nolint:errcheck
	t.Cleanup(srv.Stop)

	pool := agentclient.NewPool(fmt.Sprint(port), poolOpts...)
	t.Cleanup(pool.Close)
	return pool, "127.0.0.1"
}

func TestPoolInject(t *testing.T) {
	agent := &fakeAgent{injectHandleID: "abc123"}
	pool, nodeIP := setupPool(t, agent)

	handleID, err := pool.Inject(context.Background(), nodeIP, "containerd://abc", time.Now().Add(24*time.Hour), false)
	if err != nil {
		t.Fatalf("Inject: %v", err)
	}
	if handleID != "abc123" {
		t.Errorf("handleID = %q, want %q", handleID, "abc123")
	}
}

func TestPoolInjectFreeze(t *testing.T) {
	agent := &fakeAgent{injectHandleID: "frozen1"}
	pool, nodeIP := setupPool(t, agent)

	handleID, err := pool.Inject(context.Background(), nodeIP, "containerd://abc", time.Now().Add(24*time.Hour), true)
	if err != nil {
		t.Fatalf("Inject (freeze): %v", err)
	}
	if handleID != "frozen1" {
		t.Errorf("handleID = %q, want %q", handleID, "frozen1")
	}
}

func TestPoolInjectError(t *testing.T) {
	agent := &fakeAgent{injectErr: status.Error(codes.Internal, "boom")}
	pool, nodeIP := setupPool(t, agent)

	_, err := pool.Inject(context.Background(), nodeIP, "c1", time.Now(), false)
	if err == nil {
		t.Fatal("expected error from Inject, got nil")
	}
}

func TestPoolSetTime(t *testing.T) {
	agent := &fakeAgent{}
	pool, nodeIP := setupPool(t, agent)

	if err := pool.SetTime(context.Background(), nodeIP, "h1", time.Now().Add(time.Hour), false); err != nil {
		t.Fatalf("SetTime: %v", err)
	}
}

func TestPoolSetTimeFreeze(t *testing.T) {
	agent := &fakeAgent{}
	pool, nodeIP := setupPool(t, agent)

	if err := pool.SetTime(context.Background(), nodeIP, "h1", time.Now().Add(time.Hour), true); err != nil {
		t.Fatalf("SetTime (freeze): %v", err)
	}
}

func TestPoolSetTimeError(t *testing.T) {
	agent := &fakeAgent{setTimeErr: status.Error(codes.NotFound, "not found")}
	pool, nodeIP := setupPool(t, agent)

	if err := pool.SetTime(context.Background(), nodeIP, "missing", time.Now(), false); err == nil {
		t.Fatal("expected error from SetTime, got nil")
	}
}

func TestPoolReset(t *testing.T) {
	agent := &fakeAgent{}
	pool, nodeIP := setupPool(t, agent)

	if err := pool.Reset(context.Background(), nodeIP, "h1"); err != nil {
		t.Fatalf("Reset: %v", err)
	}
}

func TestPoolResetError(t *testing.T) {
	agent := &fakeAgent{resetErr: status.Error(codes.NotFound, "not found")}
	pool, nodeIP := setupPool(t, agent)

	if err := pool.Reset(context.Background(), nodeIP, "missing"); err == nil {
		t.Fatal("expected error from Reset, got nil")
	}
}

func TestPoolGetStatus(t *testing.T) {
	agent := &fakeAgent{}
	pool, nodeIP := setupPool(t, agent)

	s, err := pool.GetStatus(context.Background(), nodeIP, "h1")
	if err != nil {
		t.Fatalf("GetStatus: %v", err)
	}
	if s.Generation != 3 {
		t.Errorf("Generation = %d, want 3", s.Generation)
	}
	if s.PID != 42 {
		t.Errorf("PID = %d, want 42", s.PID)
	}
	if s.StateAddr != "0x7ffe1234" {
		t.Errorf("StateAddr = %q, want %q", s.StateAddr, "0x7ffe1234")
	}
	if s.LastTarget != "2030-01-01T00:00:00Z" {
		t.Errorf("LastTarget = %q, want %q", s.LastTarget, "2030-01-01T00:00:00Z")
	}
}

func TestPoolGetStatusError(t *testing.T) {
	agent := &fakeAgent{statusErr: status.Error(codes.NotFound, "not found")}
	pool, nodeIP := setupPool(t, agent)

	if _, err := pool.GetStatus(context.Background(), nodeIP, "missing"); err == nil {
		t.Fatal("expected error from GetStatus, got nil")
	}
}

func TestPoolClose(t *testing.T) {
	agent := &fakeAgent{injectHandleID: "h-close"}
	pool, nodeIP := setupPool(t, agent)

	// Make a call first to open a connection.
	if _, err := pool.Inject(context.Background(), nodeIP, "c1", time.Now(), false); err != nil {
		t.Fatalf("Inject: %v", err)
	}
	// Close must not panic; double-close must also be safe.
	pool.Close()
	pool.Close()
}

func TestPoolConnectionReuse(t *testing.T) {
	agent := &fakeAgent{injectHandleID: "h-reuse"}
	pool, nodeIP := setupPool(t, agent)

	// Multiple calls to the same nodeIP must reuse the connection without errors.
	for i := 0; i < 3; i++ {
		if _, err := pool.Inject(context.Background(), nodeIP, "c1", time.Now(), false); err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
	}
}

func TestPoolRelease(t *testing.T) {
	pool, nodeIP := setupPool(t, &fakeAgent{})
	if err := pool.Release(context.Background(), nodeIP, "h1"); err != nil {
		t.Fatalf("Release: %v", err)
	}
}

func TestPoolReleaseError(t *testing.T) {
	pool, nodeIP := setupPool(t, &fakeAgent{releaseErr: status.Error(codes.NotFound, "gone")})
	err := pool.Release(context.Background(), nodeIP, "h1")
	if err == nil || status.Code(errors.Unwrap(err)) != codes.NotFound {
		t.Errorf("Release error = %v, want it to wrap the agent's NOT_FOUND", err)
	}
}

func TestPoolGetStatusReturnsEveryPid(t *testing.T) {
	agent := &fakeAgent{statusResp: &agentpb.StatusResponse{
		HandleId: "h1", LastTargetTime: timestamppb.Now(), Pid: 10, Pids: []int32{10, 11, 12},
	}}
	pool, nodeIP := setupPool(t, agent)
	st, err := pool.GetStatus(context.Background(), nodeIP, "h1")
	if err != nil {
		t.Fatal(err)
	}
	if st.PID != 10 || len(st.PIDs) != 3 {
		t.Errorf("status = pid %d pids %v, want 10 and all three", st.PID, st.PIDs)
	}
}

// With a token configured the pool authenticates; without it, an agent that
// demands one refuses.
func TestPoolSendsTheBearerToken(t *testing.T) {
	serverOpts := []grpc.ServerOption{grpc.UnaryInterceptor(auth.UnaryServerInterceptor("s3cret"))}

	withToken, nodeIP := setupPoolWith(t, &fakeAgent{injectHandleID: "h1"}, serverOpts, agentclient.WithToken("s3cret"))
	if _, err := withToken.Inject(context.Background(), nodeIP, "containerd://abc", time.Now(), false); err != nil {
		t.Errorf("Inject with the right token: %v", err)
	}

	wrong, nodeIP := setupPoolWith(t, &fakeAgent{}, serverOpts, agentclient.WithToken("nope"))
	_, err := wrong.Inject(context.Background(), nodeIP, "containerd://abc", time.Now(), false)
	if status.Code(errors.Unwrap(err)) != codes.Unauthenticated {
		t.Errorf("Inject with the wrong token = %v, want Unauthenticated", err)
	}

	none, nodeIP := setupPoolWith(t, &fakeAgent{}, serverOpts)
	if err := none.Reset(context.Background(), nodeIP, "h1"); status.Code(errors.Unwrap(err)) != codes.Unauthenticated {
		t.Errorf("Reset with no token = %v, want Unauthenticated", err)
	}
}

// A call without a deadline of its own must still be bounded, or one dead node
// stalls a whole timeshift.
func TestPoolCallTimeoutBoundsAHungAgent(t *testing.T) {
	pool, nodeIP := setupPoolWith(t, &fakeAgent{hang: time.Minute}, nil, agentclient.WithCallTimeout(150*time.Millisecond))

	start := time.Now()
	_, err := pool.Inject(context.Background(), nodeIP, "containerd://abc", time.Now(), false)
	if err == nil {
		t.Fatal("Inject against a hung agent returned no error")
	}
	if code := status.Code(errors.Unwrap(err)); code != codes.DeadlineExceeded {
		t.Errorf("code = %v, want DeadlineExceeded (%v)", code, err)
	}
	if took := time.Since(start); took > 5*time.Second {
		t.Errorf("the call took %v; the timeout did not bound it", took)
	}
}

// A deadline the caller set must win over the pool's default.
func TestPoolRespectsTheCallersDeadline(t *testing.T) {
	pool, nodeIP := setupPoolWith(t, &fakeAgent{hang: 300 * time.Millisecond, injectHandleID: "h"}, nil,
		agentclient.WithCallTimeout(20*time.Millisecond))

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := pool.Inject(ctx, nodeIP, "containerd://abc", time.Now(), false); err != nil {
		t.Errorf("the pool's 20ms default overrode the caller's 5s deadline: %v", err)
	}
}
