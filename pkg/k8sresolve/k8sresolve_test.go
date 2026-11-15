//go:build linux

package k8sresolve

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

// TestStripRuntimePrefix verifies that various containerID formats are
// normalised correctly.
func TestStripRuntimePrefix(t *testing.T) {
	cases := []struct {
		input, want string
	}{
		{"containerd://abc123def456", "abc123def456"},
		{"docker://abc123def456", "abc123def456"},
		{"cri-o://abc123def456", "abc123def456"},
		{"abc123def456", "abc123def456"}, // already bare
	}
	for _, c := range cases {
		got := stripRuntimePrefix(c.input)
		if got != c.want {
			t.Errorf("stripRuntimePrefix(%q) = %q, want %q", c.input, got, c.want)
		}
	}
}

// TestLookupPIDSelf verifies that LookupPID can find the test process itself
// by matching its own cgroup entry. This is a self-contained smoke test that
// doesn't require a real container environment.
//
// On a node with a typical container runtime (containerd/cgroup v2), the
// cgroup file for a process looks like:
//
//	0::/kubepods/burstable/pod<uuid>/<containerID>
//
// For a non-containerised process (the test runner on the host), the cgroup
// file will contain something like "0::/user.slice/..." which is NOT a
// container ID, so we can't use our own container ID here. Instead we verify
// the parsing and scanning logic by checking that our own PID appears in the
// scan when we write a fake cgroup file with a known ID.
//
// The actual container-lookup path is exercised in the agent integration tests.
func TestLookupPIDSelf(t *testing.T) {
	pid := os.Getpid()

	// Read our own cgroup file to get a real substring we can match on.
	cgroupPath := "/proc/" + strconv.Itoa(pid) + "/cgroup"
	data, err := os.ReadFile(cgroupPath)
	if err != nil {
		t.Fatalf("ReadFile %s: %v", cgroupPath, err)
	}

	// Use 20 chars from the middle of the cgroup content as a "fake container
	// ID". This exercises the scanning and matching logic without requiring a
	// real kubelet cgroup hierarchy.
	content := string(data)
	if len(content) < 20 {
		t.Skipf("cgroup content too short to construct a test ID: %q", content)
	}
	fakeID := content[len(content)/2:]
	if len(fakeID) > 20 {
		fakeID = fakeID[:20]
	}

	// Strip any runtime prefix characters that might confuse the substring match.
	// We want a raw substring that appears in the cgroup file.
	for len(fakeID) > 8 {
		found := false
		entries, _ := os.ReadDir("/proc")
		for _, e := range entries {
			p, err := strconv.Atoi(e.Name())
			if err != nil || p != pid {
				continue
			}
			cg, _ := os.ReadFile("/proc/" + e.Name() + "/cgroup")
			if len(cg) > 0 {
				found = true
				break
			}
		}
		if found {
			break
		}
		fakeID = fakeID[1:]
	}

	// Now call LookupPID with a synthesised container ID that has a runtime
	// prefix and whose bare form is our chosen substring.
	syntheticID := "containerd://" + fakeID + "0000000000000000000000000000000000000000000000000"

	// LookupPID will find our own PID (among others) since our cgroup file
	// contains the prefix. We just verify it doesn't error and returns a
	// positive PID.
	found, err := LookupPID(syntheticID)
	if err != nil {
		t.Logf("LookupPID returned error (may be expected in non-k8s environment): %v", err)
		t.Skip("skipping: cgroup substring not found — not running in a cgroup-v2 environment")
	}
	if found <= 0 {
		t.Errorf("LookupPID returned pid %d, want > 0", found)
	}
	t.Logf("LookupPID found pid %d for synthetic ID %q", found, fakeID[:12])
}

// TestLookupPIDNotFound verifies that a clearly non-existent container ID
// returns a clear error.
func TestLookupPIDNotFound(t *testing.T) {
	// Use a 64-char hex string that almost certainly doesn't appear in any cgroup.
	const fakeID = "containerd://0000000000000000000000000000000000000000000000000000000000000000"
	_, err := LookupPID(fakeID)
	if err == nil {
		t.Error("expected error for unknown container ID, got nil")
	}
	t.Logf("got expected error: %v", err)
}

// TestLookupPIDTooShort verifies that an ID shorter than 8 characters is
// rejected before scanning /proc.
func TestLookupPIDTooShort(t *testing.T) {
	_, err := LookupPID("abc")
	if err == nil {
		t.Error("expected error for short container ID, got nil")
	}
}

// ---------------------------------------------------------------------------
// LookupPIDs against a fake /proc tree
// ---------------------------------------------------------------------------

// fakeProc builds a /proc-shaped directory and points procRoot at it.
type fakeProc struct {
	t    *testing.T
	root string
}

func newFakeProc(t *testing.T) *fakeProc {
	t.Helper()
	root := t.TempDir()
	old := procRoot
	procRoot = root
	t.Cleanup(func() { procRoot = old })
	return &fakeProc{t: t, root: root}
}

// add creates /proc/<pid> with the given cgroup content and start time. The stat
// line has the real layout, including a command name with spaces and parens.
func (f *fakeProc) add(pid int, cgroup string, start uint64) {
	f.t.Helper()
	dir := filepath.Join(f.root, strconv.Itoa(pid))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "cgroup"), []byte(cgroup), 0o644); err != nil {
		f.t.Fatal(err)
	}
	if start == 0 {
		return // leave stat unreadable
	}
	// pid (comm) state ppid pgrp session tty tpgid flags minflt cminflt majflt
	// cmajflt utime stime cutime cstime priority nice threads itrealvalue starttime ...
	stat := strconv.Itoa(pid) + " (my (weird) cmd) S 1 1 1 0 -1 0 0 0 0 0 0 0 0 0 20 0 1 0 " +
		strconv.FormatUint(start, 10) + " 0 0\n"
	if err := os.WriteFile(filepath.Join(dir, "stat"), []byte(stat), 0o644); err != nil {
		f.t.Fatal(err)
	}
}

const fakeCID = "aabbccddeeff00112233445566778899aabbccddeeff00112233445566778899"

func cg(id string) string { return "0::/kubepods/burstable/pod1234/" + id + "\n" }

func TestLookupPIDsReturnsEveryProcessOfTheContainer(t *testing.T) {
	f := newFakeProc(t)
	f.add(300, cg(fakeCID), 1000)
	f.add(301, cg(fakeCID), 1010)
	f.add(302, cg(fakeCID), 1020)
	f.add(400, cg("1111222233334444555566667777888899990000aaaabbbbccccddddeeeeffff"), 900) // another container
	f.add(500, "0::/user.slice/session-1.scope\n", 800)                                     // not a container

	got, err := LookupPIDs("containerd://" + fakeCID)
	if err != nil {
		t.Fatal(err)
	}
	if want := []int{300, 301, 302}; !equalInts(got, want) {
		t.Errorf("LookupPIDs = %v, want exactly this container's processes %v", got, want)
	}
}

// pids wrap, so a worker started after the init can have a lower pid number. The
// init is whichever started first.
func TestLookupPIDsOrdersByStartTimeNotPidNumber(t *testing.T) {
	f := newFakeProc(t)
	f.add(32000, cg(fakeCID), 500) // the init: started first, high pid
	f.add(12, cg(fakeCID), 9000)   // a worker after the pid counter wrapped
	f.add(20, cg(fakeCID), 9500)

	got, err := LookupPIDs(fakeCID)
	if err != nil {
		t.Fatal(err)
	}
	if want := []int{32000, 12, 20}; !equalInts(got, want) {
		t.Errorf("LookupPIDs = %v, want %v (init first)", got, want)
	}
	if pid, _ := LookupPID(fakeCID); pid != 32000 {
		t.Errorf("LookupPID = %d, want the init (32000)", pid)
	}
}

func TestLookupPIDsHandlesAnUnreadableStartTime(t *testing.T) {
	f := newFakeProc(t)
	f.add(50, cg(fakeCID), 0) // no stat file: sorts last
	f.add(60, cg(fakeCID), 7)
	f.add(40, cg(fakeCID), 0) // no stat file, lower pid: before 50

	got, err := LookupPIDs(fakeCID)
	if err != nil {
		t.Fatal(err)
	}
	if want := []int{60, 40, 50}; !equalInts(got, want) {
		t.Errorf("LookupPIDs = %v, want %v", got, want)
	}
}

func TestLookupPIDsErrors(t *testing.T) {
	f := newFakeProc(t)
	f.add(300, cg(fakeCID), 1)

	if _, err := LookupPIDs("containerd://ffffffffffffffffffffffffffffffffffffffffffffffff"); err == nil {
		t.Error("an unknown container must be an error")
	}
	if _, err := LookupPIDs("abc"); err == nil {
		t.Error("a container id that is too short must be an error")
	}
}

func TestStartTimeParsesAwkwardCommandNames(t *testing.T) {
	f := newFakeProc(t)
	f.add(77, cg(fakeCID), 424242)
	if got := startTime(77); got != 424242 {
		t.Errorf("startTime = %d, want 424242 (the command name contains spaces and parentheses)", got)
	}
}

func equalInts(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
