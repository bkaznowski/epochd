//go:build linux && amd64

package trampoline

import (
	"encoding/binary"
	"syscall"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"

	"github.com/bkaznowski/epochd/pkg/trampoline/internal/callasm"
)

// These tests run the compiled hooks directly: the blob is copied into an
// executable anonymous mapping and each entry point is called by address (see
// internal/callasm). That checks the hooks' own logic -- carry and borrow,
// error propagation, argument passthrough -- deterministically, with no ptrace
// and no target process. pkg/inject's tests separately prove they work once
// injected into a real vDSO.

const (
	pageSize  = 4096
	bufOffset = 2048 // scratch area for timespec/timeval/tz buffers, past the blob

	clockRealtime       = 0
	clockMonotonic      = 1
	clockRealtimeCoarse = 5
	badClock            = 12345 // not a valid clock id: the kernel answers EINVAL
)

// hooks is one executable copy of the blob with its own state struct.
type hooks struct {
	t   *testing.T
	mem []byte
}

func newHooks(t *testing.T) *hooks {
	t.Helper()
	if len(Payload) > bufOffset {
		t.Fatalf("blob is %d bytes; grow bufOffset", len(Payload))
	}
	mem, err := syscall.Mmap(-1, 0, pageSize,
		syscall.PROT_READ|syscall.PROT_WRITE|syscall.PROT_EXEC,
		syscall.MAP_PRIVATE|syscall.MAP_ANON)
	if err != nil {
		t.Skipf("cannot map an executable page here: %v", err)
	}
	t.Cleanup(func() { syscall.Munmap(mem) }) //nolint:errcheck
	copy(mem, Payload)
	return &hooks{t: t, mem: mem}
}

// setState writes the state struct the way the Go layer does.
func (h *hooks) setState(sec, nsec int64, mask uint64) {
	copy(h.mem[StateOffset:], EncodeState(sec, nsec, mask, 0))
}

func (h *hooks) addr(off int) uintptr { return uintptr(unsafe.Pointer(&h.mem[off])) }

func (h *hooks) clockGettime(clk int, tpOff int) int32 {
	return int32(callasm.Call(h.addr(ClockGettimeEntryOffset), uintptr(clk), h.addr(tpOff)))
}

func (h *hooks) gettimeofday(tvOff, tzOff int) int32 {
	tz := uintptr(0)
	if tzOff != 0 {
		tz = h.addr(tzOff)
	}
	tv := uintptr(0)
	if tvOff != 0 {
		tv = h.addr(tvOff)
	}
	return int32(callasm.Call(h.addr(GettimeofdayEntryOffset), tv, tz))
}

func (h *hooks) time(tlocOff int) int64 {
	tloc := uintptr(0)
	if tlocOff != 0 {
		tloc = h.addr(tlocOff)
	}
	return int64(callasm.Call(h.addr(TimeEntryOffset), tloc, 0))
}

func (h *hooks) pair(off int) (int64, int64) {
	return int64(binary.LittleEndian.Uint64(h.mem[off:])), int64(binary.LittleEndian.Uint64(h.mem[off+8:]))
}

func (h *hooks) setPair(off int, a, b int64) {
	binary.LittleEndian.PutUint64(h.mem[off:], uint64(a))
	binary.LittleEndian.PutUint64(h.mem[off+8:], uint64(b))
}

func realNow(t *testing.T) time.Time {
	t.Helper()
	return time.Now()
}

func near(got, want time.Time, tol time.Duration) bool {
	d := got.Sub(want)
	return d >= -tol && d <= tol
}

func TestHooksFrozenReturnExactTime(t *testing.T) {
	h := newHooks(t)
	const sec, nsec = 1_900_000_000, 123_456_789
	h.setState(sec, nsec, MaskFrozen)

	if rc := h.clockGettime(clockRealtime, bufOffset); rc != 0 {
		t.Fatalf("clock_gettime returned %d, want 0", rc)
	}
	if gs, gn := h.pair(bufOffset); gs != sec || gn != nsec {
		t.Errorf("clock_gettime = (%d,%d), want (%d,%d)", gs, gn, sec, nsec)
	}

	// REALTIME_COARSE is the same wall clock and must be frozen too.
	h.setPair(bufOffset, 0, 0)
	h.clockGettime(clockRealtimeCoarse, bufOffset)
	if gs, gn := h.pair(bufOffset); gs != sec || gn != nsec {
		t.Errorf("clock_gettime(COARSE) = (%d,%d), want (%d,%d)", gs, gn, sec, nsec)
	}

	// gettimeofday reports microseconds, truncated from the nanoseconds.
	if rc := h.gettimeofday(bufOffset, 0); rc != 0 {
		t.Fatalf("gettimeofday returned %d, want 0", rc)
	}
	if gs, gu := h.pair(bufOffset); gs != sec || gu != nsec/1000 {
		t.Errorf("gettimeofday = (%d,%d), want (%d,%d)", gs, gu, int64(sec), int64(nsec/1000))
	}

	// time() returns whole seconds and stores them through tloc.
	if got := h.time(bufOffset); got != sec {
		t.Errorf("time() = %d, want %d", got, sec)
	}
	if got, _ := h.pair(bufOffset); got != sec {
		t.Errorf("*tloc = %d, want %d", got, sec)
	}
	if got := h.time(0); got != sec {
		t.Errorf("time(NULL) = %d, want %d", got, sec)
	}
}

func TestHooksAdvancingAddsOffset(t *testing.T) {
	h := newHooks(t)
	const offset = 36 * time.Hour
	h.setState(int64(offset/time.Second), 0, MaskEnabled)

	before := realNow(t)
	if rc := h.clockGettime(clockRealtime, bufOffset); rc != 0 {
		t.Fatalf("clock_gettime returned %d", rc)
	}
	sec, nsec := h.pair(bufOffset)
	if got := time.Unix(sec, nsec); !near(got, before.Add(offset), time.Second) {
		t.Errorf("clock_gettime = %v, want ~%v", got, before.Add(offset))
	}

	h.gettimeofday(bufOffset, 0)
	sec, usec := h.pair(bufOffset)
	if got := time.Unix(sec, usec*1000); !near(got, before.Add(offset), time.Second) {
		t.Errorf("gettimeofday = %v, want ~%v", got, before.Add(offset))
	}

	if got := time.Unix(h.time(bufOffset), 0); !near(got, before.Add(offset), 2*time.Second) {
		t.Errorf("time() = %v, want ~%v", got, before.Add(offset))
	}
}

// Offsets near +-1s force the carry and borrow paths: whatever the real
// fraction is, the sum must come back normalised and equal to real+offset.
func TestHooksNormalizeCarryAndBorrow(t *testing.T) {
	for _, offNsec := range []int64{999_999_999, -999_999_999, 500_000_000, -500_000_000} {
		h := newHooks(t)
		h.setState(100, offNsec, MaskEnabled)
		want := time.Duration(100)*time.Second + time.Duration(offNsec)

		for i := 0; i < 200; i++ {
			before := realNow(t)
			h.clockGettime(clockRealtime, bufOffset)
			sec, nsec := h.pair(bufOffset)
			if nsec < 0 || nsec >= 1_000_000_000 {
				t.Fatalf("offsetNsec %d: tv_nsec %d is not normalised", offNsec, nsec)
			}
			if got := time.Unix(sec, nsec); !near(got, before.Add(want), 50*time.Millisecond) {
				t.Fatalf("offsetNsec %d: got %v, want ~%v", offNsec, got, before.Add(want))
			}

			h.gettimeofday(bufOffset, 0)
			_, usec := h.pair(bufOffset)
			if usec < 0 || usec >= 1_000_000 {
				t.Fatalf("offsetNsec %d: tv_usec %d is not normalised", offNsec, usec)
			}
		}
	}
}

// time() must agree with clock_gettime about the current second even when the
// offset's fraction would carry across a second boundary.
func TestHooksTimeAgreesWithClockGettime(t *testing.T) {
	h := newHooks(t)
	h.setState(1000, 999_999_999, MaskEnabled)
	for i := 0; i < 500; i++ {
		h.clockGettime(clockRealtime, bufOffset)
		cgt, _ := h.pair(bufOffset)
		tm := h.time(0)
		if d := tm - cgt; d < 0 || d > 1 { // time() runs just after, so it may be one second later
			t.Fatalf("time()=%d vs clock_gettime=%d differ by %d", tm, cgt, d)
		}
	}
}

func TestHooksDisabledPassesThrough(t *testing.T) {
	h := newHooks(t)
	h.setState(1_000_000, 0, 0) // not enabled: must be the real clock

	before := realNow(t)
	h.clockGettime(clockRealtime, bufOffset)
	sec, nsec := h.pair(bufOffset)
	if got := time.Unix(sec, nsec); !near(got, before, time.Second) {
		t.Errorf("disabled clock_gettime = %v, want real time ~%v", got, before)
	}
	if got := time.Unix(h.time(0), 0); !near(got, before, 2*time.Second) {
		t.Errorf("disabled time() = %v, want real time ~%v", got, before)
	}
}

// Only the wall clock is faked: monotonic reads must reach the kernel untouched
// even with an enormous offset installed.
func TestHooksMonotonicIsNeverFaked(t *testing.T) {
	h := newHooks(t)
	h.setState(1_000_000_000, 0, MaskEnabled)

	var real unix.Timespec
	if err := unix.ClockGettime(unix.CLOCK_MONOTONIC, &real); err != nil {
		t.Fatal(err)
	}
	if rc := h.clockGettime(clockMonotonic, bufOffset); rc != 0 {
		t.Fatalf("clock_gettime(MONOTONIC) returned %d", rc)
	}
	sec, nsec := h.pair(bufOffset)
	got := time.Duration(sec)*time.Second + time.Duration(nsec)
	want := time.Duration(real.Sec)*time.Second + time.Duration(real.Nsec)
	if d := got - want; d < 0 || d > time.Second {
		t.Errorf("monotonic reading %v is not within a second after the real one %v", got, want)
	}
}

// A hooked call must report what the real vDSO function would: the kernel's
// error for a clock it rejects, and EFAULT for a bad pointer -- in every mode,
// and without applying an offset to memory that was never written.
func TestHooksPropagateErrors(t *testing.T) {
	for _, mode := range []struct {
		name string
		mask uint64
	}{{"disabled", 0}, {"advancing", MaskEnabled}} {
		t.Run(mode.name, func(t *testing.T) {
			h := newHooks(t)
			h.setState(500, 250_000_000, mode.mask)

			if rc := h.clockGettime(badClock, bufOffset); rc != -int32(unix.EINVAL) {
				t.Errorf("clock_gettime(bad clock) = %d, want %d (-EINVAL)", rc, -int32(unix.EINVAL))
			}
			// An unmapped address: the syscall answers EFAULT. The hook must hand
			// that back instead of faulting or reporting success.
			rc := int32(callasm.Call(h.addr(ClockGettimeEntryOffset), clockRealtime, 8))
			if rc != -int32(unix.EFAULT) {
				t.Errorf("clock_gettime(bad pointer) = %d, want %d (-EFAULT)", rc, -int32(unix.EFAULT))
			}
			rc = int32(callasm.Call(h.addr(GettimeofdayEntryOffset), 8, 0))
			if rc != -int32(unix.EFAULT) {
				t.Errorf("gettimeofday(bad pointer) = %d, want %d (-EFAULT)", rc, -int32(unix.EFAULT))
			}
		})
	}
}

// gettimeofday's second argument (struct timezone) belongs to the caller; the
// passthrough must hand it to the kernel rather than dropping it.
func TestHooksGettimeofdayPassesTimezoneThrough(t *testing.T) {
	for _, mode := range []struct {
		name string
		mask uint64
	}{{"disabled", 0}, {"advancing", MaskEnabled}} {
		t.Run(mode.name, func(t *testing.T) {
			h := newHooks(t)
			h.setState(0, 0, mode.mask)

			const tzOff = bufOffset + 64
			for i := 0; i < 8; i++ { // poison it so an untouched buffer is detectable
				h.mem[tzOff+i] = 0xff
			}
			if rc := h.gettimeofday(bufOffset, tzOff); rc != 0 {
				t.Fatalf("gettimeofday returned %d", rc)
			}
			west := int32(binary.LittleEndian.Uint32(h.mem[tzOff:]))
			dst := int32(binary.LittleEndian.Uint32(h.mem[tzOff+4:]))
			if west == -1 && dst == -1 {
				t.Error("struct timezone was never written: tz was not passed to the syscall")
			}
		})
	}
}
