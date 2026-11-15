//go:build linux

package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/bkaznowski/epochd/pkg/preload"
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

// ---------------------------------------------------------------------------
// Signal forwarding
// ---------------------------------------------------------------------------

// captureExit makes exitFunc record the status instead of ending the test binary.
func captureExit(t *testing.T) *int {
	t.Helper()
	code := -1
	old := exitFunc
	exitFunc = func(c int) { code = c }
	t.Cleanup(func() { exitFunc = old })
	return &code
}

// `docker stop` sends SIGTERM to PID 1, which is faketimectl when it is a
// container's entry point. It must reach the command it started, not just stop
// faketimectl and orphan it.
func TestRunForwardsSIGTERMToTheCommand(t *testing.T) {
	requireTool(t, "sh")
	requireTool(t, "sleep")
	captureStderr(t)
	code := captureExit(t)

	ready := filepath.Join(t.TempDir(), "ready")
	script := "trap 'exit 42' TERM; : > " + ready + "; while :; do sleep 0.05; done"

	done := make(chan error, 1)
	go func() {
		done <- run([]string{"run", "--mode=preload", "--time=2030-01-01T00:00:00Z", "--", "sh", "-c", script})
	}()

	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := os.Stat(ready); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the command never started")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err := syscall.Kill(os.Getpid(), syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("run: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("run did not return: the SIGTERM never reached the command")
	}
	if *code != 42 {
		t.Errorf("exit status = %d, want 42 (the command's own handler ran)", *code)
	}
}

// The token for the controller subcommands comes from the environment, never a flag.
func TestTokenFromEnv(t *testing.T) {
	t.Setenv("EPOCHD_TOKEN", "")
	t.Setenv("EPOCHD_TOKEN_FILE", "")
	if tok, err := tokenFromEnv(); tok != "" || err != nil {
		t.Errorf("with nothing set: (%q, %v), want no token and no error", tok, err)
	}

	t.Setenv("EPOCHD_TOKEN", "  literal  ")
	if tok, _ := tokenFromEnv(); tok != "literal" {
		t.Errorf("EPOCHD_TOKEN = %q, want it trimmed", tok)
	}

	file := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(file, []byte("from-file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("EPOCHD_TOKEN", "")
	t.Setenv("EPOCHD_TOKEN_FILE", file)
	if tok, err := tokenFromEnv(); tok != "from-file" || err != nil {
		t.Errorf("EPOCHD_TOKEN_FILE = (%q, %v), want the file's contents", tok, err)
	}

	t.Setenv("EPOCHD_TOKEN", "wins")
	if tok, _ := tokenFromEnv(); tok != "wins" {
		t.Errorf("EPOCHD_TOKEN should take precedence over the file, got %q", tok)
	}

	t.Setenv("EPOCHD_TOKEN", "")
	t.Setenv("EPOCHD_TOKEN_FILE", filepath.Join(t.TempDir(), "absent"))
	if _, err := tokenFromEnv(); err == nil {
		t.Error("an unreadable EPOCHD_TOKEN_FILE must be an error, not silently no token")
	}
	empty := filepath.Join(t.TempDir(), "empty")
	os.WriteFile(empty, []byte("\n"), 0o600) //nolint:errcheck
	t.Setenv("EPOCHD_TOKEN_FILE", empty)
	if _, err := tokenFromEnv(); err == nil {
		t.Error("an empty EPOCHD_TOKEN_FILE must be an error")
	}
}

// A command that never loads the preload library runs on real time whatever
// the clock was set to; the user has to be told, since nothing else shows it.
func TestWarnIfNeverLoaded(t *testing.T) {
	h, err := preload.New(time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()

	var buf bytes.Buffer
	warnIfNeverLoaded(&buf, h, "mydaemon")
	for _, want := range []string{"warning", `"mydaemon"`, "real time", "--mode=vdso"} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("warning %q should mention %q", buf.String(), want)
		}
	}

	// Once something loaded the library there is nothing to warn about.
	requireTool(t, "date")
	cmd := exec.Command("date")
	h2, err := preload.Start(cmd, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	defer h2.Close()
	cmd.Wait() //nolint:errcheck
	buf.Reset()
	warnIfNeverLoaded(&buf, h2, "date")
	if buf.Len() != 0 {
		t.Errorf("unexpected warning for a command that loaded the library: %q", buf.String())
	}
}

// The ordinary path must stay quiet: a command that loaded the library is not
// warned about.
func TestRunPreloadDoesNotWarnWhenLoaded(t *testing.T) {
	requireTool(t, "sh")
	logs := captureStderr(t)
	if err := run([]string{"run", "--mode=preload", "--time=2030-01-01T00:00:00Z", "--", "sh", "-c", "true"}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(logs.String(), "warning") {
		t.Errorf("unexpected warning: %q", logs.String())
	}
}
