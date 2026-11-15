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
 *   +28  uint32 pad
 *
 * What it cannot affect: statically linked binaries, Go binaries (the Go
 * runtime reads the vDSO directly, not libc), raw syscall(SYS_clock_gettime)
 * calls, and processes started with a scrubbed environment.
 *
 * Build against the OLDEST glibc to support (Redpanda bundles 2.35) and keep
 * the imports minimal; a library built against an older glibc loads on newer
 * ones, not the reverse. See Dockerfile in this directory.
 */
#define _GNU_SOURCE /* RTLD_NEXT */
#include <dlfcn.h>
#include <fcntl.h>
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
 * many attempts and fall back to real time instead of hanging the process. */
#define MAX_SNAPSHOT_TRIES 1000

typedef struct {
    int64_t  offset_sec;
    int64_t  offset_nsec;
    uint64_t mask;
    uint32_t seq;
    uint32_t pad;
} State;

typedef struct {
    int64_t  sec;
    int64_t  nsec;
    uint64_t mask;
} Snapshot;

static const State *state_map; /* NULL means pass through to real time */
static int init_done;

static int (*real_clock_gettime)(clockid_t, struct timespec *);
static int (*real_gettimeofday)(struct timeval *, void *);
static time_t (*real_time)(time_t *);

static void init_state(void)
{
    if (__atomic_load_n(&init_done, __ATOMIC_ACQUIRE))
        return;

    const char *path = getenv("EPOCHD_STATE_FILE");
    if (path && *path) {
        int fd = open(path, O_RDONLY | O_CLOEXEC);
        if (fd >= 0) {
            void *p = mmap(NULL, STATE_SIZE, PROT_READ, MAP_SHARED, fd, 0);
            close(fd);
            if (p != MAP_FAILED)
                state_map = p;
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
        uint32_t before = __atomic_load_n(&s->seq, __ATOMIC_ACQUIRE);
        if (before & 1u)
            continue;
        out->sec  = __atomic_load_n(&s->offset_sec, __ATOMIC_RELAXED);
        out->nsec = __atomic_load_n(&s->offset_nsec, __ATOMIC_RELAXED);
        out->mask = __atomic_load_n(&s->mask, __ATOMIC_RELAXED);
        __atomic_thread_fence(__ATOMIC_ACQUIRE);
        if (__atomic_load_n(&s->seq, __ATOMIC_RELAXED) == before)
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
