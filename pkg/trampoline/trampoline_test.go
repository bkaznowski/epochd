//go:build linux

package trampoline

import (
	"encoding/binary"
	"testing"
)

// TestStateOffsetRegression is the CI guard called out in the plan.
// If someone edits trampoline.s and the state struct shifts without updating
// StateOffset, this fails loudly rather than silently corrupting memory in the
// target process at runtime.
func TestStateOffsetRegression(t *testing.T) {
	want := len(Payload) - StateSize
	if StateOffset != want {
		t.Errorf("StateOffset = %d, but len(Payload)-StateSize = %d; "+
			"update the StateOffset constant after re-assembling trampoline.s",
			StateOffset, want)
	}
}

// TestPayloadDefaultState checks that the embedded binary has the expected
// at-rest field values: both offsets zero, enabledMask=1, generation=0.
func TestPayloadDefaultState(t *testing.T) {
	if len(Payload) < StateOffset+StateSize {
		t.Fatalf("Payload too short: len=%d, need at least %d", len(Payload), StateOffset+StateSize)
	}

	state := Payload[StateOffset:]

	sec := int64(binary.LittleEndian.Uint64(state[0:]))
	nsec := int64(binary.LittleEndian.Uint64(state[8:]))
	mask := binary.LittleEndian.Uint64(state[16:])
	gen := binary.LittleEndian.Uint32(state[24:])

	if sec != 0 {
		t.Errorf("offsetSec = %d, want 0", sec)
	}
	if nsec != 0 {
		t.Errorf("offsetNsec = %d, want 0", nsec)
	}
	if mask != 1 {
		t.Errorf("enabledMask = %d, want 1 (CLOCK_REALTIME on by default)", mask)
	}
	if gen != 0 {
		t.Errorf("generation = %d, want 0", gen)
	}
}

// TestEncodeDecodeRoundTrip verifies that EncodeState and DecodeState are
// inverses of each other across a non-trivial set of field values.
func TestEncodeDecodeRoundTrip(t *testing.T) {
	cases := []struct {
		sec  int64
		nsec int64
		mask uint64
		gen  uint32
	}{
		{0, 0, 1, 0},
		{3600, 500_000_000, 1, 7},
		{-3600, -1, 0, 255},
		{1<<62 - 1, 999_999_999, ^uint64(0), ^uint32(0)},
	}
	for _, c := range cases {
		encoded := EncodeState(c.sec, c.nsec, c.mask, c.gen)
		if len(encoded) != StateSize {
			t.Errorf("EncodeState returned %d bytes, want %d", len(encoded), StateSize)
		}
		gotSec, gotNsec, gotMask, gotGen, err := DecodeState(encoded)
		if err != nil {
			t.Fatalf("DecodeState: %v", err)
		}
		if gotSec != c.sec || gotNsec != c.nsec || gotMask != c.mask || gotGen != c.gen {
			t.Errorf("round-trip(%d,%d,%d,%d): got (%d,%d,%d,%d)",
				c.sec, c.nsec, c.mask, c.gen,
				gotSec, gotNsec, gotMask, gotGen)
		}
	}
}

// TestDecodeStateTooShort checks that DecodeState returns a clean error on
// undersized input rather than panicking.
func TestDecodeStateTooShort(t *testing.T) {
	_, _, _, _, err := DecodeState(make([]byte, 16))
	if err == nil {
		t.Error("expected error for buffer shorter than StateSize, got nil")
	}
}
