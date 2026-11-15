//go:build linux

package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/bkaznowski/epochd/pkg/inject"
	"github.com/bkaznowski/epochd/pkg/preload"
	"github.com/bkaznowski/epochd/pkg/procmem"
	"golang.org/x/sys/unix"
)

// Values for run's --mode flag.
const (
	modeVDSO    = "vdso"
	modePreload = "preload"
)

// exitFunc ends the process with the command's exit status. It is a variable so
// tests can observe the status without terminating the test binary.
var exitFunc = os.Exit

// forwardedSignals are relayed to the command run starts. Without this the
// signal would stop faketimectl and leave the command running: `docker stop`
// sends SIGTERM to PID 1, and faketimectl is PID 1 when it is a container's entry
// point.
var forwardedSignals = []os.Signal{syscall.SIGTERM, syscall.SIGHUP, syscall.SIGQUIT, syscall.SIGUSR1, syscall.SIGUSR2}

// forwardSignals starts relaying signals to the command once it is running, and
// returns a function that stops. Call it before starting the command so that a
// signal arriving in between is held, not lost.
//
// SIGINT is relayed only when stdin is not a terminal. From a terminal, Ctrl-C
// already reaches the command (it is in the same foreground process group), and
// relaying it too would deliver it twice, which many programs read as "force
// quit". Without a terminal nothing else delivers it.
func forwardSignals() (start func(p *os.Process), stop func()) {
	sigs := []os.Signal{}
	sigs = append(sigs, forwardedSignals...)
	if fi, err := os.Stdin.Stat(); err != nil || fi.Mode()&os.ModeCharDevice == 0 {
		sigs = append(sigs, os.Interrupt)
	}
	ch := make(chan os.Signal, 16)
	signal.Notify(ch, sigs...)
	done := make(chan struct{})
	start = func(p *os.Process) {
		go func() {
			for {
				select {
				case sig := <-ch:
					p.Signal(sig) //nolint:errcheck // the command may already have exited
				case <-done:
					return
				}
			}
		}()
	}
	var once sync.Once
	stop = func() {
		once.Do(func() {
			signal.Stop(ch)
			close(done)
		})
	}
	return start, stop
}

func cmdRun(args []string) error {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	fs.SetOutput(stderr)
	timeStr := fs.String("time", "", "fake time in RFC3339, e.g. 2030-01-01T00:00:00Z (required)")
	freeze := fs.Bool("freeze", false, "freeze clock at --time (clock does not advance)")
	track := fs.Bool("track", false, "track forked children and re-inject after exec() (vdso mode only; preload mode always covers children)")
	modeName := fs.String("mode", modeVDSO, "how to fake the clock: "+modeVDSO+" (ptrace + vDSO patch; works on any binary, including Go and static ones) "+
		"or "+modePreload+" (LD_PRELOAD; no ptrace, children inherit it, but dynamically linked non-Go binaries only)")
	if err := fs.Parse(args); err != nil {
		return err
	}

	cmdArgs := fs.Args()
	if len(cmdArgs) == 0 {
		return fmt.Errorf("run: a command is required\nusage: faketimectl run --time=RFC3339 [--freeze] [--track] [--mode=vdso|preload] [--] COMMAND [ARGS]")
	}
	if *timeStr == "" {
		return fmt.Errorf("run: --time is required")
	}
	if *modeName != modeVDSO && *modeName != modePreload {
		return fmt.Errorf("run: --mode must be %q or %q, got %q", modeVDSO, modePreload, *modeName)
	}
	target, err := time.Parse(time.RFC3339, *timeStr)
	if err != nil {
		return fmt.Errorf("run: --time: %v", err)
	}
	if *modeName == modePreload {
		return runPreload(cmdArgs, target, *freeze)
	}

	cmd := exec.Command(cmdArgs[0], cmdArgs[1:]...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Ptrace: true}

	startForwarding, stopForwarding := forwardSignals()
	defer stopForwarding()

	// cmd.Start() is not called here: the inject.*FollowChild* functions
	// below start cmd themselves, on the Tracer's own pinned OS thread. The
	// fork (which performs PTRACE_TRACEME) must happen on that same thread as
	// the ptrace requests that follow it, or they intermittently fail with
	// ESRCH depending on Go scheduler behavior.
	var h *inject.Handle
	var tr *procmem.Tracer

	if *track {
		if *freeze {
			h, tr, err = inject.InjectFrozenFollowChildKeepTracer(cmd, target)
		} else {
			h, tr, err = inject.InjectAtTimeFollowChildKeepTracer(cmd, target)
		}
	} else {
		if *freeze {
			h, err = inject.InjectFrozenFollowChild(cmd, target)
		} else {
			h, err = inject.InjectAtTimeFollowChild(cmd, target)
		}
	}
	if err != nil {
		if cmd.Process != nil {
			cmd.Process.Kill() //nolint:errcheck
			cmd.Wait()         //nolint:errcheck
		}
		return fmt.Errorf("run: inject: %v", err)
	}
	pid := cmd.Process.Pid
	startForwarding(cmd.Process)

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
		code, ok := exitCodeFor(waitErr)
		if !ok {
			select {
			case <-done:
			default:
				close(done)
			}
			<-watchDone
			return fmt.Errorf("run: wait: %v", waitErr)
		}
		exitCode = code
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
		stopForwarding()
		exitFunc(exitCode)
	}
	return nil
}

// exitCodeFor maps the error from cmd.Wait to the exit code run should
// propagate. ok is false when waitErr is not a process exit status at all (a
// failure of the wait itself), which the caller should report as an error.
func exitCodeFor(waitErr error) (code int, ok bool) {
	exit, isExit := waitErr.(*exec.ExitError)
	if !isExit {
		return 0, false
	}
	code = exit.ExitCode()
	if code < 0 {
		// Killed by signal — use 128+signum convention.
		if ws, ok := exit.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			return 128 + int(ws.Signal()), true
		}
		return 1, true
	}
	return code, true
}

// runPreload is run's --mode=preload path: instead of ptracing the command and
// patching its vDSO, it starts the command with the epochd preload library in
// its environment. No tracer is involved, so --track has nothing to do: every
// descendant that keeps the environment inherits the clock across fork and
// exec on its own. Binaries LD_PRELOAD cannot affect (Go, static) are refused
// up front rather than silently left on real time.
func runPreload(cmdArgs []string, target time.Time, freeze bool) error {
	cmd := exec.Command(cmdArgs[0], cmdArgs[1:]...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	startForwarding, stopForwarding := forwardSignals()
	defer stopForwarding()

	var h *preload.Handle
	var err error
	if freeze {
		h, err = preload.StartFrozen(cmd, target)
	} else {
		h, err = preload.Start(cmd, target)
	}
	if err != nil {
		if errors.Is(err, preload.ErrNotInterposable) {
			return fmt.Errorf("run: %v\nrun: use --mode=%s for this binary", err, modeVDSO)
		}
		return fmt.Errorf("run: preload: %v", err)
	}

	startForwarding(cmd.Process)

	clockMode := "advancing"
	if freeze {
		clockMode = "frozen"
	}
	fmt.Fprintf(stderr, "faketimectl: pid %d: clock set to %s (%s, preload; children inherit it)\n",
		cmd.Process.Pid, target.Format(time.RFC3339), clockMode)

	exitCode := 0
	if waitErr := cmd.Wait(); waitErr != nil {
		code, ok := exitCodeFor(waitErr)
		if !ok {
			h.Close() //nolint:errcheck
			return fmt.Errorf("run: wait: %v", waitErr)
		}
		exitCode = code
	}
	// Close before os.Exit below, which would skip a deferred call.
	h.Close() //nolint:errcheck

	if exitCode != 0 {
		stopForwarding()
		exitFunc(exitCode)
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
		tr.Close()           // release the tracer's pinned goroutine and OS thread
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
