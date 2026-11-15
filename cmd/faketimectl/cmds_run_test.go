//go:build linux

package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// captureStderr redirects the package-level stderr for the test's duration.
func captureStderr(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	old := stderr
	stderr = &buf
	t.Cleanup(func() { stderr = old })
	return &buf
}

func requireTool(t *testing.T, name string) {
	t.Helper()
	if _, err := exec.LookPath(name); err != nil {
		t.Skipf("%s not available: %v", name, err)
	}
}

// readEpoch parses the single integer a command wrote to path.
func readEpoch(t *testing.T, path string) int64 {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	n, err := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
	if err != nil {
		t.Fatalf("unexpected contents %q: %v", b, err)
	}
	return n
}

func TestRunPreloadAdvancing(t *testing.T) {
	requireTool(t, "sh")
	requireTool(t, "date")
	logs := captureStderr(t)

	out := filepath.Join(t.TempDir(), "out")
	target := time.Now().Add(48 * time.Hour).UTC().Truncate(time.Second)
	err := run([]string{"run", "--mode=preload", "--time=" + target.Format(time.RFC3339),
		"--", "sh", "-c", "date -u +%s > " + out})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if got := readEpoch(t, out); got < target.Unix() || got > target.Unix()+5 {
		t.Errorf("child saw %d, want %d..%d", got, target.Unix(), target.Unix()+5)
	}
	if !strings.Contains(logs.String(), "preload") {
		t.Errorf("status line should say which mode was used, got %q", logs.String())
	}
}

// The grandchild (sh -> date) sees the frozen instant, so the clock reached it
// through the inherited environment with no tracer involved.
func TestRunPreloadFrozenReachesChildren(t *testing.T) {
	requireTool(t, "sh")
	requireTool(t, "date")
	captureStderr(t)

	out := filepath.Join(t.TempDir(), "out")
	target := time.Date(2032, 2, 3, 4, 5, 6, 0, time.UTC)
	err := run([]string{"run", "--mode=preload", "--freeze", "--time=" + target.Format(time.RFC3339),
		"--", "sh", "-c", "date -u +%s > " + out})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if got := readEpoch(t, out); got != target.Unix() {
		t.Errorf("child saw %d, want exactly %d", got, target.Unix())
	}
}

// Preload mode must refuse a binary it cannot affect instead of running it on
// real time, and the error has to point at the mode that does work.
func TestRunPreloadRefusesGoBinary(t *testing.T) {
	captureStderr(t)
	self, err := os.Executable() // the test binary is itself a Go binary
	if err != nil {
		t.Fatal(err)
	}
	err = run([]string{"run", "--mode=preload", "--time=2030-01-01T00:00:00Z", "--", self})
	if err == nil {
		t.Fatal("expected an error for a Go binary")
	}
	for _, want := range []string{"Go binary", "--mode=vdso"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q should mention %q", err, want)
		}
	}
}

func TestRunRejectsUnknownMode(t *testing.T) {
	captureStderr(t)
	err := run([]string{"run", "--mode=bogus", "--time=2030-01-01T00:00:00Z", "--", "true"})
	if err == nil || !strings.Contains(err.Error(), `"bogus"`) {
		t.Errorf("got %v, want an error naming the bad mode", err)
	}
}

func TestExitCodeFor(t *testing.T) {
	requireTool(t, "sh")
	cases := []struct {
		script string
		want   int
	}{
		{"exit 7", 7},
		{"kill -9 $$", 128 + 9},
	}
	for _, c := range cases {
		err := exec.Command("sh", "-c", c.script).Run()
		code, ok := exitCodeFor(err)
		if !ok || code != c.want {
			t.Errorf("%q: exitCodeFor = (%d, %v), want (%d, true)", c.script, code, ok, c.want)
		}
	}
	if _, ok := exitCodeFor(os.ErrNotExist); ok {
		t.Error("a non-exit error should report ok=false")
	}
}
