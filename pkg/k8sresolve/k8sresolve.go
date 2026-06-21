//go:build linux

// Package k8sresolve maps a Kubernetes container ID to the host PID of the
// container's init process by scanning /proc/*/cgroup.
//
// This approach requires no dependency on the container runtime (containerd,
// Docker, CRI-O) and works as long as the caller has hostPID access to see all
// processes on the node (which the agent DaemonSet provides via hostPID: true).
//
// Future: swap LookupPID for a CRI-socket implementation (k8s.io/cri-api) if
// /proc scanning proves unreliable in a particular runtime configuration.
package k8sresolve

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// LookupPID returns the init PID of the container identified by containerID.
//
// containerID may include a runtime prefix ("containerd://abc123...",
// "docker://abc123...") or be a bare hex container ID. The lookup matches the
// first 20 characters of the ID against each process's /proc/<pid>/cgroup
// content, then returns the lowest-numbered matching PID, which is the
// container's init process (all subsequent processes in the container are
// descendants with higher PIDs).
//
// The agent must run with hostPID: true so that /proc exposes all node
// processes, not just those in its own PID namespace.
func LookupPID(containerID string) (int, error) {
	id := stripRuntimePrefix(containerID)
	if len(id) < 8 {
		return 0, fmt.Errorf("k8sresolve: container ID too short: %q", containerID)
	}

	// Use a 20-char prefix as the discriminator — long enough to be unique,
	// short enough that truncated IDs from older k8s versions still match.
	prefix := id
	if len(prefix) > 20 {
		prefix = prefix[:20]
	}

	entries, err := os.ReadDir("/proc")
	if err != nil {
		return 0, fmt.Errorf("k8sresolve: ReadDir /proc: %w", err)
	}

	var pids []int
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid <= 1 {
			continue // skip non-PID entries and PID 1 (the node's init)
		}
		cgroup, err := os.ReadFile(filepath.Join("/proc", e.Name(), "cgroup"))
		if err != nil {
			continue // process may have exited between ReadDir and ReadFile
		}
		if strings.Contains(string(cgroup), prefix) {
			pids = append(pids, pid)
		}
	}

	if len(pids) == 0 {
		return 0, fmt.Errorf("k8sresolve: no process found for container %s", displayID(id))
	}

	sort.Ints(pids)
	return pids[0], nil // lowest PID = container init process
}

// stripRuntimePrefix removes a "<runtime>://" prefix if present.
func stripRuntimePrefix(id string) string {
	if i := strings.Index(id, "://"); i >= 0 {
		return id[i+3:]
	}
	return id
}

// displayID returns a short printable version of a container ID for error messages.
func displayID(id string) string {
	if len(id) > 12 {
		return id[:12] + "..."
	}
	return id
}
