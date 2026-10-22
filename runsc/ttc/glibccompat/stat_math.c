/*
 * SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
 * SPDX-License-Identifier: Apache-2.0
 */

// The file status and math functions that Mesa's Zink, which the GPU renderer
// links, calls, which glibc gave new versions: stat and fstat, which glibc
// 2.33 made functions of their own, called as the system calls, which every
// Linux has, of glibc's struct stat, which is the kernel's on x86-64 and
// arm64; and the math functions that glibc 2.27 and 2.29 rewrote, bound to
// their oldest versions, whose results are the same but where they set
// errno. The renderer's calls come here (see TTC_LOCAL).

#ifdef __linux__
#define _GNU_SOURCE
#include <features.h>
#ifdef __GLIBC__

#include <fcntl.h>
#include <sys/stat.h>
#include <sys/syscall.h>
#include <unistd.h>

#include "glibccompat.h"

TTC_LOCAL int stat(const char *path, struct stat *buf) {
    return (int)syscall(SYS_newfstatat, AT_FDCWD, path, buf, 0);
}

TTC_LOCAL int fstat(int fd, struct stat *buf) { return (int)syscall(SYS_fstat, fd, buf); }

TTC_GLIBC_OLDEST(exp2f);
TTC_GLIBC_OLDEST(log2f);
TTC_GLIBC_OLDEST(logf);
TTC_GLIBC_OLDEST(powf);
TTC_GLIBC_OLDEST(log2);
TTC_GLIBC_OLDEST(pow);

extern float ttc_oldest_exp2f(float x);
extern float ttc_oldest_log2f(float x);
extern float ttc_oldest_logf(float x);
extern float ttc_oldest_powf(float x, float y);
extern double ttc_oldest_log2(double x);
extern double ttc_oldest_pow(double x, double y);

TTC_LOCAL float exp2f(float x) { return ttc_oldest_exp2f(x); }

TTC_LOCAL float log2f(float x) { return ttc_oldest_log2f(x); }

TTC_LOCAL float logf(float x) { return ttc_oldest_logf(x); }

TTC_LOCAL float powf(float x, float y) { return ttc_oldest_powf(x, y); }

TTC_LOCAL double log2(double x) { return ttc_oldest_log2(x); }

TTC_LOCAL double pow(double x, double y) { return ttc_oldest_pow(x, y); }

#endif // __GLIBC__
#endif // __linux__
