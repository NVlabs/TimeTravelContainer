/*
 * SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
 * SPDX-License-Identifier: Apache-2.0
 */

// The pthread functions that the GPU renderer calls, through Go's runtime,
// bound to their oldest versions in glibc, which every glibc since 2.17 has:
// glibc 2.32 and 2.34 gave them new versions as they moved libpthread into
// libc. And memfd_create, which glibc has only since 2.27, called as the
// system call. The renderer's calls come here (see TTC_LOCAL).

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

TTC_LOCAL int memfd_create(const char *name, unsigned int flags) {
    return (int)syscall(SYS_memfd_create, name, flags);
}

#endif // __GLIBC__
#endif // __linux__
