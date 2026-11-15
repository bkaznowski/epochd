//go:build linux

package main

import (
	"time"

	"github.com/bkaznowski/epochd/pkg/inject"
	"github.com/bkaznowski/epochd/pkg/k8sresolve"
)

// realInjector is the production injector: it finds a container's processes
// through /proc and hooks them with ptrace.
type realInjector struct{}

func (realInjector) pids(containerID string) ([]int, error) {
	return k8sresolve.LookupPIDs(containerID)
}

func (realInjector) inject(pid int, target time.Time, freeze bool) (hook, error) {
	var (
		h   *inject.Handle
		err error
	)
	if freeze {
		h, err = inject.InjectFrozen(pid, target)
	} else {
		// InjectAtTime attaches via PTRACE_SEIZE (stopping every thread of the
		// process), writes the trampoline, patches the vDSO, and detaches. A process
		// that already carries the hooks, such as a fork of a hooked one, has them
		// reused. It converts target→offset right before writing.
		h, err = inject.InjectAtTime(pid, target)
	}
	if err != nil {
		return nil, err
	}
	return realHook{h}, nil
}

func (realInjector) adopt(pid int) (hook, error) {
	h, err := inject.Adopt(pid)
	if err != nil {
		return nil, err
	}
	return realHook{h}, nil
}

// realHook adapts *inject.Handle (whose PID is a field) to the hook interface.
type realHook struct{ h *inject.Handle }

func (r realHook) Pid() int                       { return r.h.PID }
func (r realHook) SetTime(target time.Time) error { return r.h.SetTime(target) }
func (r realHook) Freeze(target time.Time) error  { return r.h.Freeze(target) }
func (r realHook) Uninstall() error               { return r.h.Uninstall() }
func (r realHook) Generation() uint32             { return r.h.Generation() }
func (r realHook) StateAddr() uintptr             { return r.h.StateAddr }
