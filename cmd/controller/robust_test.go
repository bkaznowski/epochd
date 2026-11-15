package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bkaznowski/epochd/pkg/api"
	applog "github.com/bkaznowski/epochd/pkg/log"

	"google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

// Tests for how the controller behaves when things go wrong or happen at once:
// agents that fail, requests that overlap, containers that vanish.

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// podOf builds a running pod labelled app=<app> with the given containers, each
// "name=containerID".
func podOf(name, app string, containers ...string) corev1.Pod {
	p := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default", Labels: map[string]string{"app": app}},
		Status:     corev1.PodStatus{Phase: corev1.PodRunning, HostIP: "10.0.0." + fmt.Sprint(len(name))},
	}
	for _, c := range containers {
		n, id, _ := strings.Cut(c, "=")
		p.Status.ContainerStatuses = append(p.Status.ContainerStatuses, corev1.ContainerStatus{
			Name: n, ContainerID: id, State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
		})
	}
	return p
}

func newestController(t *testing.T, pods ...corev1.Pod) (*controller, *mockAgentPool, *fake.Clientset) {
	t.Helper()
	k8s := fake.NewClientset()
	for i := range pods {
		if _, err := k8s.CoreV1().Pods(pods[i].Namespace).Create(context.Background(), &pods[i], metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	pool := &mockAgentPool{}
	ctrl := newController(k8s, pool, nil, applog.Discard())
	ctrl.concurrency = 1
	return ctrl, pool, k8s
}

func do(t *testing.T, ctrl *controller, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	rec := httptest.NewRecorder()
	ctrl.routes().ServeHTTP(rec, req)
	return rec
}

func mustCreate(t *testing.T, ctrl *controller, selector string, freeze bool) *timeshift {
	t.Helper()
	s, err := ctrl.createTimeshift(context.Background(), "default", selector, time.Now().Add(24*time.Hour), 0, freeze)
	if err != nil {
		t.Fatalf("createTimeshift: %v", err)
	}
	return s
}

func notFound(msg string) error { return grpcstatus.Error(codes.NotFound, msg) }

// ---------------------------------------------------------------------------
// Updates
// ---------------------------------------------------------------------------

// Advancing a frozen timeshift by a duration (what the SDK and CLI send, with no
// freeze field) used to thaw it: the handler passed freeze=false to the agents.
func TestAdvanceKeepsAFrozenClockFrozen(t *testing.T) {
	ctrl, pool, _ := newestController(t, podOf("web-1", "web", "main=aaaaaaaa1111"))
	s := mustCreate(t, ctrl, "app=web", true)

	var sawFreeze []bool
	pool.setTimeFn = func(_ context.Context, _, _ string, _ time.Time, freeze bool) error {
		sawFreeze = append(sawFreeze, freeze)
		return nil
	}

	rec := do(t, ctrl, http.MethodPatch, "/timeshifts/"+s.id, `{"duration":"1h"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("PATCH = %d: %s", rec.Code, rec.Body)
	}
	var resp api.TimeshiftResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if !resp.Frozen {
		t.Error("the response says the clock is no longer frozen")
	}
	if len(sawFreeze) != 1 || !sawFreeze[0] {
		t.Errorf("the agent was told freeze=%v, want [true]", sawFreeze)
	}
	after, _ := ctrl.getTimeshift(s.id)
	if !after.frozen {
		t.Error("the registry thawed the timeshift")
	}
}

func TestAdvanceKeepsARunningClockRunningUnlessAskedToFreeze(t *testing.T) {
	ctrl, pool, _ := newestController(t, podOf("web-1", "web", "main=aaaaaaaa1111"))
	s := mustCreate(t, ctrl, "app=web", false)

	var sawFreeze []bool
	pool.setTimeFn = func(_ context.Context, _, _ string, _ time.Time, freeze bool) error {
		sawFreeze = append(sawFreeze, freeze)
		return nil
	}
	if rec := do(t, ctrl, http.MethodPatch, "/timeshifts/"+s.id, `{"duration":"1h"}`); rec.Code != http.StatusOK {
		t.Fatalf("PATCH = %d", rec.Code)
	}
	if rec := do(t, ctrl, http.MethodPatch, "/timeshifts/"+s.id, `{"duration":"1h","freeze":true}`); rec.Code != http.StatusOK {
		t.Fatalf("PATCH freeze = %d", rec.Code)
	}
	if len(sawFreeze) != 2 || sawFreeze[0] || !sawFreeze[1] {
		t.Errorf("freeze flags = %v, want [false true]", sawFreeze)
	}
}

func TestUpdateFailureIsReportedAsBadGateway(t *testing.T) {
	ctrl, pool, _ := newestController(t, podOf("web-1", "web", "main=aaaaaaaa1111"))
	s := mustCreate(t, ctrl, "app=web", false)
	pool.setTimeFn = func(context.Context, string, string, time.Time, bool) error { return errors.New("node unreachable") }

	rec := do(t, ctrl, http.MethodPatch, "/timeshifts/"+s.id, `{"duration":"1h"}`)
	if rec.Code != http.StatusBadGateway {
		t.Errorf("PATCH when no container accepts it = %d, want 502: %s", rec.Code, rec.Body)
	}
}

func TestUpdateReportsContainersItMissedAsWarnings(t *testing.T) {
	ctrl, pool, _ := newestController(t,
		podOf("web-1", "web", "main=aaaaaaaa1111"),
		podOf("web-22", "web", "main=bbbbbbbb2222"))
	s := mustCreate(t, ctrl, "app=web", false)

	pool.setTimeFn = func(_ context.Context, nodeIP, _ string, _ time.Time, _ bool) error {
		if nodeIP == "10.0.0.5" { // web-1
			return errors.New("node unreachable")
		}
		return nil
	}
	rec := do(t, ctrl, http.MethodPatch, "/timeshifts/"+s.id, `{"duration":"1h"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("a partly successful update = %d, want 200: %s", rec.Code, rec.Body)
	}
	var resp api.TimeshiftResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Warnings) != 1 || !strings.Contains(resp.Warnings[0], "web-1/main") {
		t.Errorf("warnings = %v, want one naming web-1/main", resp.Warnings)
	}
	// A later read must not carry the warning along.
	if again, _ := ctrl.getTimeshift(s.id); len(again.warnings) != 0 {
		t.Errorf("a stored timeshift kept the one-off warnings %v", again.warnings)
	}
}

// ---------------------------------------------------------------------------
// Deleting and expiring: never lose track of a container that is still faked
// ---------------------------------------------------------------------------

func TestDeleteKeepsTheTimeshiftWhenAContainerCannotBeReset(t *testing.T) {
	ctrl, pool, _ := newestController(t,
		podOf("web-1", "web", "main=aaaaaaaa1111"),
		podOf("web-22", "web", "main=bbbbbbbb2222"))
	s := mustCreate(t, ctrl, "app=web", false)

	failing := true
	pool.resetFn = func(_ context.Context, nodeIP, _ string) error {
		if nodeIP == "10.0.0.5" && failing { // web-1's node is down
			return errors.New("connection refused")
		}
		return nil
	}

	rec := do(t, ctrl, http.MethodDelete, "/timeshifts/"+s.id, "")
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("DELETE with a container that cannot be reset = %d, want 502: %s", rec.Code, rec.Body)
	}
	kept, err := ctrl.getTimeshift(s.id)
	if err != nil {
		t.Fatalf("the timeshift was dropped although web-1 is still on a fake clock: %v", err)
	}
	if len(kept.handles) != 1 || kept.handles[0].pod != "web-1" {
		t.Errorf("kept handles = %+v, want just web-1 (web-22 was reset)", kept.handles)
	}

	failing = false // the node came back
	if rec := do(t, ctrl, http.MethodDelete, "/timeshifts/"+s.id, ""); rec.Code != http.StatusNoContent {
		t.Fatalf("retrying DELETE = %d: %s", rec.Code, rec.Body)
	}
	if _, err := ctrl.getTimeshift(s.id); err == nil {
		t.Error("the timeshift is still registered after a successful retry")
	}
}

// The TTL is the safety net for a forgotten timeshift. An expired one whose reset
// fails must stay and be retried, not vanish with its container still faked.
func TestSweepRetriesAnExpiredTimeshiftWhoseResetFails(t *testing.T) {
	ctrl, pool, _ := newestController(t, podOf("web-1", "web", "main=aaaaaaaa1111"))
	s, err := ctrl.createTimeshift(context.Background(), "default", "app=web", time.Now().Add(time.Hour), time.Minute, false)
	if err != nil {
		t.Fatal(err)
	}
	ctrl.mu.Lock()
	ctrl.timeshifts[s.id].expiresAt = time.Now().Add(-time.Second)
	ctrl.mu.Unlock()

	var resets atomic.Int32
	failing := true
	pool.resetFn = func(context.Context, string, string) error {
		resets.Add(1)
		if failing {
			return errors.New("connection refused")
		}
		return nil
	}

	ctrl.sweepExpired(context.Background())
	if _, err := ctrl.getTimeshift(s.id); err != nil {
		t.Fatal("an expired timeshift was dropped although its container is still faked")
	}

	failing = false
	ctrl.sweepExpired(context.Background())
	if _, err := ctrl.getTimeshift(s.id); err == nil {
		t.Error("the timeshift stayed after the reset finally succeeded")
	}
	if resets.Load() != 2 {
		t.Errorf("resets = %d, want 2 (one failed, one retried)", resets.Load())
	}
}

// If the container is gone there is nothing left to reset, so a reset that fails
// because its pod was deleted must not keep a timeshift alive for ever.
func TestResetOfAContainerThatNoLongerExistsCountsAsDone(t *testing.T) {
	for _, c := range []struct {
		name   string
		change func(t *testing.T, k8s *fake.Clientset)
	}{
		{"its pod was deleted", func(t *testing.T, k8s *fake.Clientset) {
			if err := k8s.CoreV1().Pods("default").Delete(context.Background(), "web-1", metav1.DeleteOptions{}); err != nil {
				t.Fatal(err)
			}
		}},
		{"it restarted under a new container id", func(t *testing.T, k8s *fake.Clientset) {
			p, _ := k8s.CoreV1().Pods("default").Get(context.Background(), "web-1", metav1.GetOptions{})
			p.Status.ContainerStatuses[0].ContainerID = "containerd://cccccccc3333"
			if _, err := k8s.CoreV1().Pods("default").Update(context.Background(), p, metav1.UpdateOptions{}); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			ctrl, pool, k8s := newestController(t, podOf("web-1", "web", "main=aaaaaaaa1111"))
			s := mustCreate(t, ctrl, "app=web", false)
			pool.resetFn = func(context.Context, string, string) error { return errors.New("connection refused") }
			c.change(t, k8s)

			if err := ctrl.deleteTimeshift(context.Background(), s.id); err != nil {
				t.Errorf("deleteTimeshift = %v, want success: the container is gone", err)
			}
		})
	}
}

// After an agent crash the hooks are still in the processes with their old
// offset. NOT_FOUND on Reset must not be read as "already real time": the
// controller takes the container over again and resets it.
func TestResetNotFoundTakesTheContainerOverInsteadOfAssumingItIsReset(t *testing.T) {
	ctrl, pool, _ := newestController(t, podOf("web-1", "web", "main=aaaaaaaa1111"))
	s := mustCreate(t, ctrl, "app=web", false)

	pool.resetFn = func(_ context.Context, _, handle string) error {
		if handle != "handle-aaaaaaaa" { // the original handle is the one the agent forgot
			return nil
		}
		return notFound("unknown handle")
	}
	var injected struct {
		n      int
		target time.Time
		freeze bool
	}
	pool.injectFn = func(_ context.Context, _, _ string, target time.Time, freeze bool) (string, error) {
		injected.n++
		injected.target, injected.freeze = target, freeze
		return "fresh-handle", nil
	}
	var released []string
	pool.releaseFn = func(_ context.Context, _, handle string) error {
		released = append(released, handle)
		return nil
	}

	if err := ctrl.deleteTimeshift(context.Background(), s.id); err != nil {
		t.Fatalf("deleteTimeshift: %v", err)
	}
	if injected.n != 1 || injected.freeze || time.Since(injected.target).Abs() > 5*time.Second {
		t.Errorf("take-over inject = %+v, want one injection at ~now, advancing", injected)
	}
	if len(released) != 1 || released[0] != "fresh-handle" {
		t.Errorf("released %v, want the replacement handle only", released)
	}
}

func TestResetNotFoundForAContainerThatIsGoneIsDone(t *testing.T) {
	ctrl, pool, _ := newestController(t, podOf("web-1", "web", "main=aaaaaaaa1111"))
	s := mustCreate(t, ctrl, "app=web", false)
	pool.resetFn = func(context.Context, string, string) error { return notFound("unknown handle") }
	pool.injectFn = func(context.Context, string, string, time.Time, bool) (string, error) {
		return "", notFound("no such container")
	}
	if err := ctrl.deleteTimeshift(context.Background(), s.id); err != nil {
		t.Errorf("deleteTimeshift = %v, want success when the container no longer exists", err)
	}
}

// Taking the hook out is performance cleanup; the clock is already real by then,
// so its failure must not keep a timeshift alive.
func TestReleaseFailureDoesNotFailTheDelete(t *testing.T) {
	for _, relErr := range []error{
		errors.New("ptrace: operation not permitted"),
		grpcstatus.Error(codes.Unimplemented, "an older agent"),
	} {
		ctrl, pool, _ := newestController(t, podOf("web-1", "web", "main=aaaaaaaa1111"))
		s := mustCreate(t, ctrl, "app=web", false)
		var resets int
		pool.resetFn = func(context.Context, string, string) error { resets++; return nil }
		pool.releaseFn = func(context.Context, string, string) error { return relErr }

		if err := ctrl.deleteTimeshift(context.Background(), s.id); err != nil {
			t.Errorf("deleteTimeshift with Release failing (%v) = %v, want success", relErr, err)
		}
		if resets != 1 {
			t.Errorf("Reset ran %d times, want 1", resets)
		}
	}
}

// ---------------------------------------------------------------------------
// Concurrency
// ---------------------------------------------------------------------------

// Reading a timeshift while another request updates it was a data race.
func TestConcurrentReadsAndUpdatesAreRaceFree(t *testing.T) {
	ctrl, _, _ := newestController(t, podOf("web-1", "web", "main=aaaaaaaa1111"))
	s := mustCreate(t, ctrl, "app=web", false)

	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				ctrl.updateTimeshift(context.Background(), s.id, time.Now().Add(time.Duration(j)*time.Minute), j%2 == 0) //nolint:errcheck
			}
		}()
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				ctrl.listTimeshifts()
				if got, err := ctrl.getTimeshift(s.id); err == nil {
					_ = got.toResponse()
				}
				do(t, ctrl, http.MethodGet, "/timeshifts/"+s.id, "")
			}
		}()
	}
	wg.Wait()
}

// Two creates for overlapping selectors must not both claim a container.
func TestConcurrentCreatesCannotClaimTheSameContainer(t *testing.T) {
	ctrl, pool, _ := newestController(t, podOf("web-1", "web", "main=aaaaaaaa1111"))

	entered := make(chan struct{})
	proceed := make(chan struct{})
	var injections atomic.Int32
	pool.injectFn = func(context.Context, string, string, time.Time, bool) (string, error) {
		if injections.Add(1) == 1 {
			close(entered)
			<-proceed // hold the first create inside its injection
		}
		return "h", nil
	}

	first := make(chan error, 1)
	go func() {
		_, err := ctrl.createTimeshift(context.Background(), "default", "app=web", time.Now().Add(time.Hour), 0, false)
		first <- err
	}()
	<-entered

	_, err := ctrl.createTimeshift(context.Background(), "default", "app=web", time.Now().Add(time.Hour), 0, false)
	var conflict *conflictError
	if !errors.As(err, &conflict) {
		t.Errorf("a second create while the first is injecting = %v, want a conflict", err)
	} else if msg := conflict.Error(); !strings.Contains(msg, "web-1/main") {
		t.Errorf("conflict message %q does not name the container", msg)
	}

	close(proceed)
	if err := <-first; err != nil {
		t.Fatalf("the first create failed: %v", err)
	}
	if n := injections.Load(); n != 1 {
		t.Errorf("the container was injected %d times, want exactly 1", n)
	}
	// The claim is released once the create is done, so the container is still
	// reported as taken by the timeshift, not by a stale reservation.
	if len(ctrl.reserved) != 0 {
		t.Errorf("reservations left behind: %v", ctrl.reserved)
	}
}

func TestFailedCreateReleasesItsReservation(t *testing.T) {
	ctrl, pool, _ := newestController(t, podOf("web-1", "web", "main=aaaaaaaa1111"))
	pool.injectFn = func(context.Context, string, string, time.Time, bool) (string, error) {
		return "", errors.New("agent down")
	}
	if _, err := ctrl.createTimeshift(context.Background(), "default", "app=web", time.Now().Add(time.Hour), 0, false); err == nil {
		t.Fatal("expected the create to fail")
	}
	pool.injectFn = nil
	if _, err := ctrl.createTimeshift(context.Background(), "default", "app=web", time.Now().Add(time.Hour), 0, false); err != nil {
		t.Errorf("retrying after a failed create: %v (the reservation was never released)", err)
	}
}

// A client that hangs up mid-create must not leave containers injected under no
// timeshift at all.
func TestCreateUndoesItsInjectionsWhenTheCallerGivesUp(t *testing.T) {
	ctrl, pool, _ := newestController(t,
		podOf("web-1", "web", "main=aaaaaaaa1111"),
		podOf("web-22", "web", "main=bbbbbbbb2222"))

	ctx, cancel := context.WithCancel(context.Background())
	pool.injectFn = func(_ context.Context, _, containerID string, _ time.Time, _ bool) (string, error) {
		cancel() // the client disconnects as the first injection lands
		return "h-" + containerID[len(containerID)-4:], nil
	}
	var resets, releases atomic.Int32
	pool.resetFn = func(context.Context, string, string) error { resets.Add(1); return nil }
	pool.releaseFn = func(context.Context, string, string) error { releases.Add(1); return nil }

	_, err := ctrl.createTimeshift(ctx, "default", "app=web", time.Now().Add(time.Hour), 0, false)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("createTimeshift = %v, want context.Canceled", err)
	}
	if got := len(ctrl.listTimeshifts()); got != 0 {
		t.Errorf("%d timeshifts registered after an abandoned create", got)
	}
	if resets.Load() == 0 || releases.Load() == 0 {
		t.Errorf("abandoned injections were not undone (resets=%d releases=%d)", resets.Load(), releases.Load())
	}
}

// Deleting while an update is still re-faking a container could leave that
// container faked with nothing recording it. The two must not interleave.
func TestDeleteWaitsForAnUpdateInFlight(t *testing.T) {
	ctrl, pool, _ := newestController(t, podOf("web-1", "web", "main=aaaaaaaa1111"))
	s := mustCreate(t, ctrl, "app=web", false)

	inSetTime := make(chan struct{})
	finishSetTime := make(chan struct{})
	var mu sync.Mutex
	var order []string
	pool.setTimeFn = func(context.Context, string, string, time.Time, bool) error {
		close(inSetTime)
		<-finishSetTime
		mu.Lock()
		order = append(order, "settime-done")
		mu.Unlock()
		return nil
	}
	pool.resetFn = func(context.Context, string, string) error {
		mu.Lock()
		order = append(order, "reset")
		mu.Unlock()
		return nil
	}

	updated := make(chan error, 1)
	go func() {
		_, err := ctrl.updateTimeshift(context.Background(), s.id, time.Now().Add(time.Hour), false)
		updated <- err
	}()
	<-inSetTime

	deleted := make(chan error, 1)
	go func() { deleted <- ctrl.deleteTimeshift(context.Background(), s.id) }()

	select {
	case <-deleted:
		t.Fatal("delete finished while an update was still talking to the agent")
	case <-time.After(150 * time.Millisecond):
	}
	close(finishSetTime)
	if err := <-updated; err != nil {
		t.Fatal(err)
	}
	if err := <-deleted; err != nil {
		t.Fatal(err)
	}
	if want := "settime-done,reset"; strings.Join(order, ",") != want {
		t.Errorf("order = %v, want the update to finish before the reset", order)
	}
}

func TestSweepLeavesATimeshiftAloneWhileAnOperationIsInFlight(t *testing.T) {
	ctrl, pool, _ := newestController(t, podOf("web-1", "web", "main=aaaaaaaa1111"))
	s, err := ctrl.createTimeshift(context.Background(), "default", "app=web", time.Now().Add(time.Hour), time.Minute, false)
	if err != nil {
		t.Fatal(err)
	}
	ctrl.mu.Lock()
	ctrl.timeshifts[s.id].expiresAt = time.Now().Add(-time.Second)
	reg := ctrl.timeshifts[s.id]
	ctrl.mu.Unlock()

	var resets atomic.Int32
	pool.resetFn = func(context.Context, string, string) error { resets.Add(1); return nil }

	reg.op.Lock() // an update or delete is in flight
	done := make(chan struct{})
	go func() { ctrl.sweepExpired(context.Background()); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("the sweep blocked on a timeshift that is busy")
	}
	reg.op.Unlock()
	if resets.Load() != 0 {
		t.Errorf("the sweep reset a timeshift another operation was working on")
	}
}

// Agent calls within one request run in parallel, but never more than allowed.
func TestAgentCallsRunInParallelUpToTheLimit(t *testing.T) {
	var pods []corev1.Pod
	for i := 0; i < 12; i++ {
		pods = append(pods, podOf(fmt.Sprintf("web-%02d", i), "web", fmt.Sprintf("main=%012d", i)))
	}
	ctrl, pool, _ := newestController(t, pods...)
	ctrl.concurrency = 4

	var inFlight, peak atomic.Int32
	pool.injectFn = func(context.Context, string, string, time.Time, bool) (string, error) {
		n := inFlight.Add(1)
		for {
			p := peak.Load()
			if n <= p || peak.CompareAndSwap(p, n) {
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
		inFlight.Add(-1)
		return "h", nil
	}
	s := mustCreate(t, ctrl, "app=web", false)

	if len(s.handles) != 12 {
		t.Fatalf("%d handles, want 12", len(s.handles))
	}
	if p := peak.Load(); p < 2 || p > 4 {
		t.Errorf("peak concurrent injections = %d, want between 2 and the limit of 4", p)
	}
}

// Concurrent persists must not reach the store out of order, or a stale
// snapshot can overwrite a newer one.
func TestPersistWritesAreSerialised(t *testing.T) {
	k8s := fake.NewClientset()
	var inFlight, peak atomic.Int32
	k8s.PrependReactor("update", "configmaps", func(k8stesting.Action) (bool, runtime.Object, error) {
		n := inFlight.Add(1)
		if n > peak.Load() {
			peak.Store(n)
		}
		time.Sleep(5 * time.Millisecond)
		inFlight.Add(-1)
		return false, nil, nil
	})
	if _, err := k8s.CoreV1().ConfigMaps("epochd").Create(context.Background(), &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: storeConfigMapName, Namespace: "epochd"},
	}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	ctrl := newController(k8s, &mockAgentPool{}, newStore(k8s, "epochd"), applog.Discard())

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); ctrl.persist(context.Background()) }()
	}
	wg.Wait()
	if p := peak.Load(); p != 1 {
		t.Errorf("up to %d store writes ran at once; they must be serialised", p)
	}
}

// A persist must still happen when the request that caused it has been cancelled:
// the change is already in memory.
func TestPersistSurvivesACancelledRequest(t *testing.T) {
	k8s := fake.NewClientset()
	ctrl := newController(k8s, &mockAgentPool{}, newStore(k8s, "epochd"), applog.Discard())
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	ctrl.persist(ctx)
	if _, err := k8s.CoreV1().ConfigMaps("epochd").Get(context.Background(), storeConfigMapName, metav1.GetOptions{}); err != nil {
		t.Errorf("nothing was persisted for a cancelled request: %v", err)
	}
}

// ---------------------------------------------------------------------------
// The pod watcher
// ---------------------------------------------------------------------------

func TestWatcherInjectsOnlyTheContainersThatNeedIt(t *testing.T) {
	pod := podOf("web-1", "web", "app=aaaaaaaa1111", "sidecar=bbbbbbbb2222")
	ctrl, pool, k8s := newestController(t, pod)
	s := mustCreate(t, ctrl, "app=web", false)
	_ = s

	// The app container restarts; the sidecar does not.
	restarted := pod.DeepCopy()
	restarted.Status.ContainerStatuses[0].ContainerID = "containerd://cccccccc3333"
	if _, err := k8s.CoreV1().Pods("default").Update(context.Background(), restarted, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}

	var injected []string
	pool.injectFn = func(_ context.Context, _, containerID string, _ time.Time, _ bool) (string, error) {
		injected = append(injected, containerID)
		return "new-" + containerID[len(containerID)-4:], nil
	}
	var released []string
	pool.releaseFn = func(_ context.Context, _, handle string) error { released = append(released, handle); return nil }

	ctrl.handlePodEvent(context.Background(), restarted)

	if len(injected) != 1 || injected[0] != "containerd://cccccccc3333" {
		t.Errorf("injected %v, want only the restarted container", injected)
	}
	got, _ := ctrl.getTimeshift(s.id)
	if len(got.handles) != 2 {
		t.Fatalf("%d handles, want 2 (the restarted one replaced, the sidecar kept)", len(got.handles))
	}
	names := map[string]string{}
	for _, h := range got.handles {
		names[h.container] = h.containerID
	}
	if names["app"] != "containerd://cccccccc3333" || names["sidecar"] != "bbbbbbbb2222" {
		t.Errorf("handles = %v", names)
	}
	if len(released) != 1 {
		t.Errorf("released %v, want the dead container's old handle freed", released)
	}
}

func TestWatcherDropsADeletedPod(t *testing.T) {
	ctrl, pool, _ := newestController(t,
		podOf("web-1", "web", "main=aaaaaaaa1111"),
		podOf("web-22", "web", "main=bbbbbbbb2222"))
	s := mustCreate(t, ctrl, "app=web", false)

	var released atomic.Int32
	pool.releaseFn = func(context.Context, string, string) error { released.Add(1); return nil }

	ctrl.handlePodDeleted(context.Background(), "default", "web-1")

	got, _ := ctrl.getTimeshift(s.id)
	if len(got.handles) != 1 || got.handles[0].pod != "web-22" {
		t.Errorf("handles = %+v, want just web-22", got.handles)
	}
	if want := []string{"web-22/main"}; strings.Join(got.appliedTo(), ",") != strings.Join(want, ",") {
		t.Errorf("appliedTo = %v, want %v", got.appliedTo(), want)
	}
	if released.Load() != 1 {
		t.Errorf("the dead pod's agent handle was released %d times, want 1", released.Load())
	}
	// The timeshift itself survives: a replacement pod will be picked up.
	if _, err := ctrl.getTimeshift(s.id); err != nil {
		t.Error("the timeshift was removed along with one pod")
	}
}

func TestPodQueueDeduplicatesAndDrains(t *testing.T) {
	q := newPodQueue()
	for _, k := range []string{"ns/a", "ns/b", "ns/a", "ns/a", "ns/c"} {
		q.add(k)
	}
	var got []string
	for i := 0; i < 3; i++ {
		k, ok := q.next()
		if !ok {
			t.Fatal("queue ended early")
		}
		got = append(got, k)
	}
	if strings.Join(got, ",") != "ns/a,ns/b,ns/c" {
		t.Errorf("got %v, want each key once, in arrival order", got)
	}
	// A key arriving while it is being processed is queued again.
	q.add("ns/a")
	if k, _ := q.next(); k != "ns/a" {
		t.Errorf("re-added key = %q", k)
	}

	q.close()
	if _, ok := q.next(); ok {
		t.Error("next on a closed, empty queue should report false")
	}
	q.add("ns/z") // ignored once closed
	if _, ok := q.next(); ok {
		t.Error("a closed queue accepted work")
	}
}

func TestPodQueueNextBlocksUntilWorkArrives(t *testing.T) {
	q := newPodQueue()
	got := make(chan string, 1)
	go func() { k, _ := q.next(); got <- k }()
	select {
	case <-got:
		t.Fatal("next returned with nothing queued")
	case <-time.After(50 * time.Millisecond):
	}
	q.add("ns/x")
	select {
	case k := <-got:
		if k != "ns/x" {
			t.Errorf("got %q", k)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("next did not wake when work arrived")
	}
}

// ---------------------------------------------------------------------------
// The HTTP layer
// ---------------------------------------------------------------------------

func TestRequestBodiesAreCapped(t *testing.T) {
	ctrl, _, _ := newestController(t)
	big := `{"namespace":"default","labelSelector":"` + strings.Repeat("a", maxRequestBody+10) + `"}`
	rec := do(t, ctrl, http.MethodPost, "/timeshifts", big)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("an over-size body = %d, want 413", rec.Code)
	}
	rec = do(t, ctrl, http.MethodPost, "/timeshifts", "{not json")
	if rec.Code != http.StatusBadRequest {
		t.Errorf("malformed JSON = %d, want 400", rec.Code)
	}
}

func TestAPIRequiresAuthenticationUnlessToldOtherwise(t *testing.T) {
	log := applog.Discard()
	ok := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	tokenFile := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenFile, []byte("s3cret\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := (apiConfig{}).wrap(ok, log); err == nil {
		t.Error("the API started with no authentication configured and no explicit opt-out")
	}
	if _, err := (apiConfig{TokenFile: filepath.Join(t.TempDir(), "absent")}).wrap(ok, log); err == nil {
		t.Error("a missing token file must be an error, not an open API")
	}
	if h, err := (apiConfig{AllowUnauthenticated: true}).wrap(ok, log); err != nil {
		t.Errorf("explicit opt-out refused: %v", err)
	} else if rec := serve(h, "/timeshifts", ""); rec.Code != http.StatusNoContent {
		t.Errorf("opted-out API = %d", rec.Code)
	}

	h, err := (apiConfig{TokenFile: tokenFile}).wrap(ok, log)
	if err != nil {
		t.Fatal(err)
	}
	if rec := serve(h, "/timeshifts", ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("no token = %d, want 401", rec.Code)
	}
	if rec := serve(h, "/timeshifts", "Bearer wrong"); rec.Code != http.StatusUnauthorized {
		t.Errorf("wrong token = %d, want 401", rec.Code)
	}
	if rec := serve(h, "/timeshifts", "Bearer s3cret"); rec.Code != http.StatusNoContent {
		t.Errorf("right token = %d, want it served", rec.Code)
	}
	for _, p := range []string{"/healthz", "/metrics"} {
		if rec := serve(h, p, ""); rec.Code != http.StatusNoContent {
			t.Errorf("%s needs no token, got %d", p, rec.Code)
		}
	}
}

func serve(h http.Handler, path, authorization string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, path, bytes.NewReader(nil))
	if authorization != "" {
		req.Header.Set("Authorization", authorization)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// shortID must never panic on a malformed id read back from the store.
func TestShortIDIsSafeOnShortInput(t *testing.T) {
	for in, want := range map[string]string{"": "", "abc": "abc", "12345678": "12345678", "123456789abc": "12345678"} {
		if got := shortID(in); got != want {
			t.Errorf("shortID(%q) = %q, want %q", in, got, want)
		}
	}
}
