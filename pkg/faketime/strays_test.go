//go:build linux

package faketime

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"sync"
	"syscall"
	"testing"
	"time"
)

const forkerEnv = "EPOCHD_FAKETIME_FORKER"

const forkerChildren = 8

// TestFaketimeForkerHelper is the parent process for the stray-child test. It
// forks long-lived clock-printing children, each from a thread of its own: the
// goroutine is locked to its OS thread, and so many are blocked at once that
// nearly all of those threads are created after the tracer attached, which means
// the tracer has no PTRACE_EVENT_FORK for them.
func TestFaketimeForkerHelper(t *testing.T) {
	if os.Getenv(forkerEnv) != "1" {
		t.Skip()
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond) // past the tracker's attach

	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < forkerChildren; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			runtime.LockOSThread() // the fork below happens on this thread
			<-start
			cmd := exec.Command(exe, "-test.run=TestFaketimeHelper", "-test.v")
			cmd.Env = append(os.Environ(), helperEnv+"=1", forkerEnv+"=0")
			cmd.Stdout = os.Stdout
			if err := cmd.Start(); err != nil {
				fmt.Println("forker: start:", err)
				return
			}
			cmd.Wait() //nolint:errcheck
		}()
	}
	time.Sleep(300 * time.Millisecond) // let every goroutine take its own thread
	close(start)
	wg.Wait()
}

// A child forked from a thread the tracer is not attached to gets no
// PTRACE_EVENT_FORK, so it would keep the clock it was born with: a SetTime on
// the tracker would reach its siblings and miss it.
func TestTrackerFollowsChildrenForkedFromUnattachedThreads(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer pr.Close()

	const (
		firstOffset  = 24 * time.Hour
		secondOffset = 48 * time.Hour
		tolerance    = 10 * time.Second
	)
	cmd := exec.Command(exe, "-test.run=TestFaketimeForkerHelper", "-test.v")
	cmd.Env = append(os.Environ(), forkerEnv+"=1")
	cmd.Stdout = pw
	ct, err := StartWithTracking(cmd, time.Now().Add(firstOffset))
	pw.Close()
	if err != nil {
		t.Fatalf("StartWithTracking: %v", err)
	}
	t.Cleanup(func() {
		for _, pid := range ct.PIDs() { // the parent and every child, strays included
			syscall.Kill(pid, syscall.SIGKILL) //nolint:errcheck
		}
		ct.Close() //nolint:errcheck
	})

	// Wait for every child to be known, however it was found.
	deadline := time.Now().Add(15 * time.Second)
	for len(ct.Children()) < forkerChildren {
		if time.Now().After(deadline) {
			t.Fatalf("only %d/%d children were ever tracked", len(ct.Children()), forkerChildren)
		}
		time.Sleep(20 * time.Millisecond)
	}
	ct.mu.Lock()
	strays := len(ct.strays)
	ct.mu.Unlock()
	t.Logf("%d of %d children were found by the sweep (the rest by fork events)", strays, forkerChildren)
	if strays == 0 {
		t.Skip("every child happened to fork from an attached thread; nothing to exercise")
	}

	if err := ct.SetTime(time.Now().Add(secondOffset)); err != nil {
		t.Fatalf("SetTime: %v", err)
	}

	// Every child must follow. A child still on the old clock prints a line near
	// +24h; once all have moved, several rounds in a row are entirely near +48h.
	// A stray's update is applied by the sweep, hence the allowance in time. A
	// child that has just exec'd is on real time until the sweep hooks it (see
	// strayScanInterval), so a line near now counts as "not yet" too.
	const need = 3 * forkerChildren
	sc := bufio.NewScanner(pr)
	fresh, stale := 0, 0
	timeout := time.AfterFunc(10*time.Second, func() { pr.Close() })
	defer timeout.Stop()
	for sc.Scan() {
		ts, ok := parseFaketimeTimestamp(sc.Text())
		if !ok {
			continue
		}
		d := time.Until(ts)
		switch {
		case d > secondOffset-tolerance && d < secondOffset+tolerance:
			fresh++
		case d > firstOffset-tolerance && d < firstOffset+tolerance,
			d > -tolerance && d < tolerance:
			stale, fresh = stale+1, 0 // someone is still on the old clock: start counting again
		default:
			t.Fatalf("timestamp %v is %v from now, near no offset the test set", ts, d)
		}
		if fresh >= need {
			return
		}
	}
	t.Fatalf("children did not all follow the clock to +48h within 10s (%d stale lines seen, %d fresh in a row)", stale, fresh)
}
