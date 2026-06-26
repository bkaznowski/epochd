package main

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

const (
	storeConfigMapName = "epochd-state"
	storeDataKey       = "state"
)

// store persists the timeshift registry to a ConfigMap so the controller can
// recover its active injections after a pod restart.
type store struct {
	k8s       kubernetes.Interface
	namespace string
}

func newStore(k8s kubernetes.Interface, namespace string) *store {
	return &store{k8s: k8s, namespace: namespace}
}

// storedHandle is the JSON-serialisable form of containerHandle.
type storedHandle struct {
	Pod         string `json:"pod"`
	Container   string `json:"container"`
	NodeIP      string `json:"nodeIP"`
	ContainerID string `json:"containerID"`
	AgentHandle string `json:"agentHandle"`
}

// storedTimeshift is the JSON-serialisable form of timeshift.
type storedTimeshift struct {
	ID            string         `json:"id"`
	Namespace     string         `json:"namespace"`
	LabelSelector string         `json:"labelSelector"`
	Offset        int64          `json:"offset"`             // nanoseconds; advancing: fake_time = now + offset
	FrozenAt      time.Time      `json:"frozenAt,omitempty"` // frozen mode only
	Frozen        bool           `json:"frozen,omitempty"`
	TTL           time.Duration  `json:"ttl"`
	ExpiresAt     time.Time      `json:"expiresAt"`
	CreatedAt     time.Time      `json:"createdAt"`
	Handles       []storedHandle `json:"handles"`
}

// encode serialises timeshifts to JSON bytes. The caller must hold at least a
// read lock on the controller mutex to prevent concurrent map modification.
func (s *store) encode(timeshifts map[string]*timeshift) ([]byte, error) {
	stored := make([]storedTimeshift, 0, len(timeshifts))
	for _, ts := range timeshifts {
		handles := make([]storedHandle, len(ts.handles))
		for i, h := range ts.handles {
			handles[i] = storedHandle{
				Pod:         h.pod,
				Container:   h.container,
				NodeIP:      h.nodeIP,
				ContainerID: h.containerID,
				AgentHandle: h.agentHandle,
			}
		}
		stored = append(stored, storedTimeshift{
			ID:            ts.id,
			Namespace:     ts.namespace,
			LabelSelector: ts.labelSelector,
			Offset:        int64(ts.offset),
			FrozenAt:      ts.frozenAt,
			Frozen:        ts.frozen,
			TTL:           ts.ttl,
			ExpiresAt:     ts.expiresAt,
			CreatedAt:     ts.createdAt,
			Handles:       handles,
		})
	}
	return json.Marshal(stored)
}

// flush writes pre-encoded state bytes to the backing ConfigMap, creating it if
// it does not yet exist. Safe to call without holding any controller lock.
func (s *store) flush(ctx context.Context, data []byte) error {
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      storeConfigMapName,
			Namespace: s.namespace,
		},
		Data: map[string]string{storeDataKey: string(data)},
	}
	_, err := s.k8s.CoreV1().ConfigMaps(s.namespace).Get(ctx, storeConfigMapName, metav1.GetOptions{})
	if k8serrors.IsNotFound(err) {
		_, err = s.k8s.CoreV1().ConfigMaps(s.namespace).Create(ctx, cm, metav1.CreateOptions{})
	} else if err == nil {
		_, err = s.k8s.CoreV1().ConfigMaps(s.namespace).Update(ctx, cm, metav1.UpdateOptions{})
	}
	if err != nil {
		return fmt.Errorf("store: flush: %w", err)
	}
	return nil
}

// save encodes timeshifts and writes them to the ConfigMap in one call.
// Production code splits encode (under read lock) and flush (outside lock) via
// the persist method; save is provided for tests.
func (s *store) save(ctx context.Context, timeshifts map[string]*timeshift) error {
	data, err := s.encode(timeshifts)
	if err != nil {
		return fmt.Errorf("store: encode: %w", err)
	}
	return s.flush(ctx, data)
}

// load reads the ConfigMap and decodes the timeshift registry. Returns an empty
// map without error when the ConfigMap does not yet exist (first run).
func (s *store) load(ctx context.Context) (map[string]*timeshift, error) {
	cm, err := s.k8s.CoreV1().ConfigMaps(s.namespace).Get(ctx, storeConfigMapName, metav1.GetOptions{})
	if k8serrors.IsNotFound(err) {
		return make(map[string]*timeshift), nil
	}
	if err != nil {
		return nil, fmt.Errorf("store: load: %w", err)
	}

	raw, ok := cm.Data[storeDataKey]
	if !ok || raw == "" {
		return make(map[string]*timeshift), nil
	}

	var stored []storedTimeshift
	if err := json.Unmarshal([]byte(raw), &stored); err != nil {
		return nil, fmt.Errorf("store: unmarshal: %w", err)
	}

	result := make(map[string]*timeshift, len(stored))
	for _, st := range stored {
		handles := make([]containerHandle, len(st.Handles))
		for i, h := range st.Handles {
			handles[i] = containerHandle{
				pod:         h.Pod,
				container:   h.Container,
				nodeIP:      h.NodeIP,
				containerID: h.ContainerID,
				agentHandle: h.AgentHandle,
			}
		}
		ts := &timeshift{
			id:            st.ID,
			namespace:     st.Namespace,
			labelSelector: st.LabelSelector,
			offset:        time.Duration(st.Offset),
			frozenAt:      st.FrozenAt,
			frozen:        st.Frozen,
			ttl:           st.TTL,
			expiresAt:     st.ExpiresAt,
			createdAt:     st.CreatedAt,
			handles:       handles,
		}
		result[ts.id] = ts
	}
	return result, nil
}
