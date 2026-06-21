//go:build linux

package vdso

import (
	"bufio"
	"bytes"
	"debug/elf"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// VDSOInfo holds the vDSO mapping range and the resolved address of clock_gettime
// within the target process's address space.
type VDSOInfo struct {
	Start, End       uintptr
	ClockGettimeAddr uintptr
}

// Locate finds the vDSO mapping in the given process and resolves the absolute
// address of clock_gettime. The caller must already hold ptrace attachment (or be
// reading their own process) since /proc/<pid>/mem requires ptrace permission.
func Locate(pid int) (*VDSOInfo, error) {
	start, end, err := findVDSORange(pid)
	if err != nil {
		return nil, err
	}

	data, err := readProcMem(pid, start, end-start)
	if err != nil {
		return nil, err
	}

	ef, err := elf.NewFile(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("vdso: parsing ELF: %w", err)
	}
	defer ef.Close()

	symVal, err := resolveCGTSymbol(ef)
	if err != nil {
		return nil, err
	}

	addr := start + uintptr(symVal)
	if addr < start || addr >= end {
		return nil, fmt.Errorf("vdso: clock_gettime symbol value 0x%x produces address 0x%x outside vDSO [0x%x, 0x%x)", symVal, addr, start, end)
	}

	return &VDSOInfo{
		Start:            start,
		End:              end,
		ClockGettimeAddr: addr,
	}, nil
}

// resolveCGTSymbol looks up clock_gettime (preferred) or __vdso_clock_gettime
// (fallback) in the ELF dynamic symbol table and returns the symbol's value
// (relative offset from vDSO base).
func resolveCGTSymbol(ef *elf.File) (uint64, error) {
	syms, err := ef.DynamicSymbols()
	if err != nil {
		return 0, fmt.Errorf("vdso: reading dynamic symbols: %w", err)
	}

	var preferred, fallback uint64
	var hasPreferred, hasFallback bool
	for _, sym := range syms {
		switch sym.Name {
		case "clock_gettime":
			preferred = sym.Value
			hasPreferred = true
		case "__vdso_clock_gettime":
			fallback = sym.Value
			hasFallback = true
		}
	}

	if hasPreferred {
		return preferred, nil
	}
	if hasFallback {
		return fallback, nil
	}
	return 0, fmt.Errorf("vdso: neither clock_gettime nor __vdso_clock_gettime found in dynamic symbol table")
}

// findVDSORange parses /proc/<pid>/maps and returns the start and end addresses
// of the [vdso] mapping.
func findVDSORange(pid int) (start, end uintptr, err error) {
	f, err := os.Open(fmt.Sprintf("/proc/%d/maps", pid))
	if err != nil {
		return 0, 0, fmt.Errorf("vdso: opening maps: %w", err)
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		fields := strings.Fields(line)
		if len(fields) < 6 || fields[5] != "[vdso]" {
			continue
		}
		dash := strings.IndexByte(fields[0], '-')
		if dash < 0 {
			return 0, 0, fmt.Errorf("vdso: malformed address range %q", fields[0])
		}
		s, err := strconv.ParseUint(fields[0][:dash], 16, 64)
		if err != nil {
			return 0, 0, fmt.Errorf("vdso: parsing start address: %w", err)
		}
		e, err := strconv.ParseUint(fields[0][dash+1:], 16, 64)
		if err != nil {
			return 0, 0, fmt.Errorf("vdso: parsing end address: %w", err)
		}
		return uintptr(s), uintptr(e), nil
	}
	if err := scanner.Err(); err != nil {
		return 0, 0, fmt.Errorf("vdso: scanning maps: %w", err)
	}
	return 0, 0, fmt.Errorf("vdso: no [vdso] mapping found in /proc/%d/maps", pid)
}

// readProcMem reads size bytes from addr in the given process via /proc/<pid>/mem.
func readProcMem(pid int, addr, size uintptr) ([]byte, error) {
	f, err := os.Open(fmt.Sprintf("/proc/%d/mem", pid))
	if err != nil {
		return nil, fmt.Errorf("vdso: opening mem: %w", err)
	}
	defer f.Close()

	buf := make([]byte, size)
	n, err := f.ReadAt(buf, int64(addr))
	if err != nil {
		return nil, fmt.Errorf("vdso: reading /proc/%d/mem at 0x%x: %w", pid, addr, err)
	}
	if n != int(size) {
		return nil, fmt.Errorf("vdso: short read from /proc/%d/mem: got %d, want %d", pid, n, size)
	}
	return buf, nil
}
