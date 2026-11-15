//go:build linux

package faketime

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"github.com/bkaznowski/epochd/pkg/inject"
)

// How often the tracker looks for processes its ptrace events did not report,
// at least: the loop also waits ten times as long as the last sweep took, so a
// tree with thousands of threads is not scanned continuously.
//
// A process that is born and gone between two sweeps is never seen. That costs
// nothing for a plain fork, which starts with a copy of its parent's clock; it
// does mean that a stray that execs (the usual way a service starts a worker)
// reads real time until the next sweep reaches it, up to one interval later.
// Only following every new thread (PTRACE_O_TRACECLONE) would close that gap,
// at the price of a stop for each thread creation.
const strayScanInterval = 100 * time.Millisecond

// A tracker learns of a new child from the PTRACE_EVENT_FORK stop of the
// thread that forked it. That only works for threads the tracer is attached to,
// and a thread the tracee creates after injection is not (following clones
// would mean stopping on every thread start, which servers do constantly). A
// fork from such a thread therefore produces no event, and the child, which
// carries a private copy of the clock's state that nothing would ever update,
// silently stays on whatever time it was born with.
//
// sweepStrays finds those children by asking the kernel which processes each
// tracked task has. Anything that is not ptrace-attached to someone (a child we
// were told about is attached from birth, so it is left to the event path and the
// two cannot both claim it) and that we do not know yet is a stray: it gets the
// current state of its parent's clock. The kernel needs CONFIG_PROC_CHILDREN for
// /proc/<pid>/task/<tid>/children; without it there is nothing to sweep and the
// tracker behaves as it did before.
//
// A stray is not ptrace-attached, so its own fork and exec are not reported.
// Its children are found by the next sweep; one that execs loses its hooks, and
// is noticed the same way and hooked afresh.
func (c *ChildTracker) sweepStrays() {
	c.mu.Lock()
	known := make(map[int]bool, len(c.children)+len(c.pendingStop)+1)
	known[c.parentPID] = true
	for pid := range c.children {
		known[pid] = true
	}
	for pid := range c.pendingStop {
		known[pid] = true
	}
	strays := make([]int, 0, len(c.strays))
	for pid := range c.strays {
		strays = append(strays, pid)
	}
	c.mu.Unlock()

	// Strays that exited, or exec'd and so lost their hooks, are forgotten; an
	// exec'd one that is still around is found again below and hooked afresh.
	for _, pid := range strays {
		if !processRunning(pid) {
			c.forgetStray(pid)
			delete(known, pid)
			continue
		}
		if _, err := inject.Adopt(pid); errors.Is(err, inject.ErrNotInjected) {
			c.forgetStray(pid)
			delete(known, pid)
		}
	}

	for _, parent := range c.trackedPIDs() {
		for _, child := range childrenOf(parent) {
			if known[child] || tracerOf(child) != 0 {
				continue
			}
			c.adoptStray(child, parent)
			known[child] = true
		}
	}
}

// adoptStray hooks child, which parent forked without a ptrace event to say so,
// and gives it parent's clock as it stands now.
func (c *ChildTracker) adoptStray(child, parent int) {
	parentH := c.Handle
	if parent != c.parentPID {
		c.mu.Lock()
		h, ok := c.children[parent]
		c.mu.Unlock()
		if !ok {
			return
		}
		parentH = h
	}
	parentH.mu.Lock()
	frozen := parentH.frozen
	target := parentH.effectiveTime()
	parentH.mu.Unlock()

	// A fork copies the hooks, so Adopt normally finds them and only the state
	// needs bringing up to date. A child that has already exec'd has none.
	ih, err := inject.Adopt(child)
	switch {
	case err == nil:
		if frozen {
			err = ih.Freeze(target)
		} else {
			err = ih.SetTime(target)
		}
	case errors.Is(err, inject.ErrNotInjected):
		if frozen {
			ih, err = inject.InjectFrozen(child, target)
		} else {
			ih, err = inject.InjectAtTime(child, target)
		}
	}
	if err != nil {
		return // gone already, or not ours to touch; the next sweep will try again
	}

	var h *Handle
	if frozen {
		h = newFrozenHandle(ih, target)
	} else {
		h = newAdvancingHandle(ih, target)
	}
	c.mu.Lock()
	c.children[child] = h
	c.strays[child] = true
	c.mu.Unlock()
}

// forgetStray removes a stray from the tracker's books.
func (c *ChildTracker) forgetStray(pid int) {
	c.mu.Lock()
	delete(c.children, pid)
	delete(c.strays, pid)
	c.mu.Unlock()
}

// childrenOf returns the pids of the processes that any thread of pid has forked,
// according to /proc/<pid>/task/<tid>/children.
func childrenOf(pid int) []int {
	files, err := filepath.Glob("/proc/" + strconv.Itoa(pid) + "/task/*/children")
	if err != nil {
		return nil
	}
	var out []int
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		for _, field := range bytes.Fields(b) {
			if n, err := strconv.Atoi(string(field)); err == nil {
				out = append(out, n)
			}
		}
	}
	return out
}

// tracerOf returns the pid of whatever is ptrace-attached to pid, or 0 for
// nothing, and also for a process it cannot read (which is then left alone).
func tracerOf(pid int) int {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/status")
	if err != nil {
		return -1
	}
	for _, line := range bytes.Split(b, []byte("\n")) {
		if rest, ok := bytes.CutPrefix(line, []byte("TracerPid:")); ok {
			n, err := strconv.Atoi(string(bytes.TrimSpace(rest)))
			if err != nil {
				return -1
			}
			return n
		}
	}
	return -1
}

// processRunning reports whether pid exists and is not a zombie, which is gone
// as far as a clock is concerned but still answers kill(pid, 0).
func processRunning(pid int) bool {
	if syscall.Kill(pid, 0) != nil {
		return false
	}
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return false
	}
	// "pid (comm) S ...": comm may hold spaces and parentheses, so look after the last ')'.
	i := bytes.LastIndexByte(b, ')')
	if i < 0 || i+2 >= len(b) {
		return false
	}
	return b[i+2] != 'Z' && b[i+2] != 'X'
}
