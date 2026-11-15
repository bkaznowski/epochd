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
 * Absolute deadlines. A program that computes "now + 5s" from the fake clock
 * and hands that timestamp to the kernel (pthread_cond_timedwait, sem_timedwait,
 * clock_nanosleep(TIMER_ABSTIME), timerfd, ...) would wait against the REAL
 * clock: about a day too long for a +1 day offset, not at all for a negative
 * one. The functions below translate such deadlines back to real time before the
 * call. In advancing mode that is deadline - offset. In frozen mode the clock
 * never reaches the deadline, so the wait is made as long in real time as it
 * was asked to be in fake time. Translation happens when the call is made: an
 * offset changed while a process is already blocked does not affect that wait.
 * Relative timeouts and the monotonic clocks need none and are left alone.
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
#include <errno.h>
#include <fcntl.h>
#include <pthread.h>
#include <sched.h>
#include <semaphore.h>
#include <signal.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <sys/mman.h>
#include <sys/select.h> /* struct timeval; deliberately not <sys/time.h> */
#include <sys/timerfd.h>
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

/* ---- absolute deadlines ------------------------------------------------------
 *
 * None of this may reference libpthread or librt directly: the library is linked
 * with -z now, so an undefined symbol that only a threaded program's libraries
 * provide would stop it loading into any other program. Everything is reached
 * through dlsym(RTLD_NEXT), and the tables below are guarded by a spin flag
 * rather than a mutex. */

#define NEXT(fp, name)                                          \
    do {                                                        \
        if (!(fp))                                              \
            *(void **)&(fp) = dlsym(RTLD_NEXT, name);           \
    } while (0)

static int is_wall_clock(clockid_t clk)
{
    return clk == CLOCK_REALTIME || clk == CLOCK_REALTIME_ALARM;
}

/* Translate an absolute CLOCK_REALTIME deadline given on the fake clock into the
 * real clock's terms. Returns in itself (untranslated) when there is nothing to
 * do or the value is not one the libc would accept anyway, so that it reports
 * its own error. */
static const struct timespec *xl(const struct timespec *in, struct timespec *buf)
{
    if (!in || in->tv_nsec < 0 || in->tv_nsec >= 1000000000L)
        return in;
    Snapshot s;
    if (!read_state(&s))
        return in;
    resolve();

    int64_t sec = in->tv_sec, nsec = in->tv_nsec;
    if (s.mask & MASK_FROZEN) {
        /* The fake clock never gets there; wait as long as was asked, really. */
        struct timespec now;
        if (real_clock_gettime(CLOCK_REALTIME, &now) != 0)
            return in;
        sec  += now.tv_sec - s.sec;
        nsec += now.tv_nsec - s.nsec;
    } else {
        sec  -= s.sec;
        nsec -= s.nsec;
    }
    normalise(&sec, &nsec, 1000000000);
    buf->tv_sec  = sec;
    buf->tv_nsec = nsec;
    return buf;
}

/* For pthread_cond_timedwait, whose clock is chosen when the condition variable
 * is created and is not visible from here. A monotonic deadline is a number of
 * seconds of uptime, which cannot be as large as a wall-clock timestamp (this is
 * 9.5 years of uptime), so a deadline below it is left alone. */
static const struct timespec *xl_guess(const struct timespec *in, struct timespec *buf)
{
    if (in && in->tv_sec < 300000000)
        return in;
    return xl(in, buf);
}

int sem_timedwait(sem_t *sem, const struct timespec *abs)
{
    static int (*real)(sem_t *, const struct timespec *);
    NEXT(real, "sem_timedwait");
    struct timespec b;
    if (!real) { errno = ENOSYS; return -1; }
    return real(sem, xl(abs, &b));
}

int sem_clockwait(sem_t *sem, clockid_t clk, const struct timespec *abs)
{
    static int (*real)(sem_t *, clockid_t, const struct timespec *);
    NEXT(real, "sem_clockwait");
    struct timespec b;
    if (!real) { errno = ENOSYS; return -1; }
    return real(sem, clk, is_wall_clock(clk) ? xl(abs, &b) : abs);
}

int pthread_mutex_timedlock(pthread_mutex_t *m, const struct timespec *abs)
{
    static int (*real)(pthread_mutex_t *, const struct timespec *);
    NEXT(real, "pthread_mutex_timedlock");
    struct timespec b;
    if (!real) return ENOSYS;
    return real(m, xl(abs, &b));
}

int pthread_mutex_clocklock(pthread_mutex_t *m, clockid_t clk, const struct timespec *abs)
{
    static int (*real)(pthread_mutex_t *, clockid_t, const struct timespec *);
    NEXT(real, "pthread_mutex_clocklock");
    struct timespec b;
    if (!real) return ENOSYS;
    return real(m, clk, is_wall_clock(clk) ? xl(abs, &b) : abs);
}

int pthread_rwlock_timedrdlock(pthread_rwlock_t *l, const struct timespec *abs)
{
    static int (*real)(pthread_rwlock_t *, const struct timespec *);
    NEXT(real, "pthread_rwlock_timedrdlock");
    struct timespec b;
    if (!real) return ENOSYS;
    return real(l, xl(abs, &b));
}

int pthread_rwlock_timedwrlock(pthread_rwlock_t *l, const struct timespec *abs)
{
    static int (*real)(pthread_rwlock_t *, const struct timespec *);
    NEXT(real, "pthread_rwlock_timedwrlock");
    struct timespec b;
    if (!real) return ENOSYS;
    return real(l, xl(abs, &b));
}

int pthread_rwlock_clockrdlock(pthread_rwlock_t *l, clockid_t clk, const struct timespec *abs)
{
    static int (*real)(pthread_rwlock_t *, clockid_t, const struct timespec *);
    NEXT(real, "pthread_rwlock_clockrdlock");
    struct timespec b;
    if (!real) return ENOSYS;
    return real(l, clk, is_wall_clock(clk) ? xl(abs, &b) : abs);
}

int pthread_rwlock_clockwrlock(pthread_rwlock_t *l, clockid_t clk, const struct timespec *abs)
{
    static int (*real)(pthread_rwlock_t *, clockid_t, const struct timespec *);
    NEXT(real, "pthread_rwlock_clockwrlock");
    struct timespec b;
    if (!real) return ENOSYS;
    return real(l, clk, is_wall_clock(clk) ? xl(abs, &b) : abs);
}

/* dlsym(RTLD_NEXT, name) is NOT always the version a program is linked to: glibc
 * on x86-64 has two pthread_cond_timedwait, and an unversioned lookup returns the
 * old GLIBC_2.2.5 one, which ignores the clock a condition variable was created
 * with (a monotonic one then times out at once). Ask for the current version
 * explicitly. dlvsym is fetched at run time rather than linked, so the library
 * still loads where it does not exist; musl has one version and ignores the
 * argument, and other architectures simply do not have this version, in which
 * case the plain lookup is the right one. */
static void *next_versioned(const char *name, const char *version)
{
    static void *(*vsym)(void *, const char *, const char *);
    if (!vsym)
        *(void **)&vsym = dlsym(RTLD_DEFAULT, "dlvsym");
    void *p = vsym ? vsym(RTLD_NEXT, name, version) : NULL;
    return p ? p : dlsym(RTLD_NEXT, name);
}

int pthread_cond_timedwait(pthread_cond_t *c, pthread_mutex_t *m, const struct timespec *abs)
{
    static int (*real)(pthread_cond_t *, pthread_mutex_t *, const struct timespec *);
    if (!real)
        *(void **)&real = next_versioned("pthread_cond_timedwait", "GLIBC_2.3.2");
    struct timespec b;
    if (!real) return ENOSYS;
    return real(c, m, xl_guess(abs, &b));
}

int pthread_cond_clockwait(pthread_cond_t *c, pthread_mutex_t *m, clockid_t clk, const struct timespec *abs)
{
    static int (*real)(pthread_cond_t *, pthread_mutex_t *, clockid_t, const struct timespec *);
    NEXT(real, "pthread_cond_clockwait");
    struct timespec b;
    if (!real) return ENOSYS;
    return real(c, m, clk, is_wall_clock(clk) ? xl(abs, &b) : abs);
}

int pthread_timedjoin_np(pthread_t t, void **ret, const struct timespec *abs)
{
    static int (*real)(pthread_t, void **, const struct timespec *);
    NEXT(real, "pthread_timedjoin_np");
    struct timespec b;
    if (!real) return ENOSYS;
    return real(t, ret, xl(abs, &b));
}

int clock_nanosleep(clockid_t clk, int flags, const struct timespec *req, struct timespec *rem)
{
    static int (*real)(clockid_t, int, const struct timespec *, struct timespec *);
    NEXT(real, "clock_nanosleep");
    struct timespec b;
    if (!real) return ENOSYS;
    if ((flags & TIMER_ABSTIME) && is_wall_clock(clk))
        req = xl(req, &b);
    return real(clk, flags, req, rem);
}

/* A timer's clock is chosen at creation and is not asked for again at arming
 * time, so it is remembered. A timerfd that was dup()ed, or created some way
 * that does not pass through here, is not recognised and keeps real deadlines. */
#define MAX_TRACKED_FDS 4096
#define MAX_TRACKED_TIMERS 64

static unsigned char fd_is_wall[MAX_TRACKED_FDS];

static struct { timer_t id; int wall; } timers[MAX_TRACKED_TIMERS];
static unsigned timers_next;
static char timers_busy;

static void timers_lock(void)
{
    while (__atomic_test_and_set(&timers_busy, __ATOMIC_ACQUIRE))
        sched_yield();
}
static void timers_unlock(void) { __atomic_clear(&timers_busy, __ATOMIC_RELEASE); }

/* The new value of an absolute-time timer, with its first expiry translated. A
 * zero first expiry disarms the timer and is not a time at all. */
static const struct itimerspec *xl_itimer(const struct itimerspec *in, struct itimerspec *buf)
{
    if (!in || (in->it_value.tv_sec == 0 && in->it_value.tv_nsec == 0))
        return in;
    struct timespec t;
    *buf = *in; /* it_interval is a period, not a point in time */
    buf->it_value = *xl(&in->it_value, &t);
    return buf;
}

int timerfd_create(int clk, int flags)
{
    static int (*real)(int, int);
    NEXT(real, "timerfd_create");
    if (!real) { errno = ENOSYS; return -1; }
    int fd = real(clk, flags);
    if (fd >= 0 && fd < MAX_TRACKED_FDS)
        fd_is_wall[fd] = is_wall_clock((clockid_t)clk);
    return fd;
}

int timerfd_settime(int fd, int flags, const struct itimerspec *nw, struct itimerspec *old)
{
    static int (*real)(int, int, const struct itimerspec *, struct itimerspec *);
    NEXT(real, "timerfd_settime");
    struct itimerspec b;
    if (!real) { errno = ENOSYS; return -1; }
    if ((flags & TFD_TIMER_ABSTIME) && fd >= 0 && fd < MAX_TRACKED_FDS && fd_is_wall[fd])
        nw = xl_itimer(nw, &b);
    return real(fd, flags, nw, old);
}

int timer_create(clockid_t clk, struct sigevent *sev, timer_t *id)
{
    static int (*real)(clockid_t, struct sigevent *, timer_t *);
    NEXT(real, "timer_create");
    if (!real) { errno = ENOSYS; return -1; }
    int rc = real(clk, sev, id);
    if (rc == 0 && id) {
        timers_lock();
        timers[timers_next % MAX_TRACKED_TIMERS].id   = *id;
        timers[timers_next % MAX_TRACKED_TIMERS].wall = is_wall_clock(clk);
        timers_next++;
        timers_unlock();
    }
    return rc;
}

int timer_settime(timer_t id, int flags, const struct itimerspec *nw, struct itimerspec *old)
{
    static int (*real)(timer_t, int, const struct itimerspec *, struct itimerspec *);
    NEXT(real, "timer_settime");
    struct itimerspec b;
    if (!real) { errno = ENOSYS; return -1; }
    if (flags & TIMER_ABSTIME) {
        int wall = 0;
        timers_lock();
        /* newest first: a timer id can be reused after timer_delete */
        for (unsigned n = 0; n < MAX_TRACKED_TIMERS && n < timers_next; n++) {
            unsigned i = (timers_next - 1 - n) % MAX_TRACKED_TIMERS;
            if (timers[i].id == id) {
                wall = timers[i].wall;
                break;
            }
        }
        timers_unlock();
        if (wall)
            nw = xl_itimer(nw, &b);
    }
    return real(id, flags, nw, old);
}

__attribute__((constructor)) static void epochd_preload_init(void)
{
    init_state();
}
