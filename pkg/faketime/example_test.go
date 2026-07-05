//go:build linux

package faketime_test

import (
	"fmt"
	"os/exec"
	"testing"
	"time"

	"github.com/bkaznowski/epochd/pkg/faketime"
)

// Inject a fake clock into a child process. The process sees time.Now()
// returning values close to target from the moment it starts executing.
func ExampleStart() {
	target := time.Date(2030, 6, 1, 12, 0, 0, 0, time.UTC)
	cmd := exec.Command("./my-service", "--port=8080")

	h, err := faketime.Start(cmd, target)
	if err != nil {
		panic(err)
	}
	defer h.Reset()
	defer cmd.Process.Kill()
	defer cmd.Wait() //nolint:errcheck

	// The process's clock advances in real time from target onwards.
	// Shift the clock forward by one hour while the process is running.
	if err := h.Advance(time.Hour); err != nil {
		panic(err)
	}
}

// Freeze a process's clock at a fixed instant. Every call to clock_gettime
// in the process returns exactly target, no matter how much real time passes.
func ExampleStartFrozen() {
	frozen := time.Date(2030, 12, 31, 23, 59, 59, 0, time.UTC)
	cmd := exec.Command("./my-service")

	h, err := faketime.StartFrozen(cmd, frozen)
	if err != nil {
		panic(err)
	}
	defer h.Reset()
	defer cmd.Process.Kill()
	defer cmd.Wait() //nolint:errcheck

	// Advance the frozen instant by one billing cycle.
	if err := h.Advance(30 * 24 * time.Hour); err != nil {
		panic(err)
	}

	// Switch back to advancing mode at the new time.
	if err := h.SetTime(frozen.Add(30 * 24 * time.Hour)); err != nil {
		panic(err)
	}
}

// Advance the fake clock by a duration without changing mode.
// For advancing clocks the offset shifts; for frozen clocks the pinned
// instant shifts. In both cases the clock mode is preserved.
func ExampleHandle_Advance() {
	target := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	cmd := exec.Command("./my-service")
	h, err := faketime.StartFrozen(cmd, target)
	if err != nil {
		panic(err)
	}
	defer h.Reset()
	defer cmd.Process.Kill()
	defer cmd.Wait() //nolint:errcheck

	// Simulate discrete time steps without leaving frozen mode.
	for i := 0; i < 3; i++ {
		if err := h.Advance(30 * 24 * time.Hour); err != nil {
			panic(err)
		}
		// ... assert billing logic here ...
	}
}

// Switch the clock between frozen and advancing modes while the process runs.
func ExampleHandle_Freeze() {
	target := time.Date(2030, 6, 1, 0, 0, 0, 0, time.UTC)
	cmd := exec.Command("./my-service")
	h, err := faketime.Start(cmd, target)
	if err != nil {
		panic(err)
	}
	defer h.Reset()
	defer cmd.Process.Kill()
	defer cmd.Wait() //nolint:errcheck

	// Pin the clock just before a deadline.
	deadline := time.Date(2030, 6, 30, 23, 59, 59, 0, time.UTC)
	if err := h.Freeze(deadline); err != nil {
		panic(err)
	}

	// Resume advancing from that point.
	if err := h.SetTime(deadline); err != nil {
		panic(err)
	}
}

// Session synchronises fake time across multiple processes.
// All handles are updated concurrently so the inter-process race window is
// as small as possible.
func ExampleSession() {
	target := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	s := faketime.NewSession(target)

	cmd1 := exec.Command("./service-a")
	cmd2 := exec.Command("./service-b")
	if err := s.Start(cmd1); err != nil {
		panic(err)
	}
	if err := s.Start(cmd2); err != nil {
		panic(err)
	}
	defer s.Reset()   //nolint:errcheck
	defer cmd1.Process.Kill()
	defer cmd1.Wait() //nolint:errcheck
	defer cmd2.Process.Kill()
	defer cmd2.Wait() //nolint:errcheck

	// Both processes jump to the new time atomically (one process_vm_writev each).
	if err := s.Advance(365 * 24 * time.Hour); err != nil {
		panic(err)
	}
}

// WithProcess is the idiomatic way to inject time in a Go test. Cleanup
// (kill, wait, reset) runs automatically via t.Cleanup, even if the test calls
// t.Fatal.
//
// In a real test t comes from the testing framework; the nil here is for
// illustration only — this example is not executed by go test.
func ExampleWithProcess() {
	var t *testing.T // replaced by the real *testing.T in your test function
	target := time.Date(2030, 6, 1, 0, 0, 0, 0, time.UTC)
	cmd := exec.Command("./my-service")

	faketime.WithProcess(t, cmd, target, func(t *testing.T, h *faketime.Handle) {
		// The process is running with a fake clock here.
		if err := h.Advance(24 * time.Hour); err != nil {
			t.Fatal(err)
		}
		// ... make HTTP calls, assert behaviour ...
	})
	// cmd is killed and clock is reset here.
}

// StartWithTracking is the idiomatic way to test a process that spawns children
// (e.g. Postgres forking a backend per connection). Each forked child
// automatically inherits the fake clock without any extra setup.
func ExampleStartWithTracking() {
	target := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)

	// Start the parent (e.g. a Postgres postmaster) with fake time.
	ct, err := faketime.StartWithTracking(exec.Command("./postmaster"), target)
	if err != nil {
		panic(err)
	}
	defer ct.Reset() //nolint:errcheck
	defer ct.Close() //nolint:errcheck

	// Advance the clock for the parent and all forked children in one call.
	if err := ct.Advance(24 * time.Hour); err != nil {
		panic(err)
	}

	// Inspect what the tracker is currently doing.
	fmt.Println("frozen:", ct.IsFrozen())
	fmt.Println("tracked PIDs:", ct.PIDs())
	fmt.Println("fake time:", ct.Handle.EffectiveTime().Format(time.RFC3339))
}

// Handle_IsFrozen shows how to use IsFrozen and EffectiveTime for assertions
// in tests — e.g. to verify the service under test is receiving the expected
// clock value before an operation that depends on it.
func ExampleHandle_IsFrozen() {
	target := time.Date(2030, 6, 30, 23, 59, 59, 0, time.UTC)
	cmd := exec.Command("./my-service")
	h, err := faketime.StartFrozen(cmd, target)
	if err != nil {
		panic(err)
	}
	defer h.Reset()
	defer cmd.Process.Kill()
	defer cmd.Wait() //nolint:errcheck

	fmt.Println("frozen:", h.IsFrozen())           // true
	fmt.Println("at:", h.EffectiveTime().Unix())    // exactly target

	// Switch to advancing mode and verify.
	if err := h.SetTime(target); err != nil {
		panic(err)
	}
	fmt.Println("frozen after SetTime:", h.IsFrozen()) // false
}

// Session_Prune shows how to use Prune when processes are expected to exit
// during a test — for example, Postgres backends that close when a connection
// ends. Without Prune (or without the automatic ESRCH handling), a dead handle
// would cause SetTime to return an error.
func ExampleSession_Prune() {
	target := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	s := faketime.NewSession(target)

	// Start several short-lived workers.
	for i := 0; i < 3; i++ {
		cmd := exec.Command("./worker", fmt.Sprintf("--id=%d", i))
		if err := s.Start(cmd); err != nil {
			panic(err)
		}
	}

	// ... run some work, workers may exit naturally ...

	// Explicitly remove dead handles before advancing the clock.
	removed := s.Prune()
	fmt.Println("pruned:", removed)

	// SetTime also silently drops any handles that return ESRCH,
	// so Prune is optional — use it when you want to know the count.
	if err := s.Advance(24 * time.Hour); err != nil {
		panic(err)
	}
	fmt.Println("remaining:", s.Len())
	fmt.Println("live PIDs:", s.PIDs())
}

// NewSession_withTracking shows how to create a session that automatically
// injects fake time into any process spawned by fork, vfork, or exec.
// Session.Close must be called to stop the watch goroutine; the idiomatic
// place is a deferred call or t.Cleanup (the latter is handled automatically
// by WithSession when WithTracking is passed as an option).
func ExampleNewSession_withTracking() {
	target := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	s := faketime.NewSession(target, faketime.WithTracking())
	defer s.Reset()   //nolint:errcheck
	defer s.Close()   //nolint:errcheck

	// The process and every child it forks or execs see the same fake clock.
	cmd := exec.Command("./postmaster")
	if err := s.Start(cmd); err != nil {
		panic(err)
	}
	defer cmd.Process.Kill()
	defer cmd.Wait() //nolint:errcheck

	// Advance the clock for all tracked processes at once.
	if err := s.Advance(24 * time.Hour); err != nil {
		panic(err)
	}
}

// WithChildTracker is the idiomatic way to test a process that spawns children
// when you want automatic tracker cleanup.
// In a real test t comes from the testing framework; the nil here is for
// illustration only — this example is not executed by go test.
func ExampleWithChildTracker() {
	var t *testing.T // replaced by the real *testing.T in your test function
	target := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	cmd := exec.Command("./postmaster")

	faketime.WithChildTracker(t, cmd, target, func(t *testing.T, ct *faketime.ChildTracker) {
		// Advance the entire process tree — parent + any forked children.
		if err := ct.Advance(30 * 24 * time.Hour); err != nil {
			t.Fatal(err)
		}
		// ct.Children() returns Handles for each forked backend.
		for _, child := range ct.Children() {
			_ = child // SetTime / Freeze / Advance / Reset also available per-child
		}
	})
	// Process is killed, tracker is closed, and clocks are reset here.
}

// WithSession is the idiomatic way to run a multi-process test with a shared
// fake clock.
// In a real test t comes from the testing framework; the nil here is for
// illustration only — this example is not executed by go test.
func ExampleWithSession() {
	var t *testing.T // replaced by the real *testing.T in your test function
	target := time.Date(2030, 6, 1, 0, 0, 0, 0, time.UTC)

	faketime.WithSession(t, target,
		func(s *faketime.Session) error {
			if err := s.Start(exec.Command("./service-a")); err != nil {
				return err
			}
			return s.Start(exec.Command("./service-b"))
		},
		func(t *testing.T, s *faketime.Session) {
			// Both processes share the same fake clock.
			if err := s.Freeze(target.Add(30 * 24 * time.Hour)); err != nil {
				t.Fatal(err)
			}
			// ... assert cross-service behaviour at the frozen instant ...
		},
	)
}
