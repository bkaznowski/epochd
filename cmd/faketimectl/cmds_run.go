//go:build linux

package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"time"

	"github.com/bkaznowski/epochd/pkg/inject"
	"github.com/bkaznowski/epochd/pkg/procmem"
	"golang.org/x/sys/unix"
)

func cmdRun(args []string) error {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	fs.SetOutput(stderr)
	timeStr := fs.String("time", "", "fake time in RFC3339, e.g. 2030-01-01T00:00:00Z (required)")
	freeze := fs.Bool("freeze", false, "freeze clock at --time (clock does not advance)")
	track := fs.Bool("track", false, "track forked children and re-inject after exec()")
	if err := fs.Parse(args); err != nil {
		return err
	}

	cmdArgs := fs.Args()
	if len(cmdArgs) == 0 {
		return fmt.Errorf("run: a command is required\nusage: faketimectl run --time=RFC3339 [--freeze] [--track] [--] COMMAND [ARGS]")
	}
	if *timeStr == "" {
		return fmt.Errorf("run: --time is required")
	}
	target, err := time.Parse(time.RFC3339, *timeStr)
	if err != nil {
		return fmt.Errorf("run: --time: %v", err)
	}

	cmd := exec.Command(cmdArgs[0], cmdArgs[1:]...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Ptrace: true}

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("run: %v", err)
	}
	pid := cmd.Process.Pid

	var h *inject.Handle
	var tr *procmem.Tracer

	if *track {
		if *freeze {
			h, tr, err = inject.InjectFrozenFollowChildKeepTracer(pid, target)
		} else {
			h, tr, err = inject.InjectAtTimeFollowChildKeepTracer(pid, target)
		}
	} else {
		if *freeze {
			h, err = inject.InjectFrozenFollowChild(pid, target)
		} else {
			h, err = inject.InjectAtTimeFollowChild(pid, target)
		}
	}
	if err != nil {
		cmd.Process.Kill() //nolint:errcheck
		cmd.Wait()         //nolint:errcheck
		return fmt.Errorf("run: inject: %v", err)
	}

	mode := "advancing"
	if *freeze {
		mode = "frozen"
	}
	if *track {
		fmt.Fprintf(stderr, "faketimectl: pid %d: clock set to %s (%s, tracking children)\n",
			pid, target.Format(time.RFC3339), mode)
	} else {
		fmt.Fprintf(stderr, "faketimectl: pid %d: clock set to %s (%s)\n",
			pid, target.Format(time.RFC3339), mode)
	}

	// For the tracking case: run the watch loop in the background while the
	// parent process runs. injectOffset captures the advancing offset at the
	// moment of injection so re-injections after exec() use the correct
	// effective fake time rather than the original snapshot.
	done := make(chan struct{})
	watchDone := make(chan error, 1)
	if *track {
		injectOffset := time.Until(target)
		go func() {
			watchDone <- runWatchLoop(tr, pid, h, target, injectOffset, *freeze, done)
		}()
	} else {
		close(done)
		watchDone <- nil
	}

	exitCode := 0
	if waitErr := cmd.Wait(); waitErr != nil {
		if exit, ok := waitErr.(*exec.ExitError); ok {
			exitCode = exit.ExitCode()
			if exitCode < 0 {
				// Killed by signal — use 128+signum convention.
				if ws, ok := exit.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
					exitCode = 128 + int(ws.Signal())
				} else {
					exitCode = 1
				}
			}
		} else {
			select {
			case <-done:
			default:
				close(done)
			}
			<-watchDone
			return fmt.Errorf("run: wait: %v", waitErr)
		}
	}

	// Signal the watch loop to stop and wait for its cleanup to finish.
	select {
	case <-done:
	default:
		close(done)
	}
	if we := <-watchDone; we != nil {
		fmt.Fprintf(stderr, "faketimectl: watch: %v\n", we)
	}

	if exitCode != 0 {
		os.Exit(exitCode)
	}
	return nil
}

// runWatchLoop handles PTRACE_EVENT_FORK, PTRACE_EVENT_VFORK, and
// PTRACE_EVENT_EXEC notifications for all processes under tr.
//
// For forks: a Handle is registered for the child using inject.ChildHandle
// (no re-injection needed — fork inherits the trampoline page).
//
// For execs: the process is fully re-injected (new mmap, new trampoline, new
// vDSO patch) before user code runs. effectiveTarget for advancing mode is
// time.Now().Add(injectOffset) so the fake clock continues smoothly.
//
// Runs until done is closed or ECHILD is received.
func runWatchLoop(
	tr *procmem.Tracer,
	parentPID int,
	parentIH *inject.Handle,
	target time.Time,
	injectOffset time.Duration,
	frozen bool,
	done <-chan struct{},
) error {
	childHandles := make(map[int]*inject.Handle)
	pendingStop := make(map[int]bool)

	defer func() {
		pids := make([]int, 0, len(childHandles))
		for pid := range childHandles {
			pids = append(pids, pid)
		}
		tr.DetachAll(pids)   //nolint:errcheck
		tr.InterruptDetach() //nolint:errcheck
	}()

	for {
		select {
		case <-done:
			return nil
		default:
		}

		pid, ws, err := tr.WaitAnyNonBlocking()
		if err != nil {
			if errors.Is(err, syscall.ECHILD) {
				return nil
			}
			return fmt.Errorf("wait: %w", err)
		}
		if pid == 0 {
			time.Sleep(5 * time.Millisecond)
			continue
		}

		if ws.Exited() || ws.Signaled() {
			delete(childHandles, pid)
			delete(pendingStop, pid)
			continue
		}
		if !ws.Stopped() {
			continue
		}

		ptraceEvent := (int(ws) >> 16) & 0xFF
		isSIGTRAP := ws.StopSignal() == syscall.SIGTRAP
		isFork := isSIGTRAP && (ptraceEvent == unix.PTRACE_EVENT_FORK || ptraceEvent == unix.PTRACE_EVENT_VFORK)
		isExec := isSIGTRAP && ptraceEvent == unix.PTRACE_EVENT_EXEC

		if isFork {
			childPIDMsg, forkErr := tr.GetEventMsgPID(pid)
			if forkErr != nil {
				tr.ContPID(pid, 0) //nolint:errcheck
				continue
			}
			childPID := int(childPIDMsg) //nolint:gosec // PID fits in int on x86-64

			var srcHandle *inject.Handle
			if pid == parentPID {
				srcHandle = parentIH
			} else {
				srcHandle = childHandles[pid]
			}
			if srcHandle == nil {
				tr.ContPID(pid, 0) //nolint:errcheck
				continue
			}

			childHandles[childPID] = inject.ChildHandle(srcHandle, childPID)
			pendingStop[childPID] = true
			fmt.Fprintf(stderr, "faketimectl: fork: pid %d -> child %d\n", pid, childPID)
			tr.ContPID(pid, 0) //nolint:errcheck
			continue
		}

		if isExec {
			var effectiveTarget time.Time
			if frozen {
				effectiveTarget = target
			} else {
				// Advancing: current fake time = time.Now() + original offset.
				effectiveTarget = time.Now().Add(injectOffset)
			}

			var newIH *inject.Handle
			var injErr error
			if frozen {
				newIH, injErr = inject.ReInjectFrozenAfterExec(tr, parentPID, pid, effectiveTarget)
			} else {
				newIH, injErr = inject.ReInjectAtTimeAfterExec(tr, parentPID, pid, effectiveTarget)
			}
			if injErr != nil {
				fmt.Fprintf(stderr, "faketimectl: exec re-inject pid %d: %v\n", pid, injErr)
				tr.ContPID(pid, 0) //nolint:errcheck
				continue
			}

			if pid == parentPID {
				parentIH = newIH
			} else {
				childHandles[pid] = newIH
			}
			fmt.Fprintf(stderr, "faketimectl: exec: re-injected pid %d\n", pid)
			tr.ContPID(pid, 0) //nolint:errcheck
			continue
		}

		// Child's initial implicit SIGSTOP (auto-attached by PTRACE_O_TRACEFORK).
		// Arm fork/exec tracking on it then resume.
		if pendingStop[pid] {
			delete(pendingStop, pid)
			tr.SetOptionsPID(pid, //nolint:errcheck
				unix.PTRACE_O_TRACEFORK|unix.PTRACE_O_TRACEVFORK|unix.PTRACE_O_TRACEEXEC)
			tr.ContPID(pid, 0) //nolint:errcheck
			continue
		}

		// Any other stop — forward real signals, suppress SIGTRAP ptrace events.
		sig := 0
		if ws.StopSignal() != syscall.SIGTRAP {
			sig = int(ws.StopSignal())
		}
		tr.ContPID(pid, sig) //nolint:errcheck
	}
}
