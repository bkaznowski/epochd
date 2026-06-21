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

	"epochd/pkg/api"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"
)

// AgentPool abstracts per-node gRPC connections to the epochd agent.
// agentclient.Pool satisfies this interface; tests use a mock.
type AgentPool interface {
	Inject(ctx context.Context, nodeIP, containerID string, target time.Time) (string, error)
	SetTime(ctx context.Context, nodeIP, handleID string, target time.Time) error
	Reset(ctx context.Context, nodeIP, handleID string) error
	GetStatus(ctx context.Context, nodeIP, handleID string) (*api.HandleStatus, error)
}

// containerHandle records the agent-issued handle for one injected container.
type containerHandle struct {
	pod         string // pod name (namespace lives on the parent timeshift)
	container   string
	nodeIP      string
	containerID string // CRI container ID — used to re-inject after an agent restart
	agentHandle string // opaque ID returned by the agent's Inject RPC
}

// timeshift is one active time-timeshift entry in the registry.
type timeshift struct {
	id            string
	namespace     string
	labelSelector string
	targetTime    time.Time
	ttl           time.Duration
	expiresAt     time.Time
	createdAt     time.Time
	handles       []containerHandle
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
		Time:      s.targetTime.UTC().Format(time.RFC3339),
		AppliedTo: s.appliedTo(),
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
	met        *metrics
	log        *slog.Logger
}

func newController(k8s kubernetes.Interface, agents AgentPool, st *store, logger *slog.Logger) *controller {
	return &controller{
		k8s:        k8s,
		agents:     agents,
		store:      st,
		timeshifts: make(map[string]*timeshift),
		met:        newMetrics(),
		log:        logger.With("component", "controller"),
	}
}

// persist snapshots the current registry and writes it to the backing store.
// Encoding happens under a read lock (µs); the ConfigMap write happens outside.
// Errors are logged but not fatal — in-memory state is always authoritative.
func (c *controller) persist(ctx context.Context) {
	if c.store == nil {
		return
	}
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

// createTimeshift lists pods matching ns+labelSel, injects target into each running
// container via the per-node agent, and registers the timeshift.
func (c *controller) createTimeshift(ctx context.Context, ns, labelSel string, target time.Time, ttl time.Duration) (*timeshift, error) {
	pods, err := c.k8s.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{
		LabelSelector: labelSel,
	})
	if err != nil {
		return nil, fmt.Errorf("list pods: %w", err)
	}
	if len(pods.Items) == 0 {
		return nil, &notFoundError{fmt.Sprintf("no pods in namespace %q match selector %q", ns, labelSel)}
	}

	if err := c.checkConflicts(ns, pods.Items); err != nil {
		return nil, err
	}

	var handles []containerHandle
	for i := range pods.Items {
		handles = append(handles, c.injectPod(ctx, &pods.Items[i], target)...)
	}
	if len(handles) == 0 {
		return nil, fmt.Errorf("no running containers found in matched pods")
	}

	now := time.Now()
	s := &timeshift{
		id:            newID(),
		namespace:     ns,
		labelSelector: labelSel,
		targetTime:    target,
		ttl:           ttl,
		createdAt:     now,
		handles:       handles,
	}
	if ttl > 0 {
		s.expiresAt = now.Add(ttl)
	}

	c.mu.Lock()
	c.timeshifts[s.id] = s
	c.mu.Unlock()

	c.met.timeshiftsActive.Inc()
	c.persist(ctx)

	ttlStr := "none"
	if ttl > 0 {
		ttlStr = ttl.String()
	}
	c.log.Info("created timeshift",
		"timeshift_id", s.id[:8],
		"namespace", ns,
		"selector", labelSel,
		"target", target.UTC().Format(time.RFC3339),
		"ttl", ttlStr,
		"containers", len(handles))
	return s, nil
}

// injectPod injects target into every running container of pod and returns the
// resulting handles. Per-container errors are logged but not fatal so that one
// bad container does not abort the rest.
func (c *controller) injectPod(ctx context.Context, pod *corev1.Pod, target time.Time) []containerHandle {
	nodeIP := pod.Status.HostIP
	var out []containerHandle
	for _, cs := range pod.Status.ContainerStatuses {
		if cs.State.Running == nil || cs.ContainerID == "" {
			continue
		}
		hid, err := c.agents.Inject(ctx, nodeIP, cs.ContainerID, target)
		if err != nil {
			c.log.Warn("inject failed",
				"pod", pod.Namespace+"/"+pod.Name,
				"container", cs.Name,
				"err", err)
			c.met.injectTotal.WithLabelValues("error").Inc()
			continue
		}
		c.met.injectTotal.WithLabelValues("success").Inc()
		out = append(out, containerHandle{
			pod:         pod.Name,
			container:   cs.Name,
			nodeIP:      nodeIP,
			containerID: cs.ContainerID,
			agentHandle: hid,
		})
	}
	return out
}

// listTimeshifts returns all active timeshifts sorted oldest-first.
func (c *controller) listTimeshifts() []api.TimeshiftResponse {
	c.mu.RLock()
	out := make([]*timeshift, 0, len(c.timeshifts))
	for _, s := range c.timeshifts {
		out = append(out, s)
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

// getTimeshift returns the timeshift for id, or errNotFound.
func (c *controller) getTimeshift(id string) (*timeshift, error) {
	c.mu.RLock()
	s := c.timeshifts[id]
	c.mu.RUnlock()
	if s == nil {
		return nil, &notFoundError{fmt.Sprintf("timeshift %q not found", id)}
	}
	return s, nil
}

// updateTimeshift calls SetTime on every agent handle for the timeshift and updates the
// registered target time. If an agent handle is gone (agent restarted), it re-injects
// the container and updates the stored handle before retrying.
func (c *controller) updateTimeshift(ctx context.Context, id string, target time.Time) (*timeshift, error) {
	s, err := c.getTimeshift(id)
	if err != nil {
		return nil, err
	}

	// Snapshot handles so gRPC calls happen outside the lock.
	c.mu.RLock()
	handles := make([]containerHandle, len(s.handles))
	copy(handles, s.handles)
	c.mu.RUnlock()

	type update struct {
		idx       int
		newHandle string
	}
	var updates []update

	for i, h := range handles {
		if err := c.agents.SetTime(ctx, h.nodeIP, h.agentHandle, target); err == nil {
			c.met.setTimeTotal.WithLabelValues("success").Inc()
			continue
		} else if !isAgentNotFound(err) {
			c.log.Warn("SetTime failed", "timeshift_id", id[:8], "handle", h.agentHandle, "err", err)
			c.met.setTimeTotal.WithLabelValues("error").Inc()
			continue
		}
		// Agent restarted — re-inject the container then retry.
		c.met.setTimeTotal.WithLabelValues("error").Inc()
		c.log.Info("re-injecting container after agent restart",
			"timeshift_id", id[:8],
			"handle", h.agentHandle,
			"container_id", h.containerID)
		newHandle, injErr := c.agents.Inject(ctx, h.nodeIP, h.containerID, target)
		if injErr != nil {
			c.log.Warn("re-inject failed", "container_id", h.containerID, "err", injErr)
			c.met.injectTotal.WithLabelValues("error").Inc()
			continue
		}
		c.met.injectTotal.WithLabelValues("success").Inc()
		updates = append(updates, update{i, newHandle})
	}

	c.mu.Lock()
	// Apply re-injected handle IDs; match by containerID in case handles shifted.
	for _, u := range updates {
		if u.idx < len(s.handles) && s.handles[u.idx].containerID == handles[u.idx].containerID {
			s.handles[u.idx].agentHandle = u.newHandle
		}
	}
	s.targetTime = target
	c.mu.Unlock()
	c.persist(ctx)
	return s, nil
}

// deleteTimeshift resets all handles to real time and removes the timeshift.
func (c *controller) deleteTimeshift(ctx context.Context, id string) error {
	c.mu.Lock()
	s := c.timeshifts[id]
	delete(c.timeshifts, id)
	c.mu.Unlock()

	if s == nil {
		return &notFoundError{fmt.Sprintf("timeshift %q not found", id)}
	}
	c.met.timeshiftsActive.Dec()
	c.persist(ctx)
	c.resetHandles(ctx, s)
	c.log.Info("deleted timeshift", "timeshift_id", id[:8])
	return nil
}

// resetHandles calls Reset on every handle in a timeshift; errors are logged only.
// A NOT_FOUND response means the agent restarted and the handle is already gone — that
// is treated as a successful reset since the container's clock is already at real time.
func (c *controller) resetHandles(ctx context.Context, s *timeshift) {
	for _, h := range s.handles {
		if err := c.agents.Reset(ctx, h.nodeIP, h.agentHandle); err != nil {
			if isAgentNotFound(err) {
				c.log.Info("reset skipped: handle already gone (agent restarted)",
					"timeshift_id", s.id[:8], "handle", h.agentHandle)
				continue
			}
			c.log.Warn("reset failed", "timeshift_id", s.id[:8], "handle", h.agentHandle, "err", err)
		}
	}
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

func (c *controller) sweepExpired(ctx context.Context) {
	now := time.Now()
	c.mu.Lock()
	var expired []*timeshift
	for id, s := range c.timeshifts {
		if !s.expiresAt.IsZero() && now.After(s.expiresAt) {
			expired = append(expired, s)
			delete(c.timeshifts, id)
		}
	}
	c.mu.Unlock()

	if len(expired) > 0 {
		c.persist(ctx)
	}
	for _, s := range expired {
		c.log.Info("expiring timeshift",
			"timeshift_id", s.id[:8],
			"overdue", now.Sub(s.expiresAt).Round(time.Millisecond).String())
		c.met.sweepExpiredTotal.Inc()
		c.met.timeshiftsActive.Dec()
		c.resetHandles(ctx, s)
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
	timeshiftID string
}

func (e *conflictError) Error() string {
	parts := make([]string, len(e.entries))
	for i, ce := range e.entries {
		parts[i] = fmt.Sprintf("%s/%s (timeshift %s)", ce.pod, ce.container, ce.timeshiftID[:8])
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

// checkConflicts returns a conflictError if any running container in pods is
// already tracked by another timeshift, nil otherwise.
func (c *controller) checkConflicts(ns string, pods []corev1.Pod) error {
	c.mu.RLock()
	occupied := c.occupiedContainers()
	c.mu.RUnlock()

	var conflicts []conflictEntry
	for i := range pods {
		pod := &pods[i]
		for _, cs := range pod.Status.ContainerStatuses {
			if cs.State.Running == nil || cs.ContainerID == "" {
				continue
			}
			key := ns + "\x00" + pod.Name + "\x00" + cs.Name
			if existingID, ok := occupied[key]; ok {
				conflicts = append(conflicts, conflictEntry{
					pod:         pod.Name,
					container:   cs.Name,
					timeshiftID: existingID,
				})
			}
		}
	}
	if len(conflicts) > 0 {
		return &conflictError{entries: conflicts}
	}
	return nil
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

func newID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(fmt.Sprintf("controller: rand.Read: %v", err))
	}
	return hex.EncodeToString(b[:])
}
