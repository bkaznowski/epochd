; pkg/trampoline/trampoline.asm
;
; Position-independent x86-64 vDSO hook payloads for clock_gettime,
; gettimeofday, and time. Assemble: nasm -f bin trampoline.asm -o trampoline.bin
; (.asm extension keeps Go's plan9 assembler from touching this file.)
;
; Three independent entry points, one per hooked vDSO function. inject.go
; JMP-patches each function's real vDSO entry point to the corresponding
; label below. All three share the single state struct at the `state` label,
; which must remain the LAST thing in this file so that
; StateOffset == len(Payload) - StateSize holds regardless of how much hook
; code precedes it (see trampoline.go / TestStateOffsetRegression).
;
; State struct layout (shared by all three hooks):
;   +0   int64  offsetSec    -- advancing mode: offset added to real tv_sec
;                              freeze mode:    absolute frozen tv_sec
;   +8   int64  offsetNsec   -- advancing mode: offset added to real tv_nsec
;                              freeze mode:    absolute frozen tv_nsec
;   +16  uint64 enabledMask  -- bit 0 = interception enabled
;                              bit 1 = freeze mode (return stored timestamp, no syscall)
;   +24  uint32 generation   -- bumped on every SetTime/Freeze for observability
;   +28  uint32 _pad

BITS 64

; =============================================================================
; clock_gettime(int clk_id /*rdi*/, struct timespec *tp /*rsi*/)
; Calling convention matches both the vDSO stub and the raw Linux syscall ABI
; for the first two args, so no shuffling is required. Intercepts both
; CLOCK_REALTIME and CLOCK_REALTIME_COARSE (same wall clock, the latter is
; just a faster/coarser read of it) -- every other clk_id (CLOCK_MONOTONIC
; and friends) passes straight through so internal timeout/latency logic in
; the target process keeps seeing real elapsed time.
; =============================================================================
clock_gettime_entry:
    cmp     edi, 0              ; CLOCK_REALTIME == 0; edi saves a REX prefix
    je      .maybe_intercept
    cmp     edi, 5              ; CLOCK_REALTIME_COARSE == 5
    jne     .real_syscall

.maybe_intercept:
    lea     r11, [rel state]
    test    byte [r11 + 16], 1  ; bit 0: interception enabled?
    jz      .real_syscall
    test    byte [r11 + 16], 2  ; bit 1: freeze mode?
    jnz     .freeze

    ; Advancing mode: invoke the real syscall, then apply the stored offset.
    push    rdi
    push    rsi
    mov     eax, 228            ; SYS_clock_gettime (shorter than mov rax,228)
    syscall
    pop     rsi
    pop     rdi

    ; Reload r11 -- the Linux x86-64 syscall ABI clobbers r11 (saves RFLAGS).
    lea     r11, [rel state]
    mov     r8,  [r11]          ; offsetSec
    mov     r9,  [r11 + 8]      ; offsetNsec

    add     [rsi],     r8       ; tp->tv_sec  += offsetSec
    add     [rsi + 8], r9       ; tp->tv_nsec += offsetNsec

    ; Normalise tv_nsec into [0, 1e9). A single correction step is enough
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
    jmp     .done

.freeze:
    ; Return the stored absolute timestamp without invoking the real syscall.
    mov     r8,  [r11]          ; frozen tv_sec
    mov     r9,  [r11 + 8]      ; frozen tv_nsec
    mov     [rsi],     r8
    mov     [rsi + 8], r9
    jmp     .done

.real_syscall:
    push    rdi
    push    rsi
    mov     eax, 228            ; SYS_clock_gettime
    syscall
    pop     rsi
    pop     rdi

.done:
    xor     eax, eax            ; return 0; xor eax saves a REX prefix vs xor rax
    ret

; =============================================================================
; gettimeofday(struct timeval *tv /*rdi*/, struct timezone *tz /*rsi*/)
; struct timeval on x86-64 is {int64 tv_sec; int64 tv_usec;} -- the same
; 16-byte layout as struct timespec, just microseconds instead of nanoseconds.
; This is PostgreSQL's GetCurrentTimestamp() source, and glibc/bash's own
; internal wall-clock reads (e.g. bash's $EPOCHREALTIME) -- neither of which
; goes through clock_gettime's vDSO entry at all.
; =============================================================================
gettimeofday_entry:
    lea     r11, [rel state]
    test    byte [r11 + 16], 1
    jz      .real_syscall
    test    byte [r11 + 16], 2
    jnz     .freeze

    push    rdi
    push    rsi
    mov     eax, 96             ; SYS_gettimeofday
    syscall
    pop     rsi
    pop     rdi

    test    rdi, rdi            ; tv may be NULL
    jz      .done

    lea     r11, [rel state]    ; reload -- syscall clobbers r11
    mov     r8,  [r11]          ; offsetSec
    mov     rax, [r11 + 8]      ; offsetNsec
    cqo
    mov     rcx, 1000
    idiv    rcx                 ; rax = offsetNsec / 1000 (truncating -> usec)

    add     [rdi],     r8       ; tv->tv_sec  += offsetSec
    add     [rdi + 8], rax      ; tv->tv_usec += offsetUsec

    mov     rax, [rdi + 8]
    cmp     rax, 1000000
    jl      .check_negative
    sub     rax, 1000000
    mov     [rdi + 8], rax
    inc     qword [rdi]
    jmp     .done

.check_negative:
    cmp     rax, 0
    jge     .done
    add     rax, 1000000
    mov     [rdi + 8], rax
    dec     qword [rdi]
    jmp     .done

.freeze:
    test    rdi, rdi
    jz      .done
    mov     r8,  [r11]          ; frozen tv_sec
    mov     rax, [r11 + 8]      ; frozen tv_nsec
    cqo
    mov     rcx, 1000
    idiv    rcx                 ; rax = frozen nsec / 1000 = frozen usec
    mov     [rdi],     r8
    mov     [rdi + 8], rax
    jmp     .done

.real_syscall:
    push    rdi
    push    rsi
    mov     eax, 96             ; SYS_gettimeofday
    syscall
    pop     rsi
    pop     rdi

.done:
    xor     eax, eax
    ret

; =============================================================================
; time(time_t *tloc /*rdi*/) -> seconds since epoch in RAX; also stores to
; *tloc if non-NULL.
;
; Deliberately does NOT use the time() syscall for the advancing-mode real
; reading: SYS_time only ever returns whole seconds, discarding the real
; fractional second before we ever see it, so a nonzero offsetNsec could
; silently carry/borrow across a second boundary that this function had no
; way to detect -- making it disagree with clock_gettime/gettimeofday about
; the current second by up to 1s for the same instant. Instead this calls
; the real clock_gettime(CLOCK_REALTIME) internally, applies the same
; carry-normalised add those two stubs use, and returns only the resulting
; tv_sec, so all three hooked functions always agree to the second.
; =============================================================================
time_entry:
    lea     r11, [rel state]
    test    byte [r11 + 16], 1
    jz      .real_time
    test    byte [r11 + 16], 2
    jnz     .freeze

    ; Advancing mode.
    push    rdi                 ; caller's tloc (may be NULL)
    sub     rsp, 16             ; scratch struct timespec on our own stack
    xor     edi, edi            ; CLOCK_REALTIME
    mov     rsi, rsp
    mov     eax, 228            ; SYS_clock_gettime
    syscall

    lea     r11, [rel state]    ; reload -- syscall clobbers r11
    mov     r8,  [r11]          ; offsetSec
    mov     r9,  [r11 + 8]      ; offsetNsec
    add     [rsp],     r8       ; ts.tv_sec  += offsetSec
    add     [rsp + 8], r9       ; ts.tv_nsec += offsetNsec

    ; We only need the carry/borrow into tv_sec -- tv_nsec itself is discarded.
    mov     rax, [rsp + 8]
    cmp     rax, 1000000000
    jl      .check_negative
    inc     qword [rsp]
    jmp     .load_result

.check_negative:
    cmp     rax, 0
    jge     .load_result
    dec     qword [rsp]

.load_result:
    mov     rax, [rsp]          ; final tv_sec, carry/borrow applied
    add     rsp, 16
    pop     rdi
    test    rdi, rdi
    jz      .done
    mov     [rdi], rax
    jmp     .done

.freeze:
    mov     rax, [r11]          ; frozen tv_sec
    test    rdi, rdi
    jz      .done
    mov     [rdi], rax
    jmp     .done

.real_time:
    mov     eax, 201            ; SYS_time
    syscall

.done:
    ret                         ; NOTE: unlike the other two, the return value
                                ; IS the seconds count -- do not zero rax.

; ----------------------------------------------------------------------------
; State struct -- written by the Go layer via process_vm_writev.
; Must stay last: StateOffset == len(Payload) - StateSize (see trampoline.go).
; ----------------------------------------------------------------------------
state:
    dq  0               ; offsetSec/frozenSec   (+0)
    dq  0               ; offsetNsec/frozenNsec  (+8)
    dq  1               ; enabledMask            (+16)  bit 0=enabled, bit 1=freeze
    dd  0               ; generation             (+24)
    dd  0               ; _pad                   (+28)
