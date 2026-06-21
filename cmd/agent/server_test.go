//go:build linux

package main

import (
	"context"
	"fmt"
	"net"
	"testing"
	"time"

	"epochd/pkg/agentpb"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// startTestServer starts an in-process gRPC server backed by the real server
// implementation and returns a connected client. No network port is opened.
func startTestServer(t *testing.T) agentpb.AgentServiceClient {
	t.Helper()
	lis := bufconn.Listen(1 << 20) // 1 MiB in-memory buffer
	srv := grpc.NewServer()
	agentpb.RegisterAgentServiceServer(srv, newServer())

	go srv.Serve(lis) //nolint:errcheck
	t.Cleanup(srv.GracefulStop)

	conn, err := grpc.NewClient("passthrough://bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return lis.DialContext(ctx)
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("grpc.NewClient: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	return agentpb.NewAgentServiceClient(conn)
}

// TestStatusUnknownHandle verifies that Status returns NOT_FOUND for a handle
// that was never created.
func TestStatusUnknownHandle(t *testing.T) {
	client := startTestServer(t)
	_, err := client.Status(context.Background(), &agentpb.StatusRequest{
		HandleId: "does-not-exist",
	})
	if err == nil {
		t.Fatal("expected error for unknown handle, got nil")
	}
	if code := status.Code(err); code != codes.NotFound {
		t.Errorf("got gRPC code %v, want NOT_FOUND", code)
	}
}

// TestSetTimeUnknownHandle verifies that SetTime returns NOT_FOUND for an
// unknown handle.
func TestSetTimeUnknownHandle(t *testing.T) {
	client := startTestServer(t)
	_, err := client.SetTime(context.Background(), &agentpb.SetTimeRequest{
		HandleId:   "does-not-exist",
		TargetTime: timestamppb.New(time.Now().Add(24 * time.Hour)),
	})
	assertNotFound(t, "SetTime", err)
}

// TestResetUnknownHandle verifies that Reset returns NOT_FOUND for an unknown
// handle.
func TestResetUnknownHandle(t *testing.T) {
	client := startTestServer(t)
	_, err := client.Reset(context.Background(), &agentpb.ResetRequest{
		HandleId: "does-not-exist",
	})
	assertNotFound(t, "Reset", err)
}

// TestInjectMissingFields verifies that Inject returns INVALID_ARGUMENT when
// required fields are absent.
func TestInjectMissingFields(t *testing.T) {
	client := startTestServer(t)

	// Missing container_id.
	_, err := client.Inject(context.Background(), &agentpb.InjectRequest{
		TargetTime: timestamppb.New(time.Now()),
	})
	if code := status.Code(err); code != codes.InvalidArgument {
		t.Errorf("missing container_id: got %v, want INVALID_ARGUMENT", code)
	}

	// Missing target_time (nil).
	_, err = client.Inject(context.Background(), &agentpb.InjectRequest{
		ContainerId: "containerd://abc123",
	})
	if code := status.Code(err); code != codes.InvalidArgument {
		t.Errorf("missing target_time: got %v, want INVALID_ARGUMENT", code)
	}
}

// TestInjectUnknownContainer verifies that Inject returns NOT_FOUND when the
// container ID cannot be resolved to a PID. In a test environment (no
// kubelet), k8sresolve.LookupPID always returns not-found for a made-up ID.
func TestInjectUnknownContainer(t *testing.T) {
	client := startTestServer(t)
	_, err := client.Inject(context.Background(), &agentpb.InjectRequest{
		ContainerId: "containerd://0000000000000000000000000000000000000000000000000000000000000000",
		TargetTime:  timestamppb.New(time.Now().Add(24 * time.Hour)),
	})
	if code := status.Code(err); code != codes.NotFound {
		t.Errorf("unknown container: got gRPC code %v, want NOT_FOUND", code)
	}
}

// TestHandleIDUniqueness verifies that newHandleID() never returns the same
// value twice across a large number of calls.
func TestHandleIDUniqueness(t *testing.T) {
	seen := make(map[string]struct{}, 10000)
	for i := 0; i < 10000; i++ {
		id := newHandleID()
		if _, dup := seen[id]; dup {
			t.Fatalf("duplicate handle ID after %d calls: %s", i, id)
		}
		seen[id] = struct{}{}
	}
}

// ---------------------------------------------------------------------------
// Drain tests
// ---------------------------------------------------------------------------

// TestDrain verifies that drain calls resetNow on every handle and continues
// past errors (one failing handle must not abort the rest).
func TestDrain(t *testing.T) {
	s := newServer()

	var resetCalls []string
	addHandle := func(id string, fail bool) {
		s.mu.Lock()
		s.handles[id] = &handleEntry{
			resetFn: func() error {
				resetCalls = append(resetCalls, id)
				if fail {
					return fmt.Errorf("simulated reset failure for %s", id)
				}
				return nil
			},
		}
		s.mu.Unlock()
	}
	addHandle("h1", false)
	addHandle("h2", false)
	addHandle("h3", true) // fails; drain must continue to h1 and h2

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	s.drain(ctx)

	if len(resetCalls) != 3 {
		t.Errorf("drain reset %d handle(s), want 3", len(resetCalls))
	}
}

// TestDrainEmpty verifies drain on a server with no handles is a no-op.
func TestDrainEmpty(t *testing.T) {
	s := newServer()
	s.drain(context.Background()) // must not panic
}

// TestDrainRespectsTimeout verifies that drain stops early when its context
// expires. We use a pre-cancelled context to trigger the timeout immediately.
func TestDrainRespectsTimeout(t *testing.T) {
	s := newServer()

	resetCalls := 0
	for i := range 5 {
		i := i
		s.mu.Lock()
		s.handles[fmt.Sprintf("h%d", i)] = &handleEntry{
			resetFn: func() error { resetCalls++; return nil },
		}
		s.mu.Unlock()
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled — drain should stop immediately

	s.drain(ctx)

	// With a pre-cancelled context, drain should exit after the first
	// ctx.Err() check (before or after the first reset). Either 0 or 1
	// resets may have occurred depending on scheduling; the key guarantee
	// is that not all 5 ran.
	if resetCalls == 5 {
		t.Errorf("drain ran all 5 resets despite cancelled context")
	}
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func assertNotFound(t *testing.T, op string, err error) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s: expected error, got nil", op)
	}
	if code := status.Code(err); code != codes.NotFound {
		t.Errorf("%s: got gRPC code %v, want NOT_FOUND", op, code)
	}
}
