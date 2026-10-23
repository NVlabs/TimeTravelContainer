/*
 * SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
 * SPDX-License-Identifier: Apache-2.0
 */

#ifndef TTC_GLIBCCOMPAT_H
#define TTC_GLIBCCOMPAT_H

// The oldest version of glibc's symbols on the architecture: its first port.
#if defined(__x86_64__)
#define TTC_GLIBC_OLDEST_VERSION "GLIBC_2.2.5"
#elif defined(__aarch64__)
#define TTC_GLIBC_OLDEST_VERSION "GLIBC_2.17"
#else
#error "glibccompat knows no oldest glibc version for this architecture"
#endif

// TTC_GLIBC_OLDEST(name) has ttc_oldest_name stand for name's oldest
// version.
#define TTC_GLIBC_OLDEST(name) \
    __asm__(".symver ttc_oldest_" #name ", " #name "@" TTC_GLIBC_OLDEST_VERSION)

// TTC_LOCAL defines a function of the C library anew in the binary, for all
// of the binary's calls, its startup code's among them, which the linker binds
// to it rather than to the library's; the binary does not export it, and the
// libraries that it loads call the library's.
#define TTC_LOCAL __attribute__((visibility("hidden")))

#endif // TTC_GLIBCCOMPAT_H
