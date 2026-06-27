//go:build linux

package faketime

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// TestFaketimeClockBenchHelper is the in-process clock benchmark target.
// When helperEnv == "clockbench" it warms up, then times 1 000 000 calls to
// time.Now() (which exercises the vDSO clock_gettime path we patch) using the
// monotonic clock so the measurement is not affected by the fake offset.
// It prints a single integer: nanoseconds per call.
func TestFaketimeClockBenchHelper(t *testing.T) {
	if os.Getenv(helperEnv) != "clockbench" {
		t.Skip()
	}
	const (
		warmup = 10_000
		iters  = 1_000_000
	)
	for i := 0; i < warmup; i++ {
		_ = time.Now().UnixNano()
	}
	// time.Since uses CLOCK_MONOTONIC, which we do not intercept, so elapsed
	// reflects real wall time even when CLOCK_REALTIME is faked.
	start := time.Now()
	for i := 0; i < iters; i++ {
		_ = time.Now().UnixNano()
	}
	elapsed := time.Since(start)
	fmt.Printf("%d\n", elapsed.Nanoseconds()/iters)
}

// measureClockInChild runs TestFaketimeClockBenchHelper in a subprocess and
// returns the ns/call it reports. If withFakeTime is true the process is
// started via Start() so the trampoline is active.
func measureClockInChild(b *testing.B, withFakeTime bool) int64 {
	b.Helper()
	exe, err := os.Executable()
	if err != nil {
		b.Fatal(err)
	}
	cmd := exec.Command(exe, "-test.run=^TestFaketimeClockBenchHelper$")
	cmd.Env = append(os.Environ(), helperEnv+"=clockbench")

	var buf bytes.Buffer
	cmd.Stdout = &buf

	if withFakeTime {
		target := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
		if _, err := Start(cmd, target); err != nil {
			b.Fatal(err)
		}
	} else {
		if err := cmd.Start(); err != nil {
			b.Fatal(err)
		}
	}
	cmd.Wait() //nolint:errcheck // PASS/FAIL exit code; we check the output

	// Find the first line that parses as a positive integer (ignoring "PASS" etc.).
	var nsPer int64
	for _, line := range strings.Split(buf.String(), "\n") {
		if n, err := fmt.Sscanf(strings.TrimSpace(line), "%d", &nsPer); n == 1 && err == nil && nsPer > 0 {
			return nsPer
		}
	}
	b.Fatalf("clock bench: no result in output: %q", buf.String())
	return 0
}

// BenchmarkClockGettime compares the per-call latency of clock_gettime
// (exercised via time.Now) with and without the faketime trampoline active.
// The measurement comes from inside the subprocess so it includes the full
// vDSO dispatch path. Run with -benchtime=1x since each sub-benchmark already
// averages 1 000 000 iterations internally.
//
//	go test -bench=BenchmarkClockGettime -benchtime=5x ./pkg/faketime/
func BenchmarkClockGettime(b *testing.B) {
	for _, tc := range []struct {
		name string
		fake bool
	}{
		{"baseline", false},
		{"faked", true},
	} {
		b.Run(tc.name, func(b *testing.B) {
			b.StopTimer() // timing is measured inside the subprocess
			var total int64
			for i := 0; i < b.N; i++ {
				total += measureClockInChild(b, tc.fake)
			}
			b.ReportMetric(float64(total)/float64(b.N), "ns/clock_gettime")
		})
	}
}
