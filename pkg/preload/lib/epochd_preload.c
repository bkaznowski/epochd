/* pkg/preload/lib/epochd_preload.c
 *
 * LD_PRELOAD implementation of epochd's fake wall clock: interposes libc's
 * clock_gettime, gettimeofday and time. It is the preload counterpart of the
 * vDSO trampoline (pkg/trampoline) and implements the same semantics:
 *
 *   - only CLOCK_REALTIME and CLOCK_REALTIME_COARSE are affected; monotonic
 *     clocks, and therefore timeouts and timers, keep running on real time
 *   - advancing mode: result = real time + offset
 *   - frozen mode:    result = the stored absolute timestamp, no real read
 *
 * State lives in a small file that the controlling process (pkg/preload)
 * maps writable and this library maps read-only (MAP_SHARED), named by
 * $EPOCHD_STATE_FILE. Because the state is shared memory, it can be changed
 * while the process tree is running, and every process that inherits the
 * environment (fork or exec) sees the same clock with no ptrace involved.
 *
 * Unlike the trampoline, the advancing path calls the REAL clock_gettime
 * through dlsym(RTLD_NEXT), which still takes libc's vDSO fast path; the
 * trampoline has to issue a real syscall.
 *
 * File layout (32 bytes, little endian; same field order as the trampoline):
 *   +0   int64  offset_sec   advancing: added to tv_sec;  frozen: absolute tv_sec
 *   +8   int64  offset_nsec  advancing: added to tv_nsec; frozen: absolute tv_nsec
 *   +16  uint64 mask         bit 0 = enabled, bit 1 = frozen
 *   +24  uint32 seq          seqlock: odd while the writer is mid-update
 *   +28  uint32 attached     number of processes that loaded this library
 *
 * `attached` is how the controlling side learns the library was never loaded
 * (a static binary, a scrubbed environment, a noexec $TMPDIR...), which would
 * otherwise look exactly like "the clock is not working". The library maps the
 * file writable only to bump it; a process that cannot open it for writing
 * (another uid) falls back to a read-only map and is simply not counted.
 *
 * What it cannot affect: statically linked binaries, Go binaries (the Go
 * runtime reads the vDSO directly, not libc), raw syscall(SYS_clock_gettime)
 * calls, and processes started with a scrubbed environment.
 *
 * Build against the OLDEST glibc to support (Redpanda bundles 2.35) and keep
 * the imports minimal; a library built against an older glibc loads on newer
 * ones, not the reverse. It is also linked without any DT_NEEDED entry and
 * without fortify/stack-protector helpers, so the same file loads under musl
 * (Alpine), whose loader has no libc.so.6 to satisfy a dependency on it and no
 * __*_chk functions; every symbol it uses is resolved from the process's own
 * libc at load time. See Dockerfile in this directory.
 */
#define _GNU_SOURCE /* RTLD_NEXT */
#include <dlfcn.h>
#include <fcntl.h>
#include <sched.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <sys/mman.h>
#include <sys/select.h> /* struct timeval; deliberately not <sys/time.h> */
#include <time.h>
#include <unistd.h>

#define STATE_SIZE   32
#define MASK_ENABLED 1u
#define MASK_FROZEN  2u

/* A writer that dies mid-update leaves seq odd forever; give up after this
 * many attempts instead of hanging the process. A live writer holds seq odd for
 * a few nanoseconds, but it can be preempted inside that window, so the reader
 * yields the CPU now and then rather than spinning through its whole budget
 * while the writer cannot run. */
#define MAX_SNAPSHOT_TRIES 4096
#define YIELD_EVERY        64

typedef struct {
    int64_t  offset_sec;
    int64_t  offset_nsec;
    uint64_t mask;
    uint32_t seq;
    uint32_t attached;
} State;

typedef struct {
    int64_t  sec;
    int64_t  nsec;
    uint64_t mask;
} Snapshot;

static State *state_map; /* NULL means pass through to real time */
static int init_done;

/* The last consistent snapshot this thread read. If the writer died mid-update
 * (seq stuck odd), carrying on with the clock as it last stood beats snapping
 * back to real time, which the process would see as a jump of the whole offset.
 * Per thread, so a fallback read can never be torn. */
static __thread Snapshot last_good;
static __thread int have_last_good;

static int (*real_clock_gettime)(clockid_t, struct timespec *);
static int (*real_gettimeofday)(struct timeval *, void *);
static time_t (*real_time)(time_t *);

static void init_state(void)
{
    if (__atomic_load_n(&init_done, __ATOMIC_ACQUIRE))
        return;

    const char *path = getenv("EPOCHD_STATE_FILE");
    if (path && *path) {
        /* Writable only so this process can be counted; the clock fields are
         * never written from here. Fall back to read-only when we may not. */
        int writable = 1;
        int fd = open(path, O_RDWR | O_CLOEXEC);
        if (fd < 0) {
            writable = 0;
            fd = open(path, O_RDONLY | O_CLOEXEC);
        }
        if (fd >= 0) {
            void *p = mmap(NULL, STATE_SIZE, writable ? PROT_READ | PROT_WRITE : PROT_READ,
                           MAP_SHARED, fd, 0);
            close(fd);
            if (p != MAP_FAILED) {
                state_map = p;
                if (writable)
                    __atomic_fetch_add(&state_map->attached, 1, __ATOMIC_RELAXED);
            }
        }
    }

    if (getenv("EPOCHD_PRELOAD_DEBUG")) {
        dprintf(2, "[epochd-preload] pid=%d state=%s mapped=%s\n", (int)getpid(),
                path ? path : "(unset)", state_map ? "yes" : "no");
    }
    __atomic_store_n(&init_done, 1, __ATOMIC_RELEASE);
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

/* Consistent read of the shared state (seqlock reader). Returns nonzero when
 * the fake clock is active. */
static int read_state(Snapshot *out)
{
    init_state();
    const State *s = state_map;
    if (!s)
        return 0;

    for (int i = 0; i < MAX_SNAPSHOT_TRIES; i++) {
        if (i && i % YIELD_EVERY == 0)
            sched_yield();
        uint32_t before = __atomic_load_n(&s->seq, __ATOMIC_ACQUIRE);
        if (before & 1u)
            continue;
        out->sec  = __atomic_load_n(&s->offset_sec, __ATOMIC_RELAXED);
        out->nsec = __atomic_load_n(&s->offset_nsec, __ATOMIC_RELAXED);
        out->mask = __atomic_load_n(&s->mask, __ATOMIC_RELAXED);
        __atomic_thread_fence(__ATOMIC_ACQUIRE);
        if (__atomic_load_n(&s->seq, __ATOMIC_RELAXED) == before) {
            last_good      = *out;
            have_last_good = 1;
            return (out->mask & MASK_ENABLED) != 0;
        }
    }

    /* The writer never finished: use what this thread last saw. A thread that
     * has seen nothing yet has no clock to continue, so it reads real time. */
    if (have_last_good) {
        *out = last_good;
        return (out->mask & MASK_ENABLED) != 0;
    }
    return 0;
}

/* One correction step is enough: the writer keeps offset_nsec in (-1e9, 1e9). */
static void normalise(int64_t *sec, int64_t *frac, int64_t per_sec)
{
    if (*frac >= per_sec) {
        *frac -= per_sec;
        (*sec)++;
    } else if (*frac < 0) {
        *frac += per_sec;
        (*sec)--;
    }
}

int clock_gettime(clockid_t clk, struct timespec *ts)
{
    resolve();
    Snapshot s;
    if ((clk != CLOCK_REALTIME && clk != CLOCK_REALTIME_COARSE) || !read_state(&s))
        return real_clock_gettime(clk, ts);

    if (s.mask & MASK_FROZEN) {
        ts->tv_sec  = s.sec;
        ts->tv_nsec = s.nsec;
        return 0;
    }

    int rc = real_clock_gettime(clk, ts);
    if (rc == 0) {
        int64_t sec  = ts->tv_sec + s.sec;
        int64_t nsec = ts->tv_nsec + s.nsec;
        normalise(&sec, &nsec, 1000000000);
        ts->tv_sec  = sec;
        ts->tv_nsec = nsec;
    }
    return rc;
}

int gettimeofday(struct timeval *tv, void *tz)
{
    resolve();
    Snapshot s;
    if (!read_state(&s))
        return real_gettimeofday(tv, tz);

    if (s.mask & MASK_FROZEN) {
        if (tv) {
            tv->tv_sec  = s.sec;
            tv->tv_usec = s.nsec / 1000;
        }
        return 0;
    }

    int rc = real_gettimeofday(tv, tz);
    if (rc == 0 && tv) {
        int64_t sec  = tv->tv_sec + s.sec;
        int64_t usec = tv->tv_usec + s.nsec / 1000; /* truncates toward zero, like the trampoline */
        normalise(&sec, &usec, 1000000);
        tv->tv_sec  = sec;
        tv->tv_usec = usec;
    }
    return rc;
}

/* time() derives its answer from clock_gettime rather than the real time():
 * time() only returns whole seconds, so a nonzero offset_nsec could carry or
 * borrow across a second boundary it cannot see, making it disagree with
 * clock_gettime and gettimeofday about the current second. */
time_t time(time_t *out)
{
    resolve();
    Snapshot s;
    if (!read_state(&s))
        return real_time(out);

    int64_t sec;
    if (s.mask & MASK_FROZEN) {
        sec = s.sec;
    } else {
        struct timespec ts;
        if (real_clock_gettime(CLOCK_REALTIME, &ts) != 0)
            return real_time(out);
        sec = ts.tv_sec + s.sec;
        int64_t nsec = ts.tv_nsec + s.nsec;
        normalise(&sec, &nsec, 1000000000);
    }
    if (out)
        *out = sec;
    return sec;
}

__attribute__((constructor)) static void epochd_preload_init(void)
{
    init_state();
}
