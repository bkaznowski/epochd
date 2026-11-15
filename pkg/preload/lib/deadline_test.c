/* pkg/preload/lib/deadline_test.c
 *
 * Run under the preload library with the clock faked (see verify_deadlines.sh):
 * every primitive below is given an absolute deadline 300 ms ahead on the FAKE
 * clock and must really time out after about 300 ms. Without translation the
 * kernel sees a deadline a day away (or a day ago) and the call would block for
 * a day or return at once. Real elapsed time is measured with CLOCK_MONOTONIC,
 * which the library does not touch.
 *
 * Usage: deadline_test        (all results on stdout; exit status 0 = all passed)
 */
#define _GNU_SOURCE
#include <errno.h>
#include <poll.h>
#include <pthread.h>
#include <semaphore.h>
#include <signal.h>
#include <stdio.h>
#include <string.h>
#include <sys/timerfd.h>
#include <time.h>
#include <unistd.h>

static int failures;

static double mono(void)
{
    struct timespec t;
    clock_gettime(CLOCK_MONOTONIC, &t);
    return (double)t.tv_sec + (double)t.tv_nsec / 1e9;
}

/* now + 300ms on the given clock */
static struct timespec deadline(clockid_t clk)
{
    struct timespec t;
    clock_gettime(clk, &t);
    t.tv_nsec += 300000000L;
    if (t.tv_nsec >= 1000000000L) {
        t.tv_nsec -= 1000000000L;
        t.tv_sec++;
    }
    return t;
}

static void check(const char *name, double began, int ok, const char *detail)
{
    double took = mono() - began;
    int timely = took >= 0.20 && took <= 2.0;
    printf("%-34s %s  (%.2fs real%s%s)\n", name, ok && timely ? "ok  " : "FAIL", took,
           *detail ? ", " : "", detail);
    if (!(ok && timely))
        failures++;
}

static pthread_mutex_t held_mutex = PTHREAD_MUTEX_INITIALIZER;
static pthread_rwlock_t held_rw = PTHREAD_RWLOCK_INITIALIZER;
static volatile int holder_ready;

/* Holds both locks for longer than any test waits. */
static void *holder(void *arg)
{
    (void)arg;
    pthread_mutex_lock(&held_mutex);
    pthread_rwlock_wrlock(&held_rw);
    holder_ready = 1;
    sleep(6);
    pthread_rwlock_unlock(&held_rw);
    pthread_mutex_unlock(&held_mutex);
    return NULL;
}

int main(void)
{
    setvbuf(stdout, NULL, _IOLBF, 0);
    struct timespec fake;
    clock_gettime(CLOCK_REALTIME, &fake);
    printf("fake clock reads %ld\n", (long)fake.tv_sec);

    pthread_t th;
    pthread_create(&th, NULL, holder, NULL);
    while (!holder_ready)
        usleep(1000);

    double t0;
    struct timespec dl;

    sem_t sem;
    sem_init(&sem, 0, 0);
    dl = deadline(CLOCK_REALTIME);
    t0 = mono();
    int rc = sem_timedwait(&sem, &dl);
    check("sem_timedwait", t0, rc == -1 && errno == ETIMEDOUT, "");

    dl = deadline(CLOCK_REALTIME);
    t0 = mono();
    rc = pthread_mutex_timedlock(&held_mutex, &dl);
    check("pthread_mutex_timedlock", t0, rc == ETIMEDOUT, "");

    dl = deadline(CLOCK_REALTIME);
    t0 = mono();
    rc = pthread_rwlock_timedwrlock(&held_rw, &dl);
    check("pthread_rwlock_timedwrlock", t0, rc == ETIMEDOUT, "");

    dl = deadline(CLOCK_REALTIME);
    t0 = mono();
    rc = pthread_rwlock_timedrdlock(&held_rw, &dl);
    check("pthread_rwlock_timedrdlock", t0, rc == ETIMEDOUT, "");

    pthread_mutex_t m = PTHREAD_MUTEX_INITIALIZER;
    pthread_cond_t c = PTHREAD_COND_INITIALIZER;
    pthread_mutex_lock(&m);
    dl = deadline(CLOCK_REALTIME);
    t0 = mono();
    rc = pthread_cond_timedwait(&c, &m, &dl);
    check("pthread_cond_timedwait (realtime)", t0, rc == ETIMEDOUT, "");

    /* A condition variable on the monotonic clock takes a monotonic deadline,
     * which must not be translated as if it were a wall-clock time. */
    pthread_condattr_t ca;
    pthread_cond_t cm;
    pthread_condattr_init(&ca);
    pthread_condattr_setclock(&ca, CLOCK_MONOTONIC);
    pthread_cond_init(&cm, &ca);
    dl = deadline(CLOCK_MONOTONIC);
    t0 = mono();
    rc = pthread_cond_timedwait(&cm, &m, &dl);
    char why[64];
    snprintf(why, sizeof why, "left alone, rc=%d, deadline %ld, now %ld", rc, (long)dl.tv_sec, (long)mono());
    check("pthread_cond_timedwait (monotonic)", t0, rc == ETIMEDOUT, why);
    pthread_mutex_unlock(&m);

    dl = deadline(CLOCK_REALTIME);
    t0 = mono();
    rc = clock_nanosleep(CLOCK_REALTIME, TIMER_ABSTIME, &dl, NULL);
    check("clock_nanosleep(ABSTIME)", t0, rc == 0, "");

    /* and a monotonic absolute sleep is not touched */
    dl = deadline(CLOCK_MONOTONIC);
    t0 = mono();
    rc = clock_nanosleep(CLOCK_MONOTONIC, TIMER_ABSTIME, &dl, NULL);
    check("clock_nanosleep(MONOTONIC)", t0, rc == 0, "left alone");

    int tfd = timerfd_create(CLOCK_REALTIME, 0);
    struct itimerspec its;
    memset(&its, 0, sizeof its);
    its.it_value = deadline(CLOCK_REALTIME);
    t0 = mono();
    timerfd_settime(tfd, TFD_TIMER_ABSTIME, &its, NULL);
    struct pollfd p = {.fd = tfd, .events = POLLIN};
    rc = poll(&p, 1, 2000);
    check("timerfd_settime(ABSTIME)", t0, rc == 1, "");

    /* Disarming (zero value) with ABSTIME must stay a disarm. */
    memset(&its, 0, sizeof its);
    timerfd_settime(tfd, TFD_TIMER_ABSTIME, &its, NULL);
    struct itimerspec cur;
    timerfd_gettime(tfd, &cur);
    t0 = mono() - 0.3; /* not a timing test */
    check("timerfd disarm stays disarmed", t0, cur.it_value.tv_sec == 0 && cur.it_value.tv_nsec == 0, "");

    timer_t tm;
    struct sigevent sev;
    memset(&sev, 0, sizeof sev);
    sev.sigev_notify = SIGEV_NONE; /* the default would raise SIGALRM when it fires */
    timer_create(CLOCK_REALTIME, &sev, &tm);
    memset(&its, 0, sizeof its);
    its.it_value = deadline(CLOCK_REALTIME);
    timer_settime(tm, TIMER_ABSTIME, &its, NULL);
    timer_gettime(tm, &cur);
    /* remaining time, not a wait: must be a fraction of a second, not a day */
    t0 = mono() - 0.3;
    check("timer_settime(ABSTIME) remaining", t0, cur.it_value.tv_sec == 0 && cur.it_value.tv_nsec > 0, "");

    pthread_join(th, NULL);

    printf(failures ? "FAILED: %d\n" : "all passed\n", failures);
    return failures != 0;
}
