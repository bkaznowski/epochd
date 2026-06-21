// Package main implements the epochd controller: an HTTP+JSON service that
// resolves pods via the Kubernetes API and orchestrates clock injection via
// per-node gRPC agents.
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log"
	"sort"
	"sync"
	"time"

	"epochd/pkg/api"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// AgentPool abstracts per-node gRPC connections to the epochd agent.
// agentclient.Pool satisfies this interface; tests use a mock.
type AgentPool interface {
	Inject(ctx context.Context, nodeIP, containerID string, target time.Time) (string, error)
	SetTime(ctx context.Context, nodeIP, handleID string, target time.Time) error
	Reset(ctx context.Context, nodeIP, handleID string) error
}

// containerHandle records the agent-issued handle for one injected container.
type containerHandle struct {
	pod         string // pod name (namespace lives on the parent timeshift)
	container   string
	nodeIP      string
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
	k8s    kubernetes.Interface
	agents AgentPool
	mu     sync.RWMutex
	timeshifts  map[string]*timeshift
}

func newController(k8s kubernetes.Interface, agents AgentPool) *controller {
	return &controller{
		k8s:    k8s,
		agents: agents,
		timeshifts:  make(map[string]*timeshift),
	}
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

	ttlStr := "none"
	if ttl > 0 {
		ttlStr = ttl.String()
	}
	log.Printf("controller: created timeshift %s ns=%s sel=%q target=%s ttl=%s containers=%d",
		s.id[:8], ns, labelSel, target.UTC().Format(time.RFC3339), ttlStr, len(handles))
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
			log.Printf("controller: inject pod=%s/%s container=%s: %v",
				pod.Namespace, pod.Name, cs.Name, err)
			continue
		}
		out = append(out, containerHandle{
			pod:         pod.Name,
			container:   cs.Name,
			nodeIP:      nodeIP,
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
// registered target time.
func (c *controller) updateTimeshift(ctx context.Context, id string, target time.Time) (*timeshift, error) {
	s, err := c.getTimeshift(id)
	if err != nil {
		return nil, err
	}

	for _, h := range s.handles {
		if err := c.agents.SetTime(ctx, h.nodeIP, h.agentHandle, target); err != nil {
			log.Printf("controller: SetTime timeshift=%s handle=%s: %v", id[:8], h.agentHandle, err)
		}
	}

	c.mu.Lock()
	s.targetTime = target
	c.mu.Unlock()
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
	c.resetHandles(ctx, s)
	log.Printf("controller: deleted timeshift %s", id[:8])
	return nil
}

// resetHandles calls Reset on every handle in a timeshift; errors are logged only.
func (c *controller) resetHandles(ctx context.Context, s *timeshift) {
	for _, h := range s.handles {
		if err := c.agents.Reset(ctx, h.nodeIP, h.agentHandle); err != nil {
			log.Printf("controller: reset timeshift=%s handle=%s: %v", s.id[:8], h.agentHandle, err)
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

	for _, s := range expired {
		log.Printf("controller: expiring timeshift %s (TTL exceeded by %v)",
			s.id[:8], now.Sub(s.expiresAt).Round(time.Millisecond))
		c.resetHandles(ctx, s)
	}
}

// ---------------------------------------------------------------------------

type notFoundError struct{ msg string }

func (e *notFoundError) Error() string { return e.msg }

func newID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(fmt.Sprintf("controller: rand.Read: %v", err))
	}
	return hex.EncodeToString(b[:])
}
