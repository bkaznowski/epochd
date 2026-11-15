//go:build linux

#include "textflag.h"

// func Call(entry, arg1, arg2 uintptr) uintptr
TEXT ·Call(SB), NOSPLIT, $0-32
	MOVQ entry+0(FP), AX
	MOVQ arg1+8(FP), DI
	MOVQ arg2+16(FP), SI
	MOVQ SP, BX       // rbx is callee-saved in the System V ABI, so it survives the call
	ANDQ $~15, SP     // the ABI wants a 16-byte aligned stack at the call
	CALL AX
	MOVQ BX, SP
	MOVQ AX, ret+24(FP)
	RET
