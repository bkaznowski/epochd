/* pkg/trampoline/trampoline.c
 *
 * Position-independent x86-64 vDSO hook payloads for clock_gettime,
 * gettimeofday, and time. inject.go JMP-patches each function's real vDSO
 * entry point to the corresponding *_entry symbol below; all three share the
 * single g_state struct, which must stay the LAST thing in the blob so that
 * StateOffset == len(Payload) - StateSize (see link.ld and trampoline.go).
 *
 * Build with ./build.sh (a Docker build; see that file). It compiles this file
 * and link.ld into a flat blob, checks that the result is genuinely
 * position-independent, and regenerates trampoline.bin and
 * offsets_generated.go from the linker's symbol table, so the entry offsets
 * are never counted by hand.
 *
 * Freestanding: no libc, no CRT startup, no stack protector. Calling the real
 * libc clock_gettime()/gettimeofday()/time() from here would recurse into the
 * vDSO entry we just replaced, so the raw_syscallN() helpers below -- each one
 * inline `syscall` instruction -- are the only assembly in this file.
 * Everything else (register allocation, spilling arguments across the
 * syscall, the SysV calling convention these functions are entered with) is
 * ordinary C left to the compiler.
 *
 * State struct layout (shared by all three hooks, written by the Go layer via
 * process_vm_writev -- see trampoline.go's Field* constants):
 *   +0   int64  offset_sec    advancing: added to the real tv_sec
 *                             frozen:    the absolute tv_sec to return
 *   +8   int64  offset_nsec   advancing: added to the real tv_nsec
 *                             frozen:    the absolute tv_nsec to return
 *   +16  uint64 enabled_mask  bit 0 = interception enabled
 *                             bit 1 = freeze mode (no real read at all)
 *   +24  uint32 generation    bumped on every SetTime/Freeze, for observability
 *   +28  uint32 _pad
 *
 * Error handling: a hooked function returns what the real vDSO function
 * would -- 0 on success, a negative errno on failure -- so a caller probing an
 * unsupported clock or passing a bad pointer sees the real error rather than
 * a bogus success. When the real read fails the offset is not applied.
 */

#include <stdint.h>

#define SYS_clock_gettime 228
#define SYS_gettimeofday   96
#define SYS_time          201

#define CLOCK_REALTIME        0
#define CLOCK_REALTIME_COARSE 5

#define MASK_ENABLED 1u
#define MASK_FROZEN  2u

#define NS_PER_SEC 1000000000LL
#define US_PER_SEC 1000000LL

struct timespec {
    int64_t tv_sec;
    int64_t tv_nsec;
};

struct timeval {
    int64_t tv_sec;
    int64_t tv_usec;
};

struct state {
    int64_t  offset_sec;
    int64_t  offset_nsec;
    uint64_t enabled_mask;
    uint32_t generation;
    uint32_t _pad;
};

/* Kept in its own linker section so link.ld can place it last. `volatile`
 * stops the compiler from caching or eliding reads of memory that another
 * process writes behind its back; `used` keeps it alive without references
 * from outside this translation unit. */
__attribute__((section(".tramp_state"), used))
static volatile struct state g_state = { 0, 0, MASK_ENABLED, 0, 0 };

/* The `syscall` instruction clobbers rcx (return rip) and r11 (rflags). */
static inline long raw_syscall1(long nr, long a1)
{
    long ret;
    __asm__ volatile("syscall"
                     : "=a"(ret)
                     : "0"(nr), "D"(a1)
                     : "rcx", "r11", "memory");
    return ret;
}

static inline long raw_syscall2(long nr, long a1, long a2)
{
    long ret;
    __asm__ volatile("syscall"
                     : "=a"(ret)
                     : "0"(nr), "D"(a1), "S"(a2)
                     : "rcx", "r11", "memory");
    return ret;
}

/* One correction step is enough: the Go layer keeps offset_nsec in
 * (-1e9, 1e9), so adding it to a normalised fraction overshoots by at most
 * one unit. */
static inline void normalize(int64_t *sec, int64_t *frac, int64_t unit)
{
    if (*frac >= unit) {
        *frac -= unit;
        *sec += 1;
    } else if (*frac < 0) {
        *frac += unit;
        *sec -= 1;
    }
}

/* clock_gettime(clockid_t clk_id, struct timespec *tp)
 *
 * Intercepts CLOCK_REALTIME and CLOCK_REALTIME_COARSE only; every other
 * clk_id (CLOCK_MONOTONIC and friends) passes straight through so a target's
 * internal timeout/latency logic keeps seeing real elapsed time. Advancing
 * mode reads the real clock with the caller's own clk_id, so COARSE stays
 * coarse.
 */
__attribute__((section(".tramp_text.clock_gettime"), used))
int clock_gettime_entry(int clk_id, struct timespec *tp)
{
    if (clk_id != CLOCK_REALTIME && clk_id != CLOCK_REALTIME_COARSE)
        return (int)raw_syscall2(SYS_clock_gettime, clk_id, (long)tp);

    uint64_t mask = g_state.enabled_mask;
    if (!(mask & MASK_ENABLED))
        return (int)raw_syscall2(SYS_clock_gettime, clk_id, (long)tp);

    if (mask & MASK_FROZEN) {
        tp->tv_sec  = g_state.offset_sec;
        tp->tv_nsec = g_state.offset_nsec;
        return 0;
    }

    long rc = raw_syscall2(SYS_clock_gettime, clk_id, (long)tp);
    if (rc)
        return (int)rc;
    int64_t sec  = tp->tv_sec + g_state.offset_sec;
    int64_t nsec = tp->tv_nsec + g_state.offset_nsec;
    normalize(&sec, &nsec, NS_PER_SEC);
    tp->tv_sec  = sec;
    tp->tv_nsec = nsec;
    return 0;
}

/* gettimeofday(struct timeval *tv, struct timezone *tz)
 *
 * struct timeval is the same 16-byte {sec, frac} shape as struct timespec,
 * just microseconds instead of nanoseconds. This is PostgreSQL's
 * GetCurrentTimestamp() and glibc/bash's own wall-clock reads (e.g. bash's
 * $EPOCHREALTIME) -- neither of which goes through clock_gettime's vDSO entry
 * at all, hence hooking it separately. tz is passed through untouched to the
 * real syscall.
 */
__attribute__((section(".tramp_text.gettimeofday"), used))
int gettimeofday_entry(struct timeval *tv, void *tz)
{
    uint64_t mask = g_state.enabled_mask;
    if (!(mask & MASK_ENABLED))
        return (int)raw_syscall2(SYS_gettimeofday, (long)tv, (long)tz);

    if (mask & MASK_FROZEN) {
        if (tv) {
            tv->tv_sec  = g_state.offset_sec;
            tv->tv_usec = g_state.offset_nsec / 1000;
        }
        return 0;
    }

    long rc = raw_syscall2(SYS_gettimeofday, (long)tv, (long)tz);
    if (rc || !tv)
        return (int)rc;
    int64_t sec  = tv->tv_sec + g_state.offset_sec;
    int64_t usec = tv->tv_usec + g_state.offset_nsec / 1000; /* truncates toward zero */
    normalize(&sec, &usec, US_PER_SEC);
    tv->tv_sec  = sec;
    tv->tv_usec = usec;
    return 0;
}

/* time(time_t *tloc) -> seconds since epoch, also stored to *tloc if non-NULL.
 *
 * Deliberately does not use the time() syscall for the advancing-mode real
 * reading: SYS_time only ever returns whole seconds, discarding the real
 * fractional second before we would ever see it, so a nonzero offset_nsec
 * could silently carry or borrow across a second boundary this function had
 * no way to detect, making it disagree with clock_gettime/gettimeofday about
 * the current second by up to 1s for the same instant. Instead this reads the
 * real clock_gettime(CLOCK_REALTIME) and applies the same carry-normalised
 * add those two use, so all three hooks always agree. If that read fails it
 * falls back to the real time() syscall rather than returning garbage.
 */
__attribute__((section(".tramp_text.time"), used))
long time_entry(long *tloc)
{
    uint64_t mask = g_state.enabled_mask;
    if (!(mask & MASK_ENABLED))
        return raw_syscall1(SYS_time, (long)tloc);

    long result;
    if (mask & MASK_FROZEN) {
        result = g_state.offset_sec;
    } else {
        struct timespec ts;
        if (raw_syscall2(SYS_clock_gettime, CLOCK_REALTIME, (long)&ts))
            return raw_syscall1(SYS_time, (long)tloc);
        int64_t sec  = ts.tv_sec + g_state.offset_sec;
        int64_t nsec = ts.tv_nsec + g_state.offset_nsec;
        normalize(&sec, &nsec, NS_PER_SEC); /* only the carry into sec matters */
        result = sec;
    }
    if (tloc)
        *tloc = result;
    return result;
}
