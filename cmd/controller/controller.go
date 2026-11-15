// Package main implements the epochd controller: an HTTP+JSON service that
// resolves pods via the Kubernetes API and orchestrates clock injection via
// per-node gRPC agents.
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/bkaznowski/epochd/pkg/api"

	"google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"
	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/record"
)

// AgentPool abstracts per-node gRPC connections to the epochd agent.
// agentclient.Pool satisfies this interface; tests use a mock.
type AgentPool interface {
	Inject(ctx context.Context, nodeIP, containerID string, target time.Time, freeze bool) (string, error)
	SetTime(ctx context.Context, nodeIP, handleID string, target time.Time, freeze bool) error
	Reset(ctx context.Context, nodeIP, handleID string) error
	// Release takes the clock hook out of the container and forgets the handle.
	Release(ctx context.Context, nodeIP, handleID string) error
	GetStatus(ctx context.Context, nodeIP, handleID string) (*api.HandleStatus, error)
}

// defaultConcurrency bounds how many agent calls one request makes at once.
const defaultConcurrency = 8

// maxResetFailureEvents caps the warning Events posted for one timeshift whose
// reset keeps failing, so a node that stays down does not flood the cluster.
const maxResetFailureEvents = 1

// containerHandle records the agent-issued handle for one injected container.
type containerHandle struct {
	pod         string // pod name (namespace lives on the parent timeshift)
	container   string
	nodeIP      string
	containerID string // CRI container ID — used to re-inject after an agent restart
	agentHandle string // opaque ID returned by the agent's Inject RPC
}

// timeshift is one active time-timeshift entry in the registry.
//
// The registry owns the only mutable copy: callers outside the controller
// work on snapshots from clone, because the fields below change under c.mu while
// requests are being served.
type timeshift struct {
	id            string
	namespace     string
	labelSelector string
	// For advancing mode: fake_time = time.Now() + offset.
	// For frozen mode: fake_time = frozenAt (constant).
	offset    time.Duration
	frozenAt  time.Time
	frozen    bool
	ttl       time.Duration
	expiresAt time.Time
	createdAt time.Time
	handles   []containerHandle

	// resetFailures counts sweeps (or deletes) that could not reset every
	// container and left the timeshift in place to be retried.
	resetFailures int

	// warnings lists containers the update that produced this snapshot did not
	// reach. It exists only on snapshots returned by updateTimeshift and is never
	// stored.
	warnings []string

	// op serialises the operations that talk to agents on behalf of this
	// timeshift: update, delete, expiry and pod events. Without it a delete could
	// reset every container while an update was still re-faking one of them,
	// leaving a clock nobody recorded. Always taken before c.mu, never while
	// holding it. It is shared by every snapshot of the same timeshift.
	op *sync.Mutex
}

// newTimeshift returns a timeshift with its operation lock allocated.
func newTimeshift() *timeshift { return &timeshift{op: new(sync.Mutex)} }

// clone returns a snapshot that is safe to read without holding c.mu. The
// caller must hold c.mu (read is enough).
func (s *timeshift) clone() *timeshift {
	cp := *s
	cp.handles = append([]containerHandle(nil), s.handles...)
	cp.warnings = append([]string(nil), s.warnings...)
	return &cp
}

// effectiveTime returns the fake time the targeted processes currently see.
// For advancing timeshifts this grows in real time; for frozen ones it is constant.
func (s *timeshift) effectiveTime() time.Time {
	if s.frozen {
		return s.frozenAt
	}
	return time.Now().Add(s.offset)
}

func (s *timeshift) appliedTo() []string {
	out := make([]string, len(s.handles))
	for i, h := range s.handles {
		out[i] = h.pod + "/" + h.container
	}
	sort.Strings(out)
	return out
}

func (s *timeshift) toResponse() api.TimeshiftResponse {
	r := api.TimeshiftResponse{
		ID:        s.id,
		Namespace: s.namespace,
		Time:      s.effectiveTime().UTC().Format(time.RFC3339),
		Frozen:    s.frozen,
		AppliedTo: s.appliedTo(),
		Warnings:  s.warnings,
	}
	if !s.expiresAt.IsZero() {
		r.ExpiresAt = s.expiresAt.UTC().Format(time.RFC3339)
	}
	return r
}

// controller is the HTTP server's state.
type controller struct {
	k8s        kubernetes.Interface
	agents     AgentPool
	store      *store
	mu         sync.RWMutex
	timeshifts map[string]*timeshift
	// reserved holds containers a createTimeshift is in the middle of injecting, so
	// two concurrent creates cannot both claim the same one.
	reserved map[string]bool
	// persistMu serialises writes of the registry to the store. Without it two
	// concurrent persists can reach the ConfigMap out of order, and a stale
	// snapshot overwrites a newer one.
	persistMu sync.Mutex
	// concurrency bounds simultaneous agent calls within one request (0 = default).
	concurrency int
	met         *metrics
	log         *slog.Logger
	recorder    record.EventRecorder // nil = no events posted
}

func newController(k8s kubernetes.Interface, agents AgentPool, st *store, logger *slog.Logger) *controller {
	return &controller{
		k8s:        k8s,
		agents:     agents,
		store:      st,
		timeshifts: make(map[string]*timeshift),
		reserved:   make(map[string]bool),
		met:        newMetrics(),
		log:        logger.With("component", "controller"),
	}
}

// setRecorder sets the Kubernetes EventRecorder used to post TTL-expiry events
// on pods. Must be called before the sweeper starts.
func (c *controller) setRecorder(r record.EventRecorder) { c.recorder = r }

// parallel runs fn(0..n-1) with at most c.concurrency calls in flight and waits
// for all of them. Callers write results into their own slice index, so no
// locking is needed there.
func (c *controller) parallel(n int, fn func(i int)) {
	limit := c.concurrency
	if limit <= 0 {
		limit = defaultConcurrency
	}
	if limit > n {
		limit = n
	}
	if limit <= 1 {
		for i := 0; i < n; i++ {
			fn(i)
		}
		return
	}
	var wg sync.WaitGroup
	sem := make(chan struct{}, limit)
	for i := 0; i < n; i++ {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int) {
			defer wg.Done()
			defer func() { <-sem }()
			fn(i)
		}(i)
	}
	wg.Wait()
}

// persist snapshots the current registry and writes it to the backing store.
// Encoding happens under a read lock (µs); the ConfigMap write happens outside.
// Errors are logged but not fatal — in-memory state is always authoritative.
//
// The write is detached from ctx: the change is already made in memory, and a
// client that hangs up must not leave the store behind it.
func (c *controller) persist(ctx context.Context) {
	if c.store == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
	defer cancel()

	// Held across both the encode and the flush, so whichever persist runs last
	// also encodes last and writes the newest state.
	c.persistMu.Lock()
	defer c.persistMu.Unlock()
	c.mu.RLock()
	data, err := c.store.encode(c.timeshifts)
	c.mu.RUnlock()
	if err != nil {
		c.log.Error("persist: encode", "err", err)
		return
	}
	if err := c.store.flush(ctx, data); err != nil {
		c.log.Error("persist", "err", err)
	}
}

// restore loads the timeshift registry from the backing store and resets the
// Prometheus active gauge to match. A missing ConfigMap (first run) or a nil
// store are both treated as no-ops. Load failures are logged but not fatal.
func (c *controller) restore(ctx context.Context) {
	if c.store == nil {
		return
	}
	timeshifts, err := c.store.load(ctx)
	if err != nil {
		c.log.Warn("restore: starting with empty state", "err", err)
		return
	}
	if len(timeshifts) == 0 {
		return
	}
	c.mu.Lock()
	c.timeshifts = timeshifts
	c.mu.Unlock()
	c.met.timeshiftsActive.Set(float64(len(timeshifts)))
	c.log.Info("restored timeshifts from store", "count", len(timeshifts))
}

// shortID returns the first 8 characters of an id for logs, or all of it if it
// is shorter (a malformed id must not crash a log line).
func shortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

// injectTask is one running container to inject.
type injectTask struct {
	pod    *corev1.Pod
	status corev1.ContainerStatus
}

// runningContainers lists the running containers of the pods, in order.
func runningContainers(pods []corev1.Pod) []injectTask {
	var out []injectTask
	for i := range pods {
		for _, cs := range pods[i].Status.ContainerStatuses {
			if cs.State.Running == nil || cs.ContainerID == "" {
				continue
			}
			out = append(out, injectTask{pod: &pods[i], status: cs})
		}
	}
	return out
}

// injectContainers injects target into each task's container, in parallel, and
// returns the resulting handles in task order. Per-container errors are logged
// but not fatal so that one bad container does not abort the rest.
func (c *controller) injectContainers(ctx context.Context, tasks []injectTask, target time.Time, freeze bool) []containerHandle {
	results := make([]*containerHandle, len(tasks))
	c.parallel(len(tasks), func(i int) {
		t := tasks[i]
		nodeIP := t.pod.Status.HostIP
		hid, err := c.agents.Inject(ctx, nodeIP, t.status.ContainerID, target, freeze)
		if err != nil {
			c.log.Warn("inject failed",
				"pod", t.pod.Namespace+"/"+t.pod.Name,
				"container", t.status.Name,
				"err", err)
			c.met.injectTotal.WithLabelValues("error").Inc()
			return
		}
		c.met.injectTotal.WithLabelValues("success").Inc()
		results[i] = &containerHandle{
			pod:         t.pod.Name,
			container:   t.status.Name,
			nodeIP:      nodeIP,
			containerID: t.status.ContainerID,
			agentHandle: hid,
		}
	})
	var out []containerHandle
	for _, h := range results {
		if h != nil {
			out = append(out, *h)
		}
	}
	return out
}

// createTimeshift lists pods matching ns+labelSel, injects target into each running
// container via the per-node agent, and registers the timeshift.
func (c *controller) createTimeshift(ctx context.Context, ns, labelSel string, target time.Time, ttl time.Duration, freeze bool) (*timeshift, error) {
	pods, err := c.k8s.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{
		LabelSelector: labelSel,
	})
	if err != nil {
		return nil, fmt.Errorf("list pods: %w", err)
	}
	if len(pods.Items) == 0 {
		return nil, &notFoundError{fmt.Sprintf("no pods in namespace %q match selector %q", ns, labelSel)}
	}

	// Claim the containers before injecting, so a concurrent create for an
	// overlapping selector is refused instead of injecting the same containers
	// and leaving two timeshifts that each believe they own the clock.
	release, err := c.reserve(ns, pods.Items)
	if err != nil {
		return nil, err
	}
	defer release()

	handles := c.injectContainers(ctx, runningContainers(pods.Items), target, freeze)
	if err := ctx.Err(); err != nil {
		// The caller gave up part-way. Nothing records these injections, so undo them.
		undoCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		c.releaseHandles(undoCtx, ns, handles)
		return nil, err
	}
	if len(handles) == 0 {
		return nil, fmt.Errorf("no running containers found in matched pods")
	}

	now := time.Now()
	s := newTimeshift()
	s.id = newID()
	s.namespace = ns
	s.labelSelector = labelSel
	s.frozen = freeze
	s.ttl = ttl
	s.createdAt = now
	s.handles = handles
	if freeze {
		s.frozenAt = target
	} else {
		s.offset = target.Sub(now)
	}
	if ttl > 0 {
		s.expiresAt = now.Add(ttl)
	}

	c.mu.Lock()
	c.timeshifts[s.id] = s
	snapshot := s.clone()
	c.mu.Unlock()

	c.met.timeshiftsActive.Inc()
	c.persist(ctx)

	ttlStr := "none"
	if ttl > 0 {
		ttlStr = ttl.String()
	}
	mode := "advancing"
	if freeze {
		mode = "frozen"
	}
	c.log.Info("created timeshift",
		"timeshift_id", shortID(snapshot.id),
		"namespace", ns,
		"selector", labelSel,
		"target", snapshot.effectiveTime().UTC().Format(time.RFC3339),
		"mode", mode,
		"ttl", ttlStr,
		"containers", len(handles))
	return snapshot, nil
}

// listTimeshifts returns all active timeshifts sorted oldest-first.
func (c *controller) listTimeshifts() []api.TimeshiftResponse {
	c.mu.RLock()
	out := make([]*timeshift, 0, len(c.timeshifts))
	for _, s := range c.timeshifts {
		out = append(out, s.clone())
	}
	c.mu.RUnlock()

	sort.Slice(out, func(i, j int) bool {
		return out[i].createdAt.Before(out[j].createdAt)
	})
	responses := make([]api.TimeshiftResponse, len(out))
	for i, s := range out {
		responses[i] = s.toResponse()
	}
	return responses
}

// getTimeshift returns a snapshot of the timeshift for id, or errNotFound.
func (c *controller) getTimeshift(id string) (*timeshift, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	s := c.timeshifts[id]
	if s == nil {
		return nil, &notFoundError{fmt.Sprintf("timeshift %q not found", id)}
	}
	return s.clone(), nil
}

// lookup returns the registered timeshift itself, for callers that must lock its
// op mutex and then mutate it. Everything else uses getTimeshift.
func (c *controller) lookup(id string) (*timeshift, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	s := c.timeshifts[id]
	if s == nil {
		return nil, &notFoundError{fmt.Sprintf("timeshift %q not found", id)}
	}
	return s, nil
}

// registered reports whether s is still the timeshift registered under its id,
// i.e. was not deleted while the caller waited for its op lock.
func (c *controller) registered(s *timeshift) bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.timeshifts[s.id] == s
}

// updateFailedError is returned when an update reached no container at all.
// The registry is left as it was, so the response never claims a clock that no
// container is running.
type updateFailedError struct{ failures []string }

func (e *updateFailedError) Error() string {
	return "no container accepted the update: " + strings.Join(e.failures, "; ")
}

// updateResult is what updating one container produced.
type updateResult struct {
	newHandle string // set when the container had to be re-injected
	err       error
}

// updateHandle sets one container's clock. If the agent has forgotten the handle
// (it restarted) the container is injected again and the new handle returned.
func (c *controller) updateHandle(ctx context.Context, h containerHandle, target time.Time, freeze bool) updateResult {
	err := c.agents.SetTime(ctx, h.nodeIP, h.agentHandle, target, freeze)
	if err == nil {
		c.met.setTimeTotal.WithLabelValues("success").Inc()
		return updateResult{}
	}
	c.met.setTimeTotal.WithLabelValues("error").Inc()
	if !isAgentNotFound(err) {
		c.log.Warn("SetTime failed", "handle", shortID(h.agentHandle), "pod", h.pod, "err", err)
		return updateResult{err: err}
	}
	// Agent restarted — re-inject the container.
	c.log.Info("re-injecting container after agent restart",
		"handle", shortID(h.agentHandle),
		"container_id", h.containerID)
	newHandle, injErr := c.agents.Inject(ctx, h.nodeIP, h.containerID, target, freeze)
	if injErr != nil {
		c.log.Warn("re-inject failed", "container_id", h.containerID, "err", injErr)
		c.met.injectTotal.WithLabelValues("error").Inc()
		return updateResult{err: injErr}
	}
	c.met.injectTotal.WithLabelValues("success").Inc()
	return updateResult{newHandle: newHandle}
}

// updateTimeshift calls SetTime on every agent handle for the timeshift and updates the
// registered target time. If an agent handle is gone (agent restarted), it re-injects
// the container and updates the stored handle before retrying.
//
// If the containers are reached only in part, the update stands and the returned
// snapshot lists the ones missed in its warnings. If none is reached it fails and
// the registry keeps its previous state.
func (c *controller) updateTimeshift(ctx context.Context, id string, target time.Time, freeze bool) (*timeshift, error) {
	s, err := c.lookup(id)
	if err != nil {
		return nil, err
	}
	s.op.Lock()
	defer s.op.Unlock()
	return c.updateLocked(ctx, s, target, freeze)
}

// updateLocked is updateTimeshift for a caller that already holds s.op.
func (c *controller) updateLocked(ctx context.Context, s *timeshift, target time.Time, freeze bool) (*timeshift, error) {
	// The timeshift may have been deleted while we waited for its lock.
	if !c.registered(s) {
		return nil, &notFoundError{fmt.Sprintf("timeshift %q not found", s.id)}
	}

	// Snapshot handles so gRPC calls happen outside the lock.
	c.mu.RLock()
	handles := make([]containerHandle, len(s.handles))
	copy(handles, s.handles)
	c.mu.RUnlock()

	results := make([]updateResult, len(handles))
	c.parallel(len(handles), func(i int) {
		results[i] = c.updateHandle(ctx, handles[i], target, freeze)
	})

	var failures []string
	for i, r := range results {
		if r.err != nil {
			failures = append(failures, fmt.Sprintf("%s/%s: %v", handles[i].pod, handles[i].container, r.err))
		}
	}
	if len(handles) > 0 && len(failures) == len(handles) {
		return nil, &updateFailedError{failures: failures}
	}

	now := time.Now()
	c.mu.Lock()
	// Apply re-injected handle IDs. The op lock keeps the handle list unchanged
	// since the snapshot, so the indexes still line up.
	for i, r := range results {
		if r.newHandle != "" && i < len(s.handles) && s.handles[i].containerID == handles[i].containerID {
			s.handles[i].agentHandle = r.newHandle
		}
	}
	if freeze {
		s.frozenAt = target
		s.offset = 0
	} else {
		s.offset = target.Sub(now)
		s.frozenAt = time.Time{}
	}
	s.frozen = freeze
	snapshot := s.clone()
	snapshot.warnings = failures
	c.mu.Unlock()
	c.persist(ctx)
	return snapshot, nil
}

// advanceTimeshift advances the timeshift's clock by delta. For advancing
// timeshifts the stored offset grows by delta; for frozen ones frozenAt shifts
// by delta. All agent handles are updated immediately via SetTime.
//
// The clock keeps its mode: advancing a frozen timeshift leaves it frozen at the
// new instant. freeze=true additionally freezes a running one at the shifted
// instant; it can never thaw a frozen one, which takes an absolute-time update.
func (c *controller) advanceTimeshift(ctx context.Context, id string, delta time.Duration, freeze bool) (*timeshift, error) {
	s, err := c.lookup(id)
	if err != nil {
		return nil, err
	}
	s.op.Lock()
	defer s.op.Unlock()

	c.mu.RLock()
	frozen := s.frozen
	var target time.Time
	if frozen {
		target = s.frozenAt.Add(delta)
	} else {
		target = time.Now().Add(s.offset + delta)
	}
	c.mu.RUnlock()
	return c.updateLocked(ctx, s, target, freeze || frozen)
}

// releaseError is returned when a delete could not reset every container. The
// timeshift is kept, holding just those containers, so the delete can be retried.
type releaseError struct {
	remaining int
	failures  []string
}

func (e *releaseError) Error() string {
	return fmt.Sprintf("%d container(s) could not be reset to real time, so the timeshift was kept to retry: %s",
		e.remaining, strings.Join(e.failures, "; "))
}

// deleteTimeshift resets all handles to real time and removes the timeshift.
// The timeshift leaves the registry only once every container is back on the
// real clock; if some cannot be reset it stays, holding just those, and the
// caller is told so the delete can be retried. Dropping it first would leave
// containers on a fake clock that nothing tracks any more.
func (c *controller) deleteTimeshift(ctx context.Context, id string) error {
	s, err := c.lookup(id)
	if err != nil {
		return err
	}
	s.op.Lock()
	defer s.op.Unlock()
	if !c.registered(s) {
		return &notFoundError{fmt.Sprintf("timeshift %q not found", id)}
	}

	c.mu.RLock()
	handles := append([]containerHandle(nil), s.handles...)
	c.mu.RUnlock()

	failed := c.releaseHandles(ctx, s.namespace, handles)

	c.mu.Lock()
	if len(failed) == 0 {
		delete(c.timeshifts, id)
	} else {
		s.handles = failedHandles(failed)
		s.resetFailures++
	}
	c.mu.Unlock()
	c.persist(ctx)

	if len(failed) > 0 {
		failures := make([]string, len(failed))
		for i, f := range failed {
			failures[i] = fmt.Sprintf("%s/%s: %v", f.h.pod, f.h.container, f.err)
		}
		return &releaseError{remaining: len(failed), failures: failures}
	}
	c.met.timeshiftsActive.Dec()
	c.log.Info("deleted timeshift", "timeshift_id", shortID(id))
	return nil
}

// failedRelease is a container that could not be reset, and why.
type failedRelease struct {
	h   containerHandle
	err error
}

func failedHandles(f []failedRelease) []containerHandle {
	out := make([]containerHandle, len(f))
	for i := range f {
		out[i] = f[i].h
	}
	return out
}

// releaseHandles puts every container back on the real clock and takes the
// hooks out, in parallel, and returns the containers that could not be reset.
// A container whose pod has gone away (or whose container was replaced) counts
// as done: there is nothing left to reset.
func (c *controller) releaseHandles(ctx context.Context, ns string, handles []containerHandle) []failedRelease {
	errs := make([]error, len(handles))
	c.parallel(len(handles), func(i int) {
		errs[i] = c.releaseHandle(ctx, ns, handles[i])
	})
	var failed []failedRelease
	for i, err := range errs {
		if err != nil {
			failed = append(failed, failedRelease{h: handles[i], err: err})
		}
	}
	return failed
}

// releaseHandle resets one container to real time, then removes its hook.
//
// The reset is what matters, so it comes first and its failure is what fails
// the release. Removing the hook only gives back the kernel's fast path for
// clock reads, so a failure there is logged and not retried.
func (c *controller) releaseHandle(ctx context.Context, ns string, h containerHandle) error {
	err := c.agents.Reset(ctx, h.nodeIP, h.agentHandle)
	if err != nil && isAgentNotFound(err) {
		// The agent has lost the handle. After a graceful shutdown it resets
		// every clock first, but after a crash the hooks are still in the
		// processes with the old offset. Injecting "now" takes the existing hook
		// over (the agent reuses it) and sets it to real time in the same step.
		c.log.Info("reset: agent has forgotten the handle, taking the container over",
			"handle", shortID(h.agentHandle), "container_id", h.containerID)
		newHandle, injErr := c.agents.Inject(ctx, h.nodeIP, h.containerID, time.Now(), false)
		if injErr == nil {
			h.agentHandle = newHandle
			err = nil
		} else if isAgentNotFound(injErr) || c.gone(ctx, ns, h) {
			err = nil // the container no longer exists: nothing to reset
		} else {
			err = injErr
		}
	}
	if err != nil {
		if c.gone(ctx, ns, h) {
			c.met.resetTotal.WithLabelValues("gone").Inc()
			return nil
		}
		c.met.resetTotal.WithLabelValues("error").Inc()
		c.log.Warn("reset failed", "handle", shortID(h.agentHandle), "pod", h.pod, "err", err)
		return err
	}
	c.met.resetTotal.WithLabelValues("success").Inc()

	if rerr := c.agents.Release(ctx, h.nodeIP, h.agentHandle); rerr != nil && !isAgentNotFound(rerr) && !isUnimplemented(rerr) {
		c.log.Warn("could not remove the clock hook (the container is on real time, but each clock read still costs a syscall)",
			"handle", shortID(h.agentHandle), "pod", h.pod, "err", rerr)
	}
	return nil
}

// gone reports whether the container behind h no longer exists: its pod was
// deleted, or the container was restarted under a new id. If the question
// cannot be answered it reports false, which keeps the retry going.
func (c *controller) gone(ctx context.Context, ns string, h containerHandle) bool {
	pod, err := c.k8s.CoreV1().Pods(ns).Get(ctx, h.pod, metav1.GetOptions{})
	if err != nil {
		return k8serrors.IsNotFound(err)
	}
	for _, cs := range pod.Status.ContainerStatuses {
		if cs.Name == h.container {
			return cs.ContainerID != h.containerID || cs.State.Running == nil
		}
	}
	return true // the pod no longer has a container of that name
}

// startSweeper starts a goroutine that resets and removes expired timeshifts on
// each tick of sweepInterval. The goroutine exits when ctx is cancelled.
func (c *controller) startSweeper(ctx context.Context, sweepInterval time.Duration) {
	go func() {
		ticker := time.NewTicker(sweepInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				c.sweepExpired(ctx)
			}
		}
	}()
}

// sweepExpired resets and removes every timeshift whose TTL has passed. The TTL
// is the safety net that stops a forgotten timeshift leaving a service on a fake
// clock, so an expired timeshift is removed only once its containers are
// confirmed back on real time. One whose reset fails stays, holding just the
// containers still faked, and is retried on the next sweep.
func (c *controller) sweepExpired(ctx context.Context) {
	now := time.Now()
	c.mu.RLock()
	var expired []*timeshift
	for _, s := range c.timeshifts {
		if !s.expiresAt.IsZero() && now.After(s.expiresAt) {
			expired = append(expired, s)
		}
	}
	c.mu.RUnlock()

	for _, s := range expired {
		// An update or delete is already working on it; the next sweep will see it.
		if !s.op.TryLock() {
			continue
		}
		c.expire(ctx, s, now)
		s.op.Unlock()
	}
}

// expire resets one expired timeshift. The caller holds s.op.
func (c *controller) expire(ctx context.Context, s *timeshift, now time.Time) {
	if !c.registered(s) {
		return
	}
	c.mu.RLock()
	snapshot := s.clone()
	c.mu.RUnlock()

	failed := c.releaseHandles(ctx, snapshot.namespace, snapshot.handles)

	c.mu.Lock()
	if len(failed) == 0 {
		delete(c.timeshifts, s.id)
	} else {
		s.handles = failedHandles(failed)
		s.resetFailures++
	}
	attempts := s.resetFailures
	c.mu.Unlock()
	c.persist(ctx)

	if len(failed) > 0 {
		c.log.Warn("expired timeshift could not be fully reset; will retry",
			"timeshift_id", shortID(s.id), "containers_still_faked", len(failed), "attempt", attempts)
		if attempts <= maxResetFailureEvents {
			c.postResetFailedEvents(snapshot, failed)
		}
		return
	}

	c.log.Info("expiring timeshift",
		"timeshift_id", shortID(s.id),
		"overdue", now.Sub(snapshot.expiresAt).Round(time.Millisecond).String())
	c.met.sweepExpiredTotal.Inc()
	c.met.timeshiftsActive.Dec()
	c.postExpiryEvents(snapshot)
}

// postExpiryEvents emits a Kubernetes Event on each unique pod targeted by s,
// recording that the timeshift expired and which time it had been set to.
// No-op when c.recorder is nil.
func (c *controller) postExpiryEvents(s *timeshift) {
	if c.recorder == nil {
		return
	}
	msg := fmt.Sprintf("Timeshift %s (target: %s) expired after %s",
		shortID(s.id), s.effectiveTime().UTC().Format(time.RFC3339), s.ttl.String())

	seen := make(map[string]bool, len(s.handles))
	for _, h := range s.handles {
		if seen[h.pod] {
			continue
		}
		seen[h.pod] = true
		podRef := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      h.pod,
				Namespace: s.namespace,
			},
		}
		c.recorder.Event(podRef, corev1.EventTypeNormal, "TimeshiftExpired", msg)
	}
}

// postResetFailedEvents warns on each pod whose container could not be put back
// on the real clock after its timeshift expired, because that pod is still
// running on fake time. No-op when c.recorder is nil.
func (c *controller) postResetFailedEvents(s *timeshift, failed []failedRelease) {
	if c.recorder == nil {
		return
	}
	for _, f := range failed {
		podRef := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: f.h.pod, Namespace: s.namespace}}
		c.recorder.Event(podRef, corev1.EventTypeWarning, "TimeshiftResetFailed",
			fmt.Sprintf("Timeshift %s expired but container %s could not be reset to real time and is still on a fake clock; retrying: %v",
				shortID(s.id), f.h.container, f.err))
	}
}

// ---------------------------------------------------------------------------

type notFoundError struct{ msg string }

func (e *notFoundError) Error() string { return e.msg }

// conflictError is returned by createTimeshift when one or more of the
// resolved containers is already tracked by an active timeshift.
type conflictError struct{ entries []conflictEntry }

type conflictEntry struct {
	pod         string
	container   string
	timeshiftID string // empty when the container is being claimed by a create in flight
}

func (e *conflictError) Error() string {
	parts := make([]string, len(e.entries))
	for i, ce := range e.entries {
		owner := "another request in progress"
		if ce.timeshiftID != "" {
			owner = "timeshift " + shortID(ce.timeshiftID)
		}
		parts[i] = fmt.Sprintf("%s/%s (%s)", ce.pod, ce.container, owner)
	}
	return "containers already have an active timeshift: " + strings.Join(parts, ", ")
}

// occupiedContainers returns a map of "namespace\x00pod\x00container" → timeshiftID
// for every container currently tracked by an active timeshift.
// Must be called with c.mu held for reading.
func (c *controller) occupiedContainers() map[string]string {
	m := make(map[string]string)
	for _, s := range c.timeshifts {
		for _, h := range s.handles {
			key := s.namespace + "\x00" + h.pod + "\x00" + h.container
			m[key] = s.id
		}
	}
	return m
}

func containerKey(ns, pod, container string) string {
	return ns + "\x00" + pod + "\x00" + container
}

// conflictsLocked returns the keys of the running containers in pods that are
// free, or a conflictError naming the ones that are not. c.mu must be held.
func (c *controller) conflictsLocked(ns string, pods []corev1.Pod) ([]string, error) {
	occupied := c.occupiedContainers()
	var keys []string
	var conflicts []conflictEntry
	for _, t := range runningContainers(pods) {
		key := containerKey(ns, t.pod.Name, t.status.Name)
		if existingID, ok := occupied[key]; ok {
			conflicts = append(conflicts, conflictEntry{pod: t.pod.Name, container: t.status.Name, timeshiftID: existingID})
		} else if c.reserved[key] {
			conflicts = append(conflicts, conflictEntry{pod: t.pod.Name, container: t.status.Name})
		} else {
			keys = append(keys, key)
		}
	}
	if len(conflicts) > 0 {
		return nil, &conflictError{entries: conflicts}
	}
	return keys, nil
}

// reserve atomically checks that none of the running containers in pods is
// taken and claims them all, so a concurrent create cannot slip between the
// check and the registration. The returned function releases the claim; call it
// once the timeshift is registered (or the create has failed).
func (c *controller) reserve(ns string, pods []corev1.Pod) (func(), error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	keys, err := c.conflictsLocked(ns, pods)
	if err != nil {
		return nil, err
	}
	for _, k := range keys {
		c.reserved[k] = true
	}
	return func() {
		c.mu.Lock()
		defer c.mu.Unlock()
		for _, k := range keys {
			delete(c.reserved, k)
		}
	}, nil
}

// isAgentNotFound reports whether err (possibly wrapped) is a gRPC NOT_FOUND status,
// indicating the agent restarted and lost its in-memory handle.
func isAgentNotFound(err error) bool {
	for e := err; e != nil; e = errors.Unwrap(e) {
		if s, ok := grpcstatus.FromError(e); ok {
			return s.Code() == codes.NotFound
		}
	}
	return false
}

// isUnimplemented reports whether err is a gRPC UNIMPLEMENTED status: an agent
// older than the RPC being called.
func isUnimplemented(err error) bool {
	for e := err; e != nil; e = errors.Unwrap(e) {
		if s, ok := grpcstatus.FromError(e); ok {
			return s.Code() == codes.Unimplemented
		}
	}
	return false
}

func newID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(fmt.Sprintf("controller: rand.Read: %v", err))
	}
	return hex.EncodeToString(b[:])
}
