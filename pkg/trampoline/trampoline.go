//go:build linux

// Package trampoline holds the hand-assembled vDSO hook payload and the helpers
// needed to encode and decode the state struct that sits immediately after it.
package trampoline

import (
	_ "embed"
	"encoding/binary"
	"fmt"
)

//go:embed trampoline.bin
var Payload []byte

// StateOffset is the byte offset of the state struct within Payload.
// It equals the size of the trampoline code and must match the position of the
// `state:` label in trampoline.asm.  The regression test below will catch any
// future edit to the assembly that shifts the struct without updating this
// constant.
const StateOffset = 136

// State field offsets within Payload (absolute, not relative to StateOffset).
// Use these when computing the address to pass to procmem.WriteMem.
const (
	FieldOffsetSec   = StateOffset + 0  // int64
	FieldOffsetNsec  = StateOffset + 8  // int64
	FieldEnabledMask = StateOffset + 16 // uint64, bit 0 = CLOCK_REALTIME, bit 1 = freeze
	FieldGeneration  = StateOffset + 24 // uint32
)

// Mask values for the enabledMask field.
const (
	// MaskEnabled activates CLOCK_REALTIME interception in advancing mode.
	MaskEnabled = uint64(1)
	// MaskFrozen activates both interception and freeze mode: time.Now() always
	// returns the stored absolute timestamp regardless of real time passing.
	MaskFrozen = uint64(3)
)

// StateSize is the byte length of the mutable state struct.
const StateSize = 32 // 8+8+8+4+4

// EncodeState serialises the four state fields into the 32-byte layout that the
// injected trampoline reads from the state struct.  Pass the result to
// procmem.WriteMem at the handle's StateAddr to update the fake time without
// stopping the target process.
func EncodeState(offsetSec, offsetNsec int64, mask uint64, generation uint32) []byte {
	b := make([]byte, StateSize)
	binary.LittleEndian.PutUint64(b[0:], uint64(offsetSec))
	binary.LittleEndian.PutUint64(b[8:], uint64(offsetNsec))
	binary.LittleEndian.PutUint64(b[16:], mask)
	binary.LittleEndian.PutUint32(b[24:], generation)
	// b[28:32] is padding; already zero.
	return b
}

// DecodeState deserialises the four state fields from a 32-byte buffer.
func DecodeState(b []byte) (offsetSec, offsetNsec int64, mask uint64, generation uint32, err error) {
	if len(b) < StateSize {
		err = fmt.Errorf("trampoline: DecodeState: buffer too short (%d < %d)", len(b), StateSize)
		return
	}
	offsetSec = int64(binary.LittleEndian.Uint64(b[0:]))
	offsetNsec = int64(binary.LittleEndian.Uint64(b[8:]))
	mask = binary.LittleEndian.Uint64(b[16:])
	generation = binary.LittleEndian.Uint32(b[24:])
	return
}
