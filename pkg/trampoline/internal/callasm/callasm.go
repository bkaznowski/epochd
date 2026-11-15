//go:build linux && amd64

// Package callasm calls a System V x86-64 function by address from Go. It
// exists only so tests can run the trampoline's machine code in-process, with
// no ptrace and no cgo, and check its behaviour directly.
package callasm

// Call calls the function at entry with arg1 in rdi and arg2 in rsi and returns
// rax. The function must be a leaf that follows the System V ABI, uses little
// stack, and does not need the Go runtime (the trampoline hooks qualify: they
// make raw syscalls and touch only their arguments and their own state).
//
//go:noescape
func Call(entry, arg1, arg2 uintptr) uintptr
