/* test/preload-probe/probe.c
 *
 * LD_PRELOAD probe: answers "does this process read wall-clock time through
 * libc's clock_gettime / gettimeofday / time, where a preload library can
 * intercept it?"
 *
 * For every interposed call it counts the call per clock id, logs the first
 * occurrence of each (function, clock id) pair to stderr, and adds
 * EPOCHD_PROBE_OFFSET_SEC seconds to wall-clock results (CLOCK_REALTIME and
 * CLOCK_REALTIME_COARSE for clock_gettime; always for gettimeofday and time).
 * With the variable unset or 0 it is a pure observer.
 *
 * Build against the OLDEST glibc you need to support (Redpanda bundles 2.35);
 * a library built against an older glibc loads on newer ones, not the reverse.
 * Keep the imports minimal for the same reason.
 */
#define _GNU_SOURCE
#include <dlfcn.h>
#include <errno.h>
#include <stdarg.h>
#include <stdatomic.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/time.h>
#include <time.h>
#include <unistd.h>

#define NCLK 16 /* clock ids >= NCLK-1 (incl. dynamic CPU-time ids) share the last bucket */

enum { FN_CLOCK_GETTIME, FN_GETTIMEOFDAY, FN_TIME, FN_COUNT };
static const char *const fn_names[FN_COUNT] = {"clock_gettime", "gettimeofday", "time"};

static atomic_ulong calls[FN_COUNT][NCLK];
static atomic_ulong shifted[FN_COUNT];
static atomic_flag seen[FN_COUNT][NCLK];

static int (*real_clock_gettime)(clockid_t, struct timespec *);
static int (*real_gettimeofday)(struct timeval *, void *);
static time_t (*real_time)(time_t *);

static atomic_int offset_state; /* 0 = unparsed, 1 = parsed */
static long offset_sec;

static long get_offset(void)
{
    if (!atomic_load(&offset_state)) {
        const char *v = getenv("EPOCHD_PROBE_OFFSET_SEC");
        offset_sec = v ? strtol(v, NULL, 10) : 0; /* idempotent, so the race is benign */
        atomic_store(&offset_state, 1);
    }
    return offset_sec;
}

/* Resolved lazily: another library's constructor can call us before ours runs. */
static void resolve(void)
{
    if (!real_clock_gettime)
        real_clock_gettime = (int (*)(clockid_t, struct timespec *))dlsym(RTLD_NEXT, "clock_gettime");
    if (!real_gettimeofday)
        real_gettimeofday = (int (*)(struct timeval *, void *))dlsym(RTLD_NEXT, "gettimeofday");
    if (!real_time)
        real_time = (time_t (*)(time_t *))dlsym(RTLD_NEXT, "time");
}

static void say(const char *fmt, ...) __attribute__((format(printf, 1, 2)));
static void say(const char *fmt, ...)
{
    char buf[512];
    int n = snprintf(buf, sizeof buf, "[epochd-probe] pid=%d comm=%s ", (int)getpid(),
                     program_invocation_short_name ? program_invocation_short_name : "?");
    va_list ap;
    va_start(ap, fmt);
    n += vsnprintf(buf + n, sizeof buf - (size_t)n, fmt, ap);
    va_end(ap);
    if (n > (int)sizeof buf - 2) n = (int)sizeof buf - 2;
    buf[n++] = '\n';
    (void)!write(2, buf, (size_t)n); /* raw write: safe before/after stdio teardown */
}

static void note(int fn, int clk)
{
    int b = (clk < 0 || clk >= NCLK) ? NCLK - 1 : clk;
    atomic_fetch_add(&calls[fn][b], 1);
    if (!atomic_flag_test_and_set(&seen[fn][b]))
        say("first %s clk=%d", fn_names[fn], clk);
}

int clock_gettime(clockid_t clk, struct timespec *ts)
{
    resolve();
    note(FN_CLOCK_GETTIME, (int)clk);
    int rc = real_clock_gettime(clk, ts);
    if (rc == 0 && (clk == CLOCK_REALTIME || clk == CLOCK_REALTIME_COARSE) && get_offset()) {
        ts->tv_sec += get_offset();
        atomic_fetch_add(&shifted[FN_CLOCK_GETTIME], 1);
    }
    return rc;
}

int gettimeofday(struct timeval *tv, void *tz)
{
    resolve();
    note(FN_GETTIMEOFDAY, 0);
    int rc = real_gettimeofday(tv, tz);
    if (rc == 0 && tv && get_offset()) {
        tv->tv_sec += get_offset();
        atomic_fetch_add(&shifted[FN_GETTIMEOFDAY], 1);
    }
    return rc;
}

time_t time(time_t *out)
{
    resolve();
    note(FN_TIME, 0);
    time_t t = real_time(NULL);
    if (t != (time_t)-1 && get_offset()) {
        t += get_offset();
        atomic_fetch_add(&shifted[FN_TIME], 1);
    }
    if (out) *out = t;
    return t;
}

__attribute__((constructor)) static void probe_load(void)
{
    say("loaded offset=%ld", get_offset());
}

__attribute__((destructor)) static void probe_summary(void)
{
    for (int fn = 0; fn < FN_COUNT; fn++) {
        unsigned long total = 0;
        for (int c = 0; c < NCLK; c++) total += atomic_load(&calls[fn][c]);
        if (!total) continue;
        char clks[256] = "";
        size_t used = 0;
        for (int c = 0; c < NCLK && used < sizeof clks - 32; c++) {
            unsigned long n = atomic_load(&calls[fn][c]);
            if (n) used += (size_t)snprintf(clks + used, sizeof clks - used, "%d:%lu ", c, n);
        }
        say("summary %s calls=%lu shifted=%lu by-clk[%s]", fn_names[fn], total,
            atomic_load(&shifted[fn]), clks);
    }
}
