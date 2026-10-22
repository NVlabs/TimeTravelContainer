/*
 * SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
 * SPDX-License-Identifier: Apache-2.0
 */

// The pthread functions that the GPU renderer calls, through Go's runtime and
// Mesa's Zink, bound to their oldest versions in glibc, which every glibc
// since 2.17 has: glibc 2.32 and 2.34 gave them new versions as they moved
// libpthread into libc. And memfd_create, which glibc has only since 2.27,
// called as the system call. The renderer's calls come here (see TTC_LOCAL).

#ifdef __linux__
#define _GNU_SOURCE
#include <features.h>
#ifdef __GLIBC__

#include <pthread.h>
#include <signal.h>
#include <stddef.h>
#include <sys/syscall.h>
#include <unistd.h>

#include "glibccompat.h"

TTC_GLIBC_OLDEST(pthread_create);
TTC_GLIBC_OLDEST(pthread_key_create);
TTC_GLIBC_OLDEST(pthread_setspecific);
TTC_GLIBC_OLDEST(pthread_attr_getstack);
TTC_GLIBC_OLDEST(pthread_attr_getstacksize);
TTC_GLIBC_OLDEST(pthread_getattr_np);
TTC_GLIBC_OLDEST(pthread_sigmask);
TTC_GLIBC_OLDEST(pthread_join);
TTC_GLIBC_OLDEST(pthread_detach);
TTC_GLIBC_OLDEST(pthread_once);
TTC_GLIBC_OLDEST(pthread_getspecific);
TTC_GLIBC_OLDEST(pthread_key_delete);
TTC_GLIBC_OLDEST(pthread_getcpuclockid);
TTC_GLIBC_SINCE(pthread_setname_np, "2.12");
TTC_GLIBC_OLDEST(pthread_mutex_trylock);
TTC_GLIBC_OLDEST(pthread_mutex_timedlock);
TTC_GLIBC_OLDEST(pthread_mutexattr_init);
TTC_GLIBC_OLDEST(pthread_mutexattr_settype);
TTC_GLIBC_OLDEST(pthread_mutexattr_destroy);
TTC_GLIBC_SINCE(pthread_condattr_setclock, "2.3.3");
TTC_GLIBC_OLDEST(pthread_rwlock_init);
TTC_GLIBC_OLDEST(pthread_rwlock_destroy);
TTC_GLIBC_OLDEST(pthread_rwlock_rdlock);
TTC_GLIBC_OLDEST(pthread_rwlock_wrlock);
TTC_GLIBC_OLDEST(pthread_rwlock_unlock);
TTC_GLIBC_OLDEST(pthread_barrier_init);
TTC_GLIBC_OLDEST(pthread_barrier_destroy);
TTC_GLIBC_OLDEST(pthread_barrier_wait);

extern int ttc_oldest_pthread_create(pthread_t *thread, const pthread_attr_t *attr,
                                     void *(*start)(void *), void *arg);
extern int ttc_oldest_pthread_key_create(pthread_key_t *key, void (*destructor)(void *));
// pthread.h has pthread_setspecific not read *value, as neither does this.
#ifndef __attr_access_none
#define __attr_access_none(argno)
#endif
extern int ttc_oldest_pthread_setspecific(pthread_key_t key, const void *value)
    __attr_access_none(2);
extern int ttc_oldest_pthread_attr_getstack(const pthread_attr_t *attr, void **addr,
                                            size_t *size);
extern int ttc_oldest_pthread_attr_getstacksize(const pthread_attr_t *attr, size_t *size);
extern int ttc_oldest_pthread_getattr_np(pthread_t thread, pthread_attr_t *attr);
extern int ttc_oldest_pthread_sigmask(int how, const sigset_t *set, sigset_t *old);
extern int ttc_oldest_pthread_join(pthread_t thread, void **ret);
extern int ttc_oldest_pthread_detach(pthread_t thread);
extern int ttc_oldest_pthread_once(pthread_once_t *once, void (*init)(void));
extern void *ttc_oldest_pthread_getspecific(pthread_key_t key);
extern int ttc_oldest_pthread_key_delete(pthread_key_t key);
extern int ttc_oldest_pthread_getcpuclockid(pthread_t thread, clockid_t *clock);
extern int ttc_oldest_pthread_setname_np(pthread_t thread, const char *name);
extern int ttc_oldest_pthread_mutex_trylock(pthread_mutex_t *mutex);
extern int ttc_oldest_pthread_mutex_timedlock(pthread_mutex_t *mutex,
                                              const struct timespec *abstime);
extern int ttc_oldest_pthread_mutexattr_init(pthread_mutexattr_t *attr);
extern int ttc_oldest_pthread_mutexattr_settype(pthread_mutexattr_t *attr, int type);
extern int ttc_oldest_pthread_mutexattr_destroy(pthread_mutexattr_t *attr);
extern int ttc_oldest_pthread_condattr_setclock(pthread_condattr_t *attr, clockid_t clock);
extern int ttc_oldest_pthread_rwlock_init(pthread_rwlock_t *lock, const pthread_rwlockattr_t *attr);
extern int ttc_oldest_pthread_rwlock_destroy(pthread_rwlock_t *lock);
extern int ttc_oldest_pthread_rwlock_rdlock(pthread_rwlock_t *lock);
extern int ttc_oldest_pthread_rwlock_wrlock(pthread_rwlock_t *lock);
extern int ttc_oldest_pthread_rwlock_unlock(pthread_rwlock_t *lock);
extern int ttc_oldest_pthread_barrier_init(pthread_barrier_t *barrier,
                                           const pthread_barrierattr_t *attr, unsigned count);
extern int ttc_oldest_pthread_barrier_destroy(pthread_barrier_t *barrier);
extern int ttc_oldest_pthread_barrier_wait(pthread_barrier_t *barrier);

TTC_LOCAL int pthread_create(pthread_t *thread, const pthread_attr_t *attr,
                             void *(*start)(void *), void *arg) {
    return ttc_oldest_pthread_create(thread, attr, start, arg);
}

TTC_LOCAL int pthread_key_create(pthread_key_t *key, void (*destructor)(void *)) {
    return ttc_oldest_pthread_key_create(key, destructor);
}

TTC_LOCAL int pthread_setspecific(pthread_key_t key, const void *value) {
    return ttc_oldest_pthread_setspecific(key, value);
}

TTC_LOCAL int pthread_attr_getstack(const pthread_attr_t *attr, void **addr, size_t *size) {
    return ttc_oldest_pthread_attr_getstack(attr, addr, size);
}

TTC_LOCAL int pthread_attr_getstacksize(const pthread_attr_t *attr, size_t *size) {
    return ttc_oldest_pthread_attr_getstacksize(attr, size);
}

TTC_LOCAL int pthread_getattr_np(pthread_t thread, pthread_attr_t *attr) {
    return ttc_oldest_pthread_getattr_np(thread, attr);
}

TTC_LOCAL int pthread_sigmask(int how, const sigset_t *set, sigset_t *old) {
    return ttc_oldest_pthread_sigmask(how, set, old);
}

TTC_LOCAL int pthread_join(pthread_t thread, void **ret) {
    return ttc_oldest_pthread_join(thread, ret);
}

TTC_LOCAL int pthread_detach(pthread_t thread) { return ttc_oldest_pthread_detach(thread); }

TTC_LOCAL int pthread_once(pthread_once_t *once, void (*init)(void)) {
    return ttc_oldest_pthread_once(once, init);
}

TTC_LOCAL void *pthread_getspecific(pthread_key_t key) {
    return ttc_oldest_pthread_getspecific(key);
}

TTC_LOCAL int pthread_key_delete(pthread_key_t key) { return ttc_oldest_pthread_key_delete(key); }

TTC_LOCAL int pthread_getcpuclockid(pthread_t thread, clockid_t *clock) {
    return ttc_oldest_pthread_getcpuclockid(thread, clock);
}

TTC_LOCAL int pthread_setname_np(pthread_t thread, const char *name) {
    return ttc_oldest_pthread_setname_np(thread, name);
}

TTC_LOCAL int pthread_mutex_trylock(pthread_mutex_t *mutex) {
    return ttc_oldest_pthread_mutex_trylock(mutex);
}

TTC_LOCAL int pthread_mutex_timedlock(pthread_mutex_t *mutex, const struct timespec *abstime) {
    return ttc_oldest_pthread_mutex_timedlock(mutex, abstime);
}

TTC_LOCAL int pthread_mutexattr_init(pthread_mutexattr_t *attr) {
    return ttc_oldest_pthread_mutexattr_init(attr);
}

TTC_LOCAL int pthread_mutexattr_settype(pthread_mutexattr_t *attr, int type) {
    return ttc_oldest_pthread_mutexattr_settype(attr, type);
}

TTC_LOCAL int pthread_mutexattr_destroy(pthread_mutexattr_t *attr) {
    return ttc_oldest_pthread_mutexattr_destroy(attr);
}

TTC_LOCAL int pthread_condattr_setclock(pthread_condattr_t *attr, clockid_t clock) {
    return ttc_oldest_pthread_condattr_setclock(attr, clock);
}

TTC_LOCAL int pthread_rwlock_init(pthread_rwlock_t *lock, const pthread_rwlockattr_t *attr) {
    return ttc_oldest_pthread_rwlock_init(lock, attr);
}

TTC_LOCAL int pthread_rwlock_destroy(pthread_rwlock_t *lock) {
    return ttc_oldest_pthread_rwlock_destroy(lock);
}

TTC_LOCAL int pthread_rwlock_rdlock(pthread_rwlock_t *lock) {
    return ttc_oldest_pthread_rwlock_rdlock(lock);
}

TTC_LOCAL int pthread_rwlock_wrlock(pthread_rwlock_t *lock) {
    return ttc_oldest_pthread_rwlock_wrlock(lock);
}

TTC_LOCAL int pthread_rwlock_unlock(pthread_rwlock_t *lock) {
    return ttc_oldest_pthread_rwlock_unlock(lock);
}

TTC_LOCAL int pthread_barrier_init(pthread_barrier_t *barrier, const pthread_barrierattr_t *attr,
                                   unsigned count) {
    return ttc_oldest_pthread_barrier_init(barrier, attr, count);
}

TTC_LOCAL int pthread_barrier_destroy(pthread_barrier_t *barrier) {
    return ttc_oldest_pthread_barrier_destroy(barrier);
}

TTC_LOCAL int pthread_barrier_wait(pthread_barrier_t *barrier) {
    return ttc_oldest_pthread_barrier_wait(barrier);
}

TTC_LOCAL int memfd_create(const char *name, unsigned int flags) {
    return (int)syscall(SYS_memfd_create, name, flags);
}

#endif // __GLIBC__
#endif // __linux__
