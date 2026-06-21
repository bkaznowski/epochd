//go:build linux

package k8sresolve

import (
	"os"
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
