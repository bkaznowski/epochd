//go:build linux

package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/bkaznowski/epochd/pkg/agentpb"
	"github.com/bkaznowski/epochd/pkg/auth"
	"github.com/bkaznowski/epochd/pkg/inject"

	"golang.org/x/sys/unix"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// ---------------------------------------------------------------------------
// A fake machine: containers made of processes, with no ptrace anywhere.
// ---------------------------------------------------------------------------

type fakeHook struct {
	pid int

	mu           sync.Mutex
	last         time.Time
	frozen       bool
	setErr       error // returned by SetTime/Freeze while non-nil
	uninstalled  bool
	uninstallErr error
}

func (h *fakeHook) Pid() int { return h.pid }
func (h *fakeHook) record(t time.Time, frozen bool) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.setErr != nil {
		return h.setErr
	}
	h.last, h.frozen = t, frozen
	return nil
}
func (h *fakeHook) SetTime(t time.Time) error { return h.record(t, false) }
func (h *fakeHook) Freeze(t time.Time) error  { return h.record(t, true) }
func (h *fakeHook) Uninstall() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.uninstallErr != nil {
		return h.uninstallErr
	}
	h.uninstalled = true
	return nil
}
func (h *fakeHook) Generation() uint32 { return 3 }
func (h *fakeHook) StateAddr() uintptr { return 0x1000 + uintptr(h.pid) }

func (h *fakeHook) lastTime() (time.Time, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.last, h.frozen
}

type fakeInjector struct {
	mu         sync.Mutex
	containers map[string][]int
	hooks      map[int]*fakeHook // the hook currently installed in each process
	injectErr  map[int]error
	injections []int // every pid inject() was called for, in order
}

func newFakeInjector() *fakeInjector {
	return &fakeInjector{
		containers: map[string][]int{},
		hooks:      map[int]*fakeHook{},
		injectErr:  map[int]error{},
	}
}

func (f *fakeInjector) pids(id string) ([]int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	pids, ok := f.containers[id]
	if !ok {
		return nil, fmt.Errorf("no such container %q", id)
	}
	return append([]int(nil), pids...), nil
}

func (f *fakeInjector) inject(pid int, target time.Time, freeze bool) (hook, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.injections = append(f.injections, pid)
	if err := f.injectErr[pid]; err != nil {
		return nil, err
	}
	h := &fakeHook{pid: pid, last: target, frozen: freeze}
	f.hooks[pid] = h
	return h, nil
}

func (f *fakeInjector) adopt(pid int) (hook, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if h, ok := f.hooks[pid]; ok {
		return h, nil
	}
	return nil, inject.ErrNotInjected
}

func (f *fakeInjector) setContainer(id string, pids ...int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.containers[id] = pids
}

func (f *fakeInjector) hook(pid int) *fakeHook {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.hooks[pid]
}

const testContainer = "containerd://0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// startServerWith serves the real server logic over an in-memory connection.
func startServerWith(t *testing.T, inj injector, opts ...grpc.ServerOption) (agentpb.AgentServiceClient, *server) {
	t.Helper()
	svc := newServerWith(testLogger(), inj)
	lis := bufconn.Listen(1 << 20)
	srv := grpc.NewServer(opts...)
	agentpb.RegisterAgentServiceServer(srv, svc)
	go srv.Serve(lis) //nolint:errcheck
	t.Cleanup(srv.Stop)

	conn, err := grpc.NewClient("passthrough://bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	return agentpb.NewAgentServiceClient(conn), svc
}

func injectContainer(t *testing.T, c agentpb.AgentServiceClient, freeze bool) string {
	t.Helper()
	resp, err := c.Inject(context.Background(), &agentpb.InjectRequest{
		ContainerId: testContainer,
		TargetTime:  timestamppb.New(time.Unix(2_000_000_000, 0)),
		Freeze:      freeze,
	})
	if err != nil {
		t.Fatalf("Inject: %v", err)
	}
	return resp.HandleId
}

func setTime(t *testing.T, c agentpb.AgentServiceClient, id string, at time.Time, freeze bool) error {
	t.Helper()
	_, err := c.SetTime(context.Background(), &agentpb.SetTimeRequest{HandleId: id, TargetTime: timestamppb.New(at), Freeze: freeze})
	return err
}

func wantCode(t *testing.T, what string, err error, want codes.Code) {
	t.Helper()
	if got := status.Code(err); got != want {
		t.Errorf("%s: got %v (%v), want %v", what, got, err, want)
	}
}

// ---------------------------------------------------------------------------
// Containers are more than their init process
// ---------------------------------------------------------------------------

func TestInjectCoversEveryProcessOfTheContainer(t *testing.T) {
	inj := newFakeInjector()
	inj.setContainer(testContainer, 100, 101, 102)
	c, _ := startServerWith(t, inj)

	id := injectContainer(t, c, false)

	for _, pid := range []int{100, 101, 102} {
		if inj.hook(pid) == nil {
			t.Errorf("pid %d was not hooked", pid)
		}
	}
	st, err := c.Status(context.Background(), &agentpb.StatusRequest{HandleId: id})
	if err != nil {
		t.Fatal(err)
	}
	if st.Pid != 100 || len(st.Pids) != 3 || st.Pids[0] != 100 || st.Pids[2] != 102 {
		t.Errorf("Status pid=%d pids=%v, want the init (100) first and all three", st.Pid, st.Pids)
	}
}

// One process refusing must not leave the rest of the container on real time.
func TestInjectToleratesAProcessThatCannotBeHooked(t *testing.T) {
	inj := newFakeInjector()
	inj.setContainer(testContainer, 100, 101, 102)
	inj.injectErr[101] = errors.New("operation not permitted")
	c, _ := startServerWith(t, inj)

	id := injectContainer(t, c, false)

	st, err := c.Status(context.Background(), &agentpb.StatusRequest{HandleId: id})
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Pids) != 2 || st.Pids[0] != 100 || st.Pids[1] != 102 {
		t.Errorf("pids = %v, want [100 102]", st.Pids)
	}
}

func TestInjectFailsWhenNoProcessCanBeHooked(t *testing.T) {
	inj := newFakeInjector()
	inj.setContainer(testContainer, 100)
	inj.injectErr[100] = errors.New("operation not permitted")
	c, _ := startServerWith(t, inj)

	_, err := c.Inject(context.Background(), &agentpb.InjectRequest{
		ContainerId: testContainer, TargetTime: timestamppb.New(time.Now())})
	wantCode(t, "Inject with nothing hookable", err, codes.Internal)
}

func TestInjectUnknownContainerIsNotFound(t *testing.T) {
	c, _ := startServerWith(t, newFakeInjector())
	_, err := c.Inject(context.Background(), &agentpb.InjectRequest{
		ContainerId: "containerd://nope", TargetTime: timestamppb.New(time.Now())})
	wantCode(t, "Inject of an unknown container", err, codes.NotFound)
}

func TestSetTimeReachesEveryProcessAndRespectsMode(t *testing.T) {
	inj := newFakeInjector()
	inj.setContainer(testContainer, 100, 101)
	c, _ := startServerWith(t, inj)
	id := injectContainer(t, c, false)

	at := time.Unix(2_100_000_000, 0)
	if err := setTime(t, c, id, at, true); err != nil {
		t.Fatal(err)
	}
	for _, pid := range []int{100, 101} {
		if got, frozen := inj.hook(pid).lastTime(); !got.Equal(at) || !frozen {
			t.Errorf("pid %d: last = %v frozen=%v, want %v frozen", pid, got, frozen, at)
		}
	}
}

// A worker forked after Inject holds a private copy of the clock that nothing
// would ever update; SetTime must find and hook it.
func TestSetTimeHooksProcessesTheContainerStartedLater(t *testing.T) {
	inj := newFakeInjector()
	inj.setContainer(testContainer, 100)
	c, _ := startServerWith(t, inj)
	id := injectContainer(t, c, false)

	inj.setContainer(testContainer, 100, 200, 201) // two workers appear

	at := time.Unix(2_200_000_000, 0)
	if err := setTime(t, c, id, at, false); err != nil {
		t.Fatal(err)
	}
	for _, pid := range []int{200, 201} {
		h := inj.hook(pid)
		if h == nil {
			t.Fatalf("pid %d, started after Inject, was never hooked", pid)
		}
		if got, _ := h.lastTime(); !got.Equal(at) {
			t.Errorf("pid %d saw %v, want the new target %v", pid, got, at)
		}
	}
	st, _ := c.Status(context.Background(), &agentpb.StatusRequest{HandleId: id})
	if len(st.Pids) != 3 {
		t.Errorf("Status lists %v, want all three processes", st.Pids)
	}
	// A second update must not hook them again.
	before := len(inj.injections)
	if err := setTime(t, c, id, at.Add(time.Hour), false); err != nil {
		t.Fatal(err)
	}
	if len(inj.injections) != before {
		t.Errorf("already-hooked processes were injected again: %v", inj.injections[before:])
	}
}

func TestSetTimeDropsProcessesThatExited(t *testing.T) {
	inj := newFakeInjector()
	inj.setContainer(testContainer, 100, 101)
	c, _ := startServerWith(t, inj)
	id := injectContainer(t, c, false)

	inj.hook(101).setErr = unix.ESRCH // the worker exited
	inj.setContainer(testContainer, 100)

	if err := setTime(t, c, id, time.Unix(2_300_000_000, 0), false); err != nil {
		t.Fatalf("SetTime with one process gone: %v", err)
	}
	st, _ := c.Status(context.Background(), &agentpb.StatusRequest{HandleId: id})
	if len(st.Pids) != 1 || st.Pids[0] != 100 {
		t.Errorf("pids = %v, want just 100", st.Pids)
	}
}

// When every process has exited the container was restarted. NOT_FOUND tells
// the controller to inject it again, and the dead handle must be dropped.
func TestSetTimeWhenEveryProcessExitedIsNotFoundAndForgetsTheHandle(t *testing.T) {
	inj := newFakeInjector()
	inj.setContainer(testContainer, 100, 101)
	c, svc := startServerWith(t, inj)
	id := injectContainer(t, c, false)

	inj.hook(100).setErr = unix.ESRCH
	inj.hook(101).setErr = unix.ESRCH
	inj.setContainer(testContainer)

	wantCode(t, "SetTime with every process gone", setTime(t, c, id, time.Now(), false), codes.NotFound)
	svc.mu.RLock()
	_, still := svc.handles[id]
	svc.mu.RUnlock()
	if still {
		t.Error("the handle of a container with no processes left was kept")
	}
}

// A process that exec'd no longer carries the hooks; writing to its old state
// address would corrupt the new program. It has to be injected afresh.
func TestSetTimeReinjectsAProcessThatExecd(t *testing.T) {
	inj := newFakeInjector()
	inj.setContainer(testContainer, 100, 101)
	c, _ := startServerWith(t, inj)
	id := injectContainer(t, c, false)

	old := inj.hook(101)
	old.setErr = inject.ErrHooksLost
	before := len(inj.injections)

	at := time.Unix(2_400_000_000, 0)
	if err := setTime(t, c, id, at, false); err != nil {
		t.Fatalf("SetTime: %v", err)
	}
	if got := inj.injections[before:]; len(got) != 1 || got[0] != 101 {
		t.Fatalf("re-injected %v, want exactly pid 101", got)
	}
	if inj.hook(101) == old {
		t.Error("pid 101 still holds the stale hook")
	}
	if got, _ := inj.hook(101).lastTime(); !got.Equal(at) {
		t.Errorf("the re-injected process saw %v, want %v", got, at)
	}
}

func TestSetTimeFailsWhenNoProcessTakesTheChange(t *testing.T) {
	inj := newFakeInjector()
	inj.setContainer(testContainer, 100)
	c, svc := startServerWith(t, inj)
	id := injectContainer(t, c, false)

	inj.hook(100).setErr = errors.New("bad address")
	wantCode(t, "SetTime that landed nowhere", setTime(t, c, id, time.Now(), false), codes.Internal)

	svc.mu.RLock()
	_, kept := svc.handles[id]
	svc.mu.RUnlock()
	if !kept {
		t.Error("a handle whose process is alive but failing was dropped; the next call could succeed")
	}
}

func TestResetSetsEveryProcessToRealTime(t *testing.T) {
	inj := newFakeInjector()
	inj.setContainer(testContainer, 100, 101)
	c, _ := startServerWith(t, inj)
	id := injectContainer(t, c, true)

	before := time.Now()
	if _, err := c.Reset(context.Background(), &agentpb.ResetRequest{HandleId: id}); err != nil {
		t.Fatal(err)
	}
	for _, pid := range []int{100, 101} {
		got, frozen := inj.hook(pid).lastTime()
		if frozen || got.Before(before) || time.Since(got) > time.Minute {
			t.Errorf("pid %d after Reset: %v frozen=%v, want advancing real time", pid, got, frozen)
		}
	}
}

// ---------------------------------------------------------------------------
// Release
// ---------------------------------------------------------------------------

func TestReleaseUninstallsEverythingAndForgetsTheHandle(t *testing.T) {
	inj := newFakeInjector()
	inj.setContainer(testContainer, 100, 101)
	c, _ := startServerWith(t, inj)
	id := injectContainer(t, c, false)

	// A process forked from a hooked one carries a copy of the hook the agent
	// never tracked. Release must find it too.
	stray := &fakeHook{pid: 300}
	inj.mu.Lock()
	inj.hooks[300] = stray
	inj.mu.Unlock()
	inj.setContainer(testContainer, 100, 101, 300)

	h100, h101 := inj.hook(100), inj.hook(101)
	if _, err := c.Release(context.Background(), &agentpb.ReleaseRequest{HandleId: id}); err != nil {
		t.Fatalf("Release: %v", err)
	}
	for name, h := range map[string]*fakeHook{"100": h100, "101": h101, "the untracked stray": stray} {
		if !h.uninstalled {
			t.Errorf("pid %s still has its hook", name)
		}
	}

	_, err := c.Status(context.Background(), &agentpb.StatusRequest{HandleId: id})
	wantCode(t, "Status after Release", err, codes.NotFound)
	wantCode(t, "SetTime after Release", setTime(t, c, id, time.Now(), false), codes.NotFound)
	_, err = c.Release(context.Background(), &agentpb.ReleaseRequest{HandleId: id})
	wantCode(t, "a second Release", err, codes.NotFound)
}

// If one process cannot be uninstalled the handle must survive, holding just that
// process, so the caller can retry rather than lose track of a hooked process.
func TestReleaseKeepsTheHandleWhenAProcessCannotBeUninstalled(t *testing.T) {
	inj := newFakeInjector()
	inj.setContainer(testContainer, 100, 101)
	c, _ := startServerWith(t, inj)
	id := injectContainer(t, c, false)

	inj.hook(101).uninstallErr = errors.New("ptrace: operation not permitted")
	_, err := c.Release(context.Background(), &agentpb.ReleaseRequest{HandleId: id})
	wantCode(t, "Release with a stuck process", err, codes.Internal)

	st, err := c.Status(context.Background(), &agentpb.StatusRequest{HandleId: id})
	if err != nil {
		t.Fatalf("the handle was dropped although pid 101 still has its hook: %v", err)
	}
	if len(st.Pids) != 1 || st.Pids[0] != 101 {
		t.Errorf("pids = %v, want just the process that could not be uninstalled (101)", st.Pids)
	}

	inj.hook(101).uninstallErr = nil
	if _, err := c.Release(context.Background(), &agentpb.ReleaseRequest{HandleId: id}); err != nil {
		t.Fatalf("retrying Release: %v", err)
	}
}

func TestReleaseRequiresAHandle(t *testing.T) {
	c, _ := startServerWith(t, newFakeInjector())
	_, err := c.Release(context.Background(), &agentpb.ReleaseRequest{})
	wantCode(t, "Release with no handle", err, codes.InvalidArgument)
	_, err = c.Release(context.Background(), &agentpb.ReleaseRequest{HandleId: "unknown"})
	wantCode(t, "Release of an unknown handle", err, codes.NotFound)
}

// ---------------------------------------------------------------------------
// Authentication and configuration
// ---------------------------------------------------------------------------

func writeFile(t *testing.T, name, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestServerRequiresTheBearerToken(t *testing.T) {
	cfg := config{TokenFile: writeFile(t, "token", "s3cret\n")}
	opts, err := cfg.serverOptions(testLogger())
	if err != nil {
		t.Fatal(err)
	}
	inj := newFakeInjector()
	inj.setContainer(testContainer, 100)
	svc := newServerWith(testLogger(), inj)
	lis := bufconn.Listen(1 << 20)
	srv := grpc.NewServer(opts...)
	agentpb.RegisterAgentServiceServer(srv, svc)
	go srv.Serve(lis) //nolint:errcheck
	t.Cleanup(srv.Stop)

	dial := func(extra ...grpc.DialOption) agentpb.AgentServiceClient {
		conn, err := grpc.NewClient("passthrough://bufnet", append([]grpc.DialOption{
			grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }),
			grpc.WithTransportCredentials(insecure.NewCredentials()),
		}, extra...)...)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { conn.Close() })
		return agentpb.NewAgentServiceClient(conn)
	}
	inject := func(c agentpb.AgentServiceClient) error {
		_, err := c.Inject(context.Background(), &agentpb.InjectRequest{
			ContainerId: testContainer, TargetTime: timestamppb.New(time.Now())})
		return err
	}

	wantCode(t, "Inject with no token", inject(dial()), codes.Unauthenticated)
	wantCode(t, "Inject with the wrong token",
		inject(dial(grpc.WithPerRPCCredentials(auth.PerRPCCredentials("wrong", false)))), codes.Unauthenticated)
	if err := inject(dial(grpc.WithPerRPCCredentials(auth.PerRPCCredentials("s3cret", false)))); err != nil {
		t.Errorf("Inject with the right token: %v", err)
	}
	// The other methods are guarded too.
	anon := dial()
	_, err = anon.Release(context.Background(), &agentpb.ReleaseRequest{HandleId: "x"})
	wantCode(t, "Release with no token", err, codes.Unauthenticated)
	_, err = anon.Reset(context.Background(), &agentpb.ResetRequest{HandleId: "x"})
	wantCode(t, "Reset with no token", err, codes.Unauthenticated)
}

func TestConfigRefusesToStartOpen(t *testing.T) {
	log := testLogger()
	token := writeFile(t, "token", "tok")
	emptyToken := writeFile(t, "empty", "\n")

	for _, c := range []struct {
		name    string
		cfg     config
		wantErr bool
	}{
		{"nothing configured", config{}, true},
		{"explicitly unauthenticated", config{AllowUnauthenticated: true}, false},
		{"token", config{TokenFile: token}, false},
		{"an empty token file is not 'no auth'", config{TokenFile: emptyToken}, true},
		{"a missing token file", config{TokenFile: filepath.Join(t.TempDir(), "absent")}, true},
		{"cert without key", config{TLSCertFile: "c.pem", AllowUnauthenticated: true}, true},
		{"key without cert", config{TLSKeyFile: "k.pem", AllowUnauthenticated: true}, true},
		{"client CA without a server certificate", config{TLSClientCAFile: "ca.pem"}, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, err := c.cfg.serverOptions(log)
			if (err != nil) != c.wantErr {
				t.Errorf("serverOptions error = %v, want error: %v", err, c.wantErr)
			}
		})
	}
}
