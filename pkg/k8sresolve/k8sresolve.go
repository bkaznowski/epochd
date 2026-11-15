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
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// procRoot is where process information is read from. It is a variable so
// tests can point it at a fake tree.
var procRoot = "/proc"

// LookupPID returns the init PID of the container identified by containerID:
// the first process in LookupPIDs' order.
func LookupPID(containerID string) (int, error) {
	pids, err := LookupPIDs(containerID)
	if err != nil {
		return 0, err
	}
	return pids[0], nil
}

// LookupPIDs returns every process in the container identified by containerID,
// the container's init process first.
//
// containerID may include a runtime prefix ("containerd://abc123...",
// "docker://abc123...") or be a bare hex container ID. The lookup matches the
// first 20 characters of the ID against each process's /proc/<pid>/cgroup
// content. A container is usually more than its init process (preforked
// workers, helpers started by an entrypoint script), and faking only the first
// would leave the rest on real time.
//
// Processes are ordered by when they started, not by pid number: pids wrap, so
// a process started later can carry a lower number than the init that spawned
// it. Processes whose start time cannot be read sort last, by pid.
//
// The agent must run with hostPID: true so that /proc exposes all node
// processes, not just those in its own PID namespace.
func LookupPIDs(containerID string) ([]int, error) {
	id := stripRuntimePrefix(containerID)
	if len(id) < 8 {
		return nil, fmt.Errorf("k8sresolve: container ID too short: %q", containerID)
	}

	// Use a 20-char prefix as the discriminator — long enough to be unique,
	// short enough that truncated IDs from older k8s versions still match.
	prefix := id
	if len(prefix) > 20 {
		prefix = prefix[:20]
	}

	entries, err := os.ReadDir(procRoot)
	if err != nil {
		return nil, fmt.Errorf("k8sresolve: ReadDir %s: %w", procRoot, err)
	}

	type proc struct {
		pid   int
		start uint64
	}
	var procs []proc
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid <= 1 {
			continue // skip non-PID entries and PID 1 (the node's init)
		}
		cgroup, err := os.ReadFile(filepath.Join(procRoot, e.Name(), "cgroup"))
		if err != nil {
			continue // process may have exited between ReadDir and ReadFile
		}
		if strings.Contains(string(cgroup), prefix) {
			procs = append(procs, proc{pid, startTime(pid)})
		}
	}

	if len(procs) == 0 {
		return nil, fmt.Errorf("k8sresolve: no process found for container %s", displayID(id))
	}

	sort.Slice(procs, func(i, j int) bool {
		if procs[i].start != procs[j].start {
			return procs[i].start < procs[j].start
		}
		return procs[i].pid < procs[j].pid
	})
	pids := make([]int, len(procs))
	for i, p := range procs {
		pids[i] = p.pid
	}
	return pids, nil
}

// startTime returns the process's start time in clock ticks since boot (field 22
// of /proc/<pid>/stat), or MaxUint64 if it cannot be read.
func startTime(pid int) uint64 {
	b, err := os.ReadFile(filepath.Join(procRoot, strconv.Itoa(pid), "stat"))
	if err != nil {
		return math.MaxUint64
	}
	// The command name is parenthesised and may itself contain spaces and
	// parentheses, so count fields from after the last ')'. What follows starts
	// at field 3 (state); starttime is field 22.
	s := string(b)
	i := strings.LastIndexByte(s, ')')
	if i < 0 {
		return math.MaxUint64
	}
	fields := strings.Fields(s[i+1:])
	const startIdx = 22 - 3
	if len(fields) <= startIdx {
		return math.MaxUint64
	}
	v, err := strconv.ParseUint(fields[startIdx], 10, 64)
	if err != nil {
		return math.MaxUint64
	}
	return v
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
