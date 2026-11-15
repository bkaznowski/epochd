package main

import (
	"context"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/tools/cache"
)

// podWorkers is how many pods are reconciled at once.
const podWorkers = 4

// podQueue is a work queue of pod keys ("namespace/name") that collapses a burst
// of events for one pod into a single pending item.
type podQueue struct {
	mu     sync.Mutex
	cond   *sync.Cond
	keys   []string
	set    map[string]bool
	closed bool
}

func newPodQueue() *podQueue {
	q := &podQueue{set: make(map[string]bool)}
	q.cond = sync.NewCond(&q.mu)
	return q
}

// add queues key unless it is already waiting.
func (q *podQueue) add(key string) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed || q.set[key] {
		return
	}
	q.set[key] = true
	q.keys = append(q.keys, key)
	q.cond.Signal()
}

// next blocks until a key is available and returns it, or reports false once
// the queue is closed and empty.
func (q *podQueue) next() (string, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for len(q.keys) == 0 && !q.closed {
		q.cond.Wait()
	}
	if len(q.keys) == 0 {
		return "", false
	}
	key := q.keys[0]
	q.keys = q.keys[1:]
	delete(q.set, key) // events arriving while it is processed queue it again
	return key, true
}

func (q *podQueue) close() {
	q.mu.Lock()
	q.closed = true
	q.mu.Unlock()
	q.cond.Broadcast()
}

// startPodWatcher starts a SharedInformer that watches pods across all namespaces.
// When a pod transitions to Running and its labels match an active timeshift's
// selector, the new containers are injected — recovering from pod restarts without
// any user action. When a pod is deleted its containers are dropped from the
// timeshifts that tracked them.
//
// Events only queue the pod's key: the work, which makes agent RPCs, runs on
// worker goroutines. Doing it inside the informer's callback would let one slow
// agent hold up every other pod event in the cluster.
func (c *controller) startPodWatcher(ctx context.Context) {
	factory := informers.NewSharedInformerFactory(c.k8s, 5*time.Minute)
	podInformer := factory.Core().V1().Pods().Informer()
	queue := newPodQueue()

	enqueue := func(obj interface{}) {
		if key, err := cache.DeletionHandlingMetaNamespaceKeyFunc(obj); err == nil {
			queue.add(key)
		}
	}
	podInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{ //nolint:errcheck
		AddFunc:    enqueue,
		UpdateFunc: func(_, newObj interface{}) { enqueue(newObj) },
		DeleteFunc: enqueue,
	})

	factory.Start(ctx.Done())
	cache.WaitForCacheSync(ctx.Done(), podInformer.HasSynced)

	for i := 0; i < podWorkers; i++ {
		go func() {
			for {
				key, ok := queue.next()
				if !ok {
					return
				}
				c.processPodKey(ctx, podInformer.GetStore(), key)
			}
		}()
	}
	go func() {
		<-ctx.Done()
		queue.close()
	}()
}

// processPodKey reconciles one pod against the current state of the cluster:
// what the informer's cache holds now, not what the event that queued it said.
func (c *controller) processPodKey(ctx context.Context, store cache.Store, key string) {
	obj, exists, err := store.GetByKey(key)
	if err != nil {
		c.log.Warn("pod watcher: lookup", "key", key, "err", err)
		return
	}
	if !exists {
		ns, name, err := cache.SplitMetaNamespaceKey(key)
		if err != nil {
			return
		}
		c.handlePodDeleted(ctx, ns, name)
		return
	}
	if pod, ok := obj.(*corev1.Pod); ok {
		c.handlePodEvent(ctx, pod)
	}
}

// handlePodEvent is called for every pod Add/Update event. It finds timeshifts whose
// namespace and label selector match the pod, removes stale handles for restarted
// containers, and injects the newly-running containers.
//
// This is separated from startPodWatcher so tests can call it directly.
func (c *controller) handlePodEvent(ctx context.Context, pod *corev1.Pod) {
	if pod.Status.Phase != corev1.PodRunning {
		return
	}
	for _, s := range c.matchingTimeshifts(pod) {
		s.op.Lock()
		c.adoptPod(ctx, s, pod)
		s.op.Unlock()
	}
}

// matchingTimeshifts returns the registered timeshifts whose namespace and
// selector match pod and that do not already cover every running container in it.
func (c *controller) matchingTimeshifts(pod *corev1.Pod) []*timeshift {
	c.mu.RLock()
	defer c.mu.RUnlock()
	var matched []*timeshift
	for _, s := range c.timeshifts {
		if s.namespace != pod.Namespace {
			continue
		}
		sel, err := labels.Parse(s.labelSelector)
		if err != nil {
			continue
		}
		if !sel.Matches(labels.Set(pod.Labels)) {
			continue
		}
		// Skip if we already have a handle for every running container in this pod
		// (avoids noisy re-injection on unrelated pod updates).
		if allContainersHandled(s.handles, pod) {
			continue
		}
		matched = append(matched, s)
	}
	return matched
}

// adoptPod injects the containers of pod that s does not cover yet, and replaces
// the handles of any container that restarted. The caller holds s.op.
func (c *controller) adoptPod(ctx context.Context, s *timeshift, pod *corev1.Pod) {
	c.mu.RLock()
	if c.timeshifts[s.id] != s {
		c.mu.RUnlock()
		return // deleted while we waited for its lock
	}
	known := make(map[string]bool)
	for _, h := range s.handles {
		if h.pod == pod.Name {
			known[h.containerID] = true
		}
	}
	target := s.effectiveTime()
	frozen := s.frozen
	c.mu.RUnlock()

	// Only the containers that need it: re-injecting one that is already faked
	// would waste a ptrace attach for nothing.
	var tasks []injectTask
	for _, cs := range pod.Status.ContainerStatuses {
		if cs.State.Running != nil && cs.ContainerID != "" && !known[cs.ContainerID] {
			tasks = append(tasks, injectTask{pod: pod, status: cs})
		}
	}
	if len(tasks) == 0 {
		return
	}
	newHandles := c.injectContainers(ctx, tasks, target, frozen)
	if len(newHandles) == 0 {
		return
	}

	replaced := make(map[string]bool, len(newHandles))
	for _, h := range newHandles {
		replaced[h.container] = true
	}

	// Build a new slice rather than filtering in place: readers that hold the old
	// one must not see it rewritten under them.
	c.mu.Lock()
	kept := make([]containerHandle, 0, len(s.handles)+len(newHandles))
	var stale []containerHandle
	for _, h := range s.handles {
		if h.pod == pod.Name && replaced[h.container] {
			stale = append(stale, h) // the same container, restarted under a new id
		} else {
			kept = append(kept, h)
		}
	}
	s.handles = append(kept, newHandles...)
	c.mu.Unlock()
	c.persist(ctx)

	for _, h := range stale {
		c.discard(ctx, h)
	}
	c.log.Info("pod watcher re-injected pod",
		"pod", pod.Namespace+"/"+pod.Name,
		"timeshift_id", shortID(s.id),
		"containers", len(newHandles))
}

// handlePodDeleted drops the containers of a deleted pod from every timeshift
// that tracked them. Left in place they would be listed as applied forever, and
// every update would try, and fail, to reach them.
func (c *controller) handlePodDeleted(ctx context.Context, ns, name string) {
	c.mu.RLock()
	var affected []*timeshift
	for _, s := range c.timeshifts {
		if s.namespace != ns {
			continue
		}
		for _, h := range s.handles {
			if h.pod == name {
				affected = append(affected, s)
				break
			}
		}
	}
	c.mu.RUnlock()

	for _, s := range affected {
		s.op.Lock()
		c.mu.Lock()
		var kept, gone []containerHandle
		if c.timeshifts[s.id] == s {
			for _, h := range s.handles {
				if h.pod == name {
					gone = append(gone, h)
				} else {
					kept = append(kept, h)
				}
			}
			if len(gone) > 0 {
				s.handles = kept
			}
		}
		c.mu.Unlock()
		s.op.Unlock()

		if len(gone) == 0 {
			continue
		}
		c.persist(ctx)
		for _, h := range gone {
			c.discard(ctx, h)
		}
		c.log.Info("pod watcher dropped a deleted pod",
			"pod", ns+"/"+name, "timeshift_id", shortID(s.id), "containers", len(gone))
	}
}

// discard asks the agent to forget a handle whose container no longer exists.
// Best effort: if the agent is unreachable it holds a little memory until it
// restarts, and there is nothing to retry because the processes are gone.
func (c *controller) discard(ctx context.Context, h containerHandle) {
	if err := c.agents.Release(ctx, h.nodeIP, h.agentHandle); err != nil && !isAgentNotFound(err) && !isUnimplemented(err) {
		c.log.Debug("could not release the handle of a replaced container", "handle", shortID(h.agentHandle), "err", err)
	}
}

// allContainersHandled returns true when every running container in pod already has
// an entry in handles with the same containerID — meaning no re-injection is needed.
func allContainersHandled(handles []containerHandle, pod *corev1.Pod) bool {
	known := make(map[string]bool, len(handles))
	for _, h := range handles {
		if h.pod == pod.Name {
			known[h.containerID] = true
		}
	}
	for _, cs := range pod.Status.ContainerStatuses {
		if cs.State.Running != nil && !known[cs.ContainerID] {
			return false
		}
	}
	return true
}
