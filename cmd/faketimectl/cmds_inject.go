//go:build linux

package main

import (
	"flag"
	"fmt"
	"time"

	"github.com/bkaznowski/epochd/pkg/inject"
)

func cmdInject(args []string) error {
	fs := flag.NewFlagSet("inject", flag.ContinueOnError)
	fs.SetOutput(stderr)
	pid := fs.Int("pid", 0, "target process `PID` (required)")
	timeStr := fs.String("time", "", "fake time in RFC3339, e.g. 2030-01-01T00:00:00Z (required)")
	freeze := fs.Bool("freeze", false, "freeze clock at --time (clock does not advance)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *pid == 0 {
		return fmt.Errorf("inject: --pid is required")
	}
	if *timeStr == "" {
		return fmt.Errorf("inject: --time is required")
	}
	target, err := time.Parse(time.RFC3339, *timeStr)
	if err != nil {
		return fmt.Errorf("inject: --time: %v", err)
	}

	var h *inject.Handle
	if *freeze {
		h, err = inject.InjectFrozen(*pid, target)
	} else {
		h, err = inject.InjectAtTime(*pid, target)
	}
	if err != nil {
		return fmt.Errorf("inject: %v", err)
	}

	mode := "advancing"
	if *freeze {
		mode = "frozen"
	}
	fmt.Fprintf(stdout, "pid %d: clock set to %s (%s, state=0x%x)\n",
		h.PID, target.Format(time.RFC3339), mode, h.StateAddr)
	return nil
}

func cmdReset(args []string) error {
	fs := flag.NewFlagSet("reset", flag.ContinueOnError)
	fs.SetOutput(stderr)
	pid := fs.Int("pid", 0, "target process `PID` (required)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *pid == 0 {
		return fmt.Errorf("reset: --pid is required")
	}
	h, err := inject.InjectAtTime(*pid, time.Now())
	if err != nil {
		return fmt.Errorf("reset: %v", err)
	}
	fmt.Fprintf(stdout, "pid %d: clock reset to real time (state=0x%x)\n", h.PID, h.StateAddr)
	return nil
}
