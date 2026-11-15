//go:build e2e

package e2e_test

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/bkaznowski/epochd/pkg/sdk"
)

// These tests cover what happens to a container's clock when a timeshift ends or
// the agent underneath it fails. TestTimeshiftDate and TestTimeshiftUpdate show
// the clock can be set; a fake clock that cannot be taken off again is the worse
// failure, so each test here ends by asserting the container is back on real time.

func e2eClient(t *testing.T) *sdk.Client {
	t.Helper()
	url := os.Getenv("EPOCHD_URL")
	if url == "" {
		t.Skip("EPOCHD_URL not set — run via 'make e2e'")
	}
	return sdk.NewClient(url).WithToken(os.Getenv("EPOCHD_TOKEN"))
}

// startClockPod runs a clockprinter pod labelled app=<name> in a namespace of its
// own and waits for it to be ready.
func startClockPod(t *testing.T, name string) (ns string) {
	t.Helper()
	ns = "epochd-e2e-" + name
	exec.Command("kubectl", "create", "namespace", ns).Run() //nolint:errcheck
	t.Cleanup(func() {
		exec.Command("kubectl", "delete", "namespace", ns, "--ignore-not-found=true", "--wait=false").Run() //nolint:errcheck
	})
	runCmd(t, "kubectl", "run", name, "-n", ns,
		"--image="+clockprinterImage(), "--image-pull-policy=Never",
		"--restart=Never", "--labels=app="+name)
	runCmd(t, "kubectl", "wait", "-n", ns, "pod/"+name, "--for=condition=Ready", "--timeout=60s")
	return ns
}

// waitYear polls the pod's log until its latest timestamp has the wanted year, or
// fails the test after timeout. Polling rather than sleeping: the clockprinter
// prints once a second and the cluster is not fast.
func waitYear(t *testing.T, ns, pod string, want int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last string
	for {
		out, err := exec.Command("kubectl", "logs", "-n", ns, pod, "--tail=3").CombinedOutput()
		if err == nil {
			last = lastYear(strings.TrimSpace(string(out)))
			if last == fmt.Sprint(want) {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("container clock year = %q after %v, want %d", last, timeout, want)
		}
		time.Sleep(time.Second)
	}
}

// realYear is the year the container should see once its clock is real again.
func realYear() int { return time.Now().UTC().Year() }

// waitAgentReady waits for the (re)created agent pod to be Ready.
func waitAgentReady(t *testing.T) {
	t.Helper()
	// The old pod may still be listed while terminating; wait for the rollout.
	runCmd(t, "kubectl", "-n", "epochd", "rollout", "status", "daemonset/epochd-agent", "--timeout=90s")
	runCmd(t, "kubectl", "-n", "epochd", "wait", "pod", "-l", "app=epochd-agent",
		"--for=condition=Ready", "--timeout=90s")
}

// agentRestarts returns how many times the agent container has been restarted.
func agentRestarts(t *testing.T) int {
	t.Helper()
	out := strings.TrimSpace(outputCmd(t, "kubectl", "-n", "epochd", "get", "pod", "-l", "app=epochd-agent",
		"-o", "jsonpath={.items[0].status.containerStatuses[0].restartCount}"))
	var n int
	fmt.Sscan(out, &n) //nolint:errcheck
	return n
}

// killAgentHard SIGKILLs the agent process on its node, with no chance to drain,
// and waits for kubelet to bring the container back. Deleting the pod, even with
// --grace-period=0, does not do this: kubelet still sends SIGTERM first, and the
// agent shuts down cleanly and resets every container on its way out. kind nodes
// are Docker containers named after the node, and the agent shares the node's PID
// namespace (hostPID), so it can be killed from outside.
func killAgentHard(t *testing.T) {
	t.Helper()
	node := strings.TrimSpace(outputCmd(t, "kubectl", "get", "nodes", "-o", "jsonpath={.items[0].metadata.name}"))
	before := agentRestarts(t)
	runCmd(t, "docker", "exec", node, "pkill", "-KILL", "-f", "^/agent --listen")

	deadline := time.Now().Add(60 * time.Second)
	for agentRestarts(t) <= before {
		if time.Now().After(deadline) {
			t.Fatal("agent container was not restarted after SIGKILL")
		}
		time.Sleep(time.Second)
	}
	runCmd(t, "kubectl", "-n", "epochd", "wait", "pod", "-l", "app=epochd-agent",
		"--for=condition=Ready", "--timeout=90s")
}

// Delete must put the container back on the real clock.
func TestDeleteRestoresRealClock(t *testing.T) {
	ctx := context.Background()
	client := e2eClient(t)
	ns := startClockPod(t, "deletetest")

	target := time.Now().UTC().AddDate(3, 0, 0)
	ts, err := client.CreateTimeshift(ctx, ns, "app=deletetest", target, 10*time.Minute)
	if err != nil {
		t.Fatalf("CreateTimeshift: %v", err)
	}
	t.Cleanup(func() { client.DeleteTimeshift(context.Background(), ts.ID) }) //nolint:errcheck
	if err := client.WaitForActive(ctx, ts.ID); err != nil {
		t.Fatalf("WaitForActive: %v", err)
	}
	waitYear(t, ns, "deletetest", target.Year(), 30*time.Second)

	if err := client.DeleteTimeshift(ctx, ts.ID); err != nil {
		t.Fatalf("DeleteTimeshift: %v", err)
	}
	waitYear(t, ns, "deletetest", realYear(), 30*time.Second)
	if _, err := client.GetTimeshift(ctx, ts.ID); !sdk.IsNotFound(err) {
		t.Errorf("GetTimeshift after delete = %v, want not found", err)
	}
}

// A TTL is the safety net for a test that never cleans up, so the sweeper has to
// really put the clock back, not just forget the timeshift. The controller
// sweeps every 30s, so this waits for the next sweep after the TTL passes.
func TestTTLExpiryRestoresRealClock(t *testing.T) {
	ctx := context.Background()
	client := e2eClient(t)
	ns := startClockPod(t, "ttltest")

	target := time.Now().UTC().AddDate(3, 0, 0)
	ts, err := client.CreateTimeshift(ctx, ns, "app=ttltest", target, 5*time.Second)
	if err != nil {
		t.Fatalf("CreateTimeshift: %v", err)
	}
	t.Cleanup(func() { client.DeleteTimeshift(context.Background(), ts.ID) }) //nolint:errcheck
	if err := client.WaitForActive(ctx, ts.ID); err != nil {
		t.Fatalf("WaitForActive: %v", err)
	}

	// Confirm the clock really was faked first: the log's tail still holds real
	// lines from before injection, so "the real year appears" proves nothing alone.
	waitYear(t, ns, "ttltest", target.Year(), 30*time.Second)
	waitYear(t, ns, "ttltest", realYear(), 90*time.Second)
	if _, err := client.GetTimeshift(ctx, ts.ID); !sdk.IsNotFound(err) {
		t.Errorf("GetTimeshift after expiry = %v, want not found", err)
	}
}

// When the agent is killed instead of shut down, the hooks stay in the container
// and the new agent knows nothing about them. Delete must still end the fake
// clock: the controller takes the container over and resets it. Before that was
// handled, such a container stayed on the fake clock for good.
func TestAgentCrashThenDeleteRestoresRealClock(t *testing.T) {
	ctx := context.Background()
	client := e2eClient(t)
	ns := startClockPod(t, "crashtest")

	target := time.Now().UTC().AddDate(3, 0, 0)
	ts, err := client.CreateTimeshift(ctx, ns, "app=crashtest", target, 10*time.Minute)
	if err != nil {
		t.Fatalf("CreateTimeshift: %v", err)
	}
	t.Cleanup(func() { client.DeleteTimeshift(context.Background(), ts.ID) }) //nolint:errcheck
	if err := client.WaitForActive(ctx, ts.ID); err != nil {
		t.Fatalf("WaitForActive: %v", err)
	}
	waitYear(t, ns, "crashtest", target.Year(), 30*time.Second)

	// SIGKILL, no drain: nothing resets the container on the way out.
	killAgentHard(t)

	// The container is still on the fake clock, which is the whole problem.
	waitYear(t, ns, "crashtest", target.Year(), 15*time.Second)

	if err := client.DeleteTimeshift(ctx, ts.ID); err != nil {
		t.Fatalf("DeleteTimeshift after agent crash: %v", err)
	}
	waitYear(t, ns, "crashtest", realYear(), 30*time.Second)
}

// After a graceful agent restart the agent has drained its handles, so an
// update finds nothing to write to; the controller must inject again rather than
// report success for a clock that did not move.
func TestAgentRestartThenUpdateReinjects(t *testing.T) {
	ctx := context.Background()
	client := e2eClient(t)
	ns := startClockPod(t, "restarttest")

	first := time.Now().UTC().AddDate(1, 0, 0)
	second := time.Now().UTC().AddDate(5, 0, 0)
	ts, err := client.CreateTimeshift(ctx, ns, "app=restarttest", first, 10*time.Minute)
	if err != nil {
		t.Fatalf("CreateTimeshift: %v", err)
	}
	t.Cleanup(func() { client.DeleteTimeshift(context.Background(), ts.ID) }) //nolint:errcheck
	if err := client.WaitForActive(ctx, ts.ID); err != nil {
		t.Fatalf("WaitForActive: %v", err)
	}
	waitYear(t, ns, "restarttest", first.Year(), 30*time.Second)

	runCmd(t, "kubectl", "-n", "epochd", "delete", "pod", "-l", "app=epochd-agent")
	waitAgentReady(t)

	if _, err := client.UpdateTimeshift(ctx, ts.ID, second); err != nil {
		t.Fatalf("UpdateTimeshift after agent restart: %v", err)
	}
	waitYear(t, ns, "restarttest", second.Year(), 30*time.Second)
}
