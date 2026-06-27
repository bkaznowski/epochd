package main

import (
	"context"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/tools/cache"
)

// startPodWatcher starts a SharedInformer that watches pods across all namespaces.
// When a pod transitions to Running and its labels match an active timeshift's
// selector, the new containers are injected — recovering from pod restarts without
// any user action.
func (c *controller) startPodWatcher(ctx context.Context) {
	factory := informers.NewSharedInformerFactory(c.k8s, 5*time.Minute)
	podInformer := factory.Core().V1().Pods().Informer()

	podInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{ //nolint:errcheck
		AddFunc: func(obj interface{}) {
			if pod, ok := obj.(*corev1.Pod); ok {
				c.handlePodEvent(ctx, pod)
			}
		},
		UpdateFunc: func(_, newObj interface{}) {
			if pod, ok := newObj.(*corev1.Pod); ok {
				c.handlePodEvent(ctx, pod)
			}
		},
	})

	factory.Start(ctx.Done())
	cache.WaitForCacheSync(ctx.Done(), podInformer.HasSynced)
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

	// Collect matching timeshifts without holding the write lock.
	type match struct {
		id       string
		offset   time.Duration
		frozenAt time.Time
		frozen   bool
	}
	c.mu.RLock()
	var matched []match
	for id, s := range c.timeshifts {
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
		matched = append(matched, match{id: id, offset: s.offset, frozenAt: s.frozenAt, frozen: s.frozen})
	}
	c.mu.RUnlock()

	if len(matched) == 0 {
		return
	}

	for _, m := range matched {
		var target time.Time
		if m.frozen {
			target = m.frozenAt
		} else {
			target = time.Now().Add(m.offset)
		}
		newHandles := c.injectPod(ctx, pod, target, m.frozen)
		if len(newHandles) == 0 {
			continue
		}

		c.mu.Lock()
		if s, ok := c.timeshifts[m.id]; ok {
			retained := s.handles[:0]
			for _, h := range s.handles {
				if h.pod != pod.Name {
					retained = append(retained, h)
				}
			}
			s.handles = append(retained, newHandles...)
		}
		c.mu.Unlock()

		c.log.Info("pod watcher re-injected pod",
			"pod", pod.Namespace+"/"+pod.Name,
			"timeshift_id", m.id[:8],
			"containers", len(newHandles))
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
