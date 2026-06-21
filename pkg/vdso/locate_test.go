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

	var symVal uint64
	var symFound bool
	for _, sym := range syms {
		if sym.Name != "clock_gettime" && sym.Name != "__vdso_clock_gettime" {
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
		t.Fatal("clock_gettime / __vdso_clock_gettime not found in dynamic symbol table")
	}

	// ClockGettimeAddr must be exactly Start + sym.Value, not merely "within range".
	wantAddr := info.Start + uintptr(symVal)
	if info.ClockGettimeAddr != wantAddr {
		t.Errorf("ClockGettimeAddr = 0x%x, want 0x%x (Start + sym.Value)", info.ClockGettimeAddr, wantAddr)
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
		t.Errorf("clock_gettime offset 0x%x is not inside any executable PT_LOAD segment", symVal)
	}

	// Bytes at the resolved offset must not be all zeros — a function has real code.
	offset := uintptr(symVal)
	if int(offset)+8 > len(data) {
		t.Fatalf("symbol offset 0x%x + 8 exceeds vDSO size %d", offset, len(data))
	}
	if bytes.Equal(data[offset:offset+8], make([]byte, 8)) {
		t.Errorf("8 bytes at clock_gettime offset 0x%x are all zeros — address is likely wrong", offset)
	}
}
