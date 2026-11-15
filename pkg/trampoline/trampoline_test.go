//go:build linux

package trampoline

import (
	"bytes"
	"crypto/sha256"
	"debug/elf"
	_ "embed"
	"encoding/binary"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// trampolineELF is the linked ELF that trampoline.bin was flattened from. It is
// only embedded here, in the test binary: it lets the tests below check the
// blob against the linker's own symbol table instead of pinning opcode bytes.
//
//go:embed trampoline.elf
var trampolineELF []byte

func loadELF(t *testing.T) *elf.File {
	t.Helper()
	f, err := elf.NewFile(bytes.NewReader(trampolineELF))
	if err != nil {
		t.Fatalf("trampoline.elf is not a valid ELF file: %v", err)
	}
	t.Cleanup(func() { f.Close() })
	return f
}

func symbolAddrs(t *testing.T, f *elf.File) map[string]uint64 {
	t.Helper()
	syms, err := f.Symbols()
	if err != nil {
		t.Fatalf("reading trampoline.elf symbols: %v", err)
	}
	m := make(map[string]uint64, len(syms))
	for _, s := range syms {
		m[s.Name] = s.Value
	}
	return m
}

// TestOffsetsMatchLinkedSymbols proves the generated offset constants describe
// the blob: each one equals the address of the corresponding symbol in the ELF
// the blob was built from. This replaces pinning opcode prefixes, which would
// only encode one compiler version's output.
func TestOffsetsMatchLinkedSymbols(t *testing.T) {
	syms := symbolAddrs(t, loadELF(t))
	for _, c := range []struct {
		symbol string
		got    int
	}{
		{"clock_gettime_entry", ClockGettimeEntryOffset},
		{"gettimeofday_entry", GettimeofdayEntryOffset},
		{"time_entry", TimeEntryOffset},
		{"g_state", StateOffset},
	} {
		addr, ok := syms[c.symbol]
		if !ok {
			t.Errorf("symbol %s missing from trampoline.elf", c.symbol)
			continue
		}
		if uint64(c.got) != addr {
			t.Errorf("%s: constant = %d, linked address = %d; rerun build.sh", c.symbol, c.got, addr)
		}
	}
}

// TestPayloadMatchesELF proves trampoline.bin is the flat image of
// trampoline.elf: the .text section is a prefix of the blob and g_state's
// initial contents sit at StateOffset.
func TestPayloadMatchesELF(t *testing.T) {
	f := loadELF(t)

	text := f.Section(".text")
	data := f.Section(".data")
	if text == nil || data == nil {
		t.Fatal("trampoline.elf must have .text and .data sections")
	}
	textBytes, err := text.Data()
	if err != nil {
		t.Fatal(err)
	}
	dataBytes, err := data.Data()
	if err != nil {
		t.Fatal(err)
	}

	if !bytes.HasPrefix(Payload, textBytes) {
		t.Error("trampoline.bin does not start with the ELF's .text; rerun build.sh")
	}
	if got := Payload[StateOffset:]; !bytes.Equal(got, dataBytes) {
		t.Errorf("state region differs from the ELF's .data:\n got % x\nwant % x", got, dataBytes)
	}
	if len(Payload)-StateSize != StateOffset {
		t.Errorf("g_state must be the last %d bytes: len(Payload)=%d, StateOffset=%d", StateSize, len(Payload), StateOffset)
	}
}

// TestEntryOffsetOrdering checks the structural invariants inject.go relies on:
// clock_gettime first at offset 0, then gettimeofday, then time, all before the
// state, each pointing at real code.
func TestEntryOffsetOrdering(t *testing.T) {
	offsets := []struct {
		name   string
		offset int
	}{
		{"ClockGettimeEntryOffset", ClockGettimeEntryOffset},
		{"GettimeofdayEntryOffset", GettimeofdayEntryOffset},
		{"TimeEntryOffset", TimeEntryOffset},
	}
	if ClockGettimeEntryOffset != 0 {
		t.Errorf("ClockGettimeEntryOffset = %d, want 0", ClockGettimeEntryOffset)
	}
	for i := 1; i < len(offsets); i++ {
		if offsets[i].offset <= offsets[i-1].offset {
			t.Errorf("%s (%d) must come after %s (%d)", offsets[i].name, offsets[i].offset, offsets[i-1].name, offsets[i-1].offset)
		}
	}
	for _, c := range offsets {
		if c.offset < 0 || c.offset >= StateOffset {
			t.Errorf("%s = %d is outside the code region [0, %d)", c.name, c.offset, StateOffset)
		}
	}
}

// TestSourcesMatchBuiltArtifacts fails when trampoline.c or link.ld was edited
// without rerunning build.sh, which would leave the committed blob stale.
func TestSourcesMatchBuiltArtifacts(t *testing.T) {
	recorded, err := os.ReadFile("trampoline.src.sha256")
	if err != nil {
		t.Fatalf("trampoline.src.sha256: %v", err)
	}
	want := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(recorded)), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			t.Fatalf("malformed line in trampoline.src.sha256: %q", line)
		}
		want[fields[1]] = fields[0]
	}
	for _, name := range []string{"trampoline.c", "link.ld"} {
		src, err := os.ReadFile(filepath.Join("src", name))
		if err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(src)
		if got := hex.EncodeToString(sum[:]); got != want[name] {
			t.Errorf("%s changed since the blob was built (hash %s, recorded %s); run build.sh and commit the results", name, got, want[name])
		}
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
