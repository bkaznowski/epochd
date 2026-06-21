//go:build linux

// Command faketimectl injects a fake wall-clock time into a running process
// and optionally resets it back to the real clock. Uses the vDSO hook approach
// implemented in pkg/inject.
//
// Usage:
//
//	faketimectl --pid=<PID> --set-time=<RFC3339>   # inject fake time
//	faketimectl --pid=<PID> --reset                 # snap back to real clock
//
// The command requires ptrace permission over the target process (run as root
// or with CAP_SYS_PTRACE, and ensure /proc/sys/kernel/yama/ptrace_scope <= 1).
package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"time"

	"epochd/pkg/inject"
)

func main() {
	log.SetFlags(0)
	log.SetPrefix("faketimectl: ")

	pid := flag.Int("pid", 0, "target process `PID` (required)")
	setTimeStr := flag.String("set-time", "", "fake wall-clock time in RFC3339 (e.g. 2030-01-01T00:00:00Z)")
	reset := flag.Bool("reset", false, "snap the target process back to the real clock")
	flag.Parse()

	if *pid == 0 {
		fmt.Fprintln(os.Stderr, "faketimectl: --pid is required")
		flag.Usage()
		os.Exit(1)
	}
	if *reset && *setTimeStr != "" {
		fmt.Fprintln(os.Stderr, "faketimectl: --reset and --set-time are mutually exclusive")
		os.Exit(1)
	}
	if !*reset && *setTimeStr == "" {
		fmt.Fprintln(os.Stderr, "faketimectl: one of --set-time or --reset is required")
		flag.Usage()
		os.Exit(1)
	}

	var target time.Time
	if *reset {
		target = time.Now()
	} else {
		var err error
		target, err = time.Parse(time.RFC3339, *setTimeStr)
		if err != nil {
			log.Fatalf("--set-time: %v", err)
		}
	}

	// InjectAtTime attaches, writes the trampoline, patches the vDSO, and
	// detaches. Calling it a second time on an already-injected process is safe:
	// the target is ptrace-stopped during the patch, so there is no race; the
	// old trampoline page is leaked but that is acceptable for a testing tool.
	h, err := inject.InjectAtTime(*pid, target)
	if err != nil {
		log.Fatalf("inject: %v", err)
	}

	if *reset {
		fmt.Printf("pid %d: clock reset to real time (state=0x%x)\n", h.PID, h.StateAddr)
	} else {
		offsetFromNow := target.Sub(time.Now()).Round(time.Second)
		fmt.Printf("pid %d: clock set to %s (%+v from now, state=0x%x)\n",
			h.PID, target.Format(time.RFC3339), offsetFromNow, h.StateAddr)
	}
}
