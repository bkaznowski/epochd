//go:build linux

package vdso

import (
	"bytes"
	"debug/elf"
	"os"
	"testing"
)

func TestLocateSelf(t *testing.T) {
	pid := os.Getpid()
	info, err := Locate(pid)
	if err != nil {
		t.Fatalf("Locate: %v", err)
	}

	// Manual cross-check (run with -v to see these values, then):
	//   grep vdso /proc/<pid>/maps                                     → confirms range
	//   cp /proc/<pid>/map_files/<start>-<end> /tmp/vdso.so           → dump vDSO
	//   objdump -T /tmp/vdso.so | grep clock_gettime                  → confirms offset
	t.Logf("vDSO range:         [0x%x, 0x%x)  (%d bytes)", info.Start, info.End, info.End-info.Start)
	t.Logf("clock_gettime addr: 0x%x  (offset 0x%x)", info.ClockGettimeAddr, info.ClockGettimeAddr-info.Start)
	t.Logf("gettimeofday addr:  0x%x  (offset 0x%x)", info.GettimeofdayAddr, info.GettimeofdayAddr-info.Start)
	t.Logf("time addr:          0x%x  (offset 0x%x)", info.TimeAddr, info.TimeAddr-info.Start)

	// Re-read the raw vDSO bytes and re-parse ELF independently so the
	// assertions below don't just echo back what Locate already computed.
	data, err := readProcMem(pid, info.Start, info.End-info.Start)
	if err != nil {
		t.Fatalf("readProcMem: %v", err)
	}

	ef, err := elf.NewFile(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("elf.NewFile: %v", err)
	}
	defer ef.Close()

	syms, err := ef.DynamicSymbols()
	if err != nil {
		t.Fatalf("DynamicSymbols: %v", err)
	}

	for _, tc := range []struct {
		names []string
		got   uintptr
	}{
		{[]string{"clock_gettime", "__vdso_clock_gettime"}, info.ClockGettimeAddr},
		{[]string{"gettimeofday", "__vdso_gettimeofday"}, info.GettimeofdayAddr},
		{[]string{"time", "__vdso_time"}, info.TimeAddr},
	} {
		checkResolvedSymbol(t, ef, syms, data, info.Start, tc.names, tc.got)
	}
}

// checkResolvedSymbol independently re-resolves one of names in syms and
// verifies got (what Locate computed) against it, so the assertions don't
// just echo back what Locate already computed.
func checkResolvedSymbol(t *testing.T, ef *elf.File, syms []elf.Symbol, data []byte, vdsoStart uintptr, names []string, got uintptr) {
	t.Helper()

	var symVal uint64
	var symFound bool
	for _, sym := range syms {
		if sym.Name != names[0] && sym.Name != names[1] {
			continue
		}

		// Must be a function, not data or an alias.
		if elf.ST_TYPE(sym.Info) != elf.STT_FUNC {
			t.Errorf("symbol %q: type = %v, want STT_FUNC", sym.Name, elf.ST_TYPE(sym.Info))
		}

		// Real functions have a non-zero size in the symbol table.
		if sym.Size == 0 {
			t.Errorf("symbol %q: size = 0", sym.Name)
		}

		symVal = sym.Value
		symFound = true
		break
	}
	if !symFound {
		t.Fatalf("%s / %s not found in dynamic symbol table", names[0], names[1])
	}

	// got must be exactly Start + sym.Value, not merely "within range".
	wantAddr := vdsoStart + uintptr(symVal)
	if got != wantAddr {
		t.Errorf("%s: addr = 0x%x, want 0x%x (Start + sym.Value)", names[0], got, wantAddr)
	}

	// The offset must fall inside an executable PT_LOAD segment.
	inExecSeg := false
	for _, ph := range ef.Progs {
		if ph.Type != elf.PT_LOAD || ph.Flags&elf.PF_X == 0 {
			continue
		}
		if symVal >= ph.Vaddr && symVal < ph.Vaddr+ph.Filesz {
			inExecSeg = true
			break
		}
	}
	if !inExecSeg {
		t.Errorf("%s offset 0x%x is not inside any executable PT_LOAD segment", names[0], symVal)
	}

	// Bytes at the resolved offset must not be all zeros — a function has real code.
	offset := uintptr(symVal)
	if int(offset)+8 > len(data) {
		t.Fatalf("%s offset 0x%x + 8 exceeds vDSO size %d", names[0], offset, len(data))
	}
	if bytes.Equal(data[offset:offset+8], make([]byte, 8)) {
		t.Errorf("8 bytes at %s offset 0x%x are all zeros — address is likely wrong", names[0], offset)
	}
}
