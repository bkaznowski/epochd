; pkg/trampoline/trampoline.asm
;
; Position-independent x86-64 clock_gettime hook payload.
; Assemble: nasm -f bin trampoline.asm -o trampoline.bin
; (.asm extension keeps Go's plan9 assembler from touching this file.)
;
; Calling convention on entry (matches both the vDSO stub and the raw Linux
; syscall ABI for the first two args, so no shuffling is required):
;   rdi = clk_id   (int)
;   rsi = tp       (struct timespec *)
;
; State struct layout immediately follows the code at label `state`:
;   +0   int64  offsetSec    — added to tp->tv_sec
;   +8   int64  offsetNsec   — added to tp->tv_nsec (normalised below)
;   +16  uint64 enabledMask  — bit 0 = CLOCK_REALTIME enabled
;   +24  uint32 generation   — bumped on every SetTime for observability
;   +28  uint32 _pad

BITS 64

trampoline:
    push    rdi
    push    rsi
    mov     eax, 228            ; SYS_clock_gettime (shorter than mov rax,228)
    syscall                     ; get real time first — always
    pop     rsi
    pop     rdi

    cmp     edi, 0              ; CLOCK_REALTIME == 0; edi saves a REX prefix
    jne     .done

    lea     r11, [rel state]    ; RIP-relative ptr to state struct
    mov     r8,  [r11]          ; offsetSec
    mov     r9,  [r11 + 8]      ; offsetNsec

    add     [rsi],     r8       ; tp->tv_sec  += offsetSec
    add     [rsi + 8], r9       ; tp->tv_nsec += offsetNsec

    ; Normalise tv_nsec into [0, 1e9).  A single correction step is enough
    ; because offsetNsec is always kept in (-1e9, 1e9) by the Go layer.
    mov     rax, [rsi + 8]
    cmp     rax, 1000000000
    jl      .check_negative
    sub     rax, 1000000000
    mov     [rsi + 8], rax
    inc     qword [rsi]         ; tv_sec++
    jmp     .done

.check_negative:
    cmp     rax, 0
    jge     .done
    add     rax, 1000000000
    mov     [rsi + 8], rax
    dec     qword [rsi]         ; tv_sec--

.done:
    xor     eax, eax            ; return 0; xor eax saves a REX prefix vs xor rax
    ret

; ----------------------------------------------------------------------------
; State struct — written by the Go layer via process_vm_writev.
; Label offset within this binary == StateOffset in trampoline.go.
; ----------------------------------------------------------------------------
state:
    dq  0               ; offsetSec    (+0)
    dq  0               ; offsetNsec   (+8)
    dq  1               ; enabledMask  (+16)  bit 0 = CLOCK_REALTIME on by default
    dd  0               ; generation   (+24)
    dd  0               ; _pad         (+28)
