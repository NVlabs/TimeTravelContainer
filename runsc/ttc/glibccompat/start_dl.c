/*
 * SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
 * SPDX-License-Identifier: Apache-2.0
 */

// The C start function and the dl functions of a helper that ttcd carries,
// bound to their oldest versions in glibc, which every glibc since 2.17 has:
// glibc 2.34 gave them new versions as it moved libdl into libc, and a
// binary that it links needs it. The helper's calls come here (see TTC_LOCAL).

#ifdef __linux__
#define _GNU_SOURCE
#include <features.h>
#ifdef __GLIBC__

#include <stddef.h>

#include "glibccompat.h"

TTC_GLIBC_OLDEST(__libc_start_main);
TTC_GLIBC_OLDEST(dlopen);
TTC_GLIBC_OLDEST(dlsym);
TTC_GLIBC_OLDEST(dlerror);
TTC_GLIBC_OLDEST(dlclose);

extern int ttc_oldest___libc_start_main(int (*main)(int, char **, char **), int argc, char **argv,
                                        void (*init)(int, char **, char **), void (*fini)(void),
                                        void (*rtld_fini)(void), void *stack_end);
extern void *ttc_oldest_dlopen(const char *file, int mode);
extern void *ttc_oldest_dlsym(void *handle, const char *name);
extern char *ttc_oldest_dlerror(void);
extern int ttc_oldest_dlclose(void *handle);

// The executable's constructors. glibc before 2.34 runs those that the
// startup code gives it, and so does 2.34's oldest __libc_start_main: the
// startup code that 2.34 links gives it none, for the newest to find them.
extern void _init(void) __attribute__((weak));
extern void (*__init_array_start[])(int, char **, char **) __attribute__((weak, visibility("hidden")));
extern void (*__init_array_end[])(int, char **, char **) __attribute__((weak, visibility("hidden")));

static void ttc_run_constructors(int argc, char **argv, char **envp) {
    if (_init)
        _init();
    size_t n = __init_array_end - __init_array_start;
    for (size_t i = 0; i < n; i++)
        __init_array_start[i](argc, argv, envp);
}

TTC_LOCAL int __libc_start_main(int (*main)(int, char **, char **), int argc, char **argv,
                                void (*init)(int, char **, char **), void (*fini)(void),
                                void (*rtld_fini)(void), void *stack_end) {
    (void)init;
    (void)fini;
    // The dynamic linker runs the executable's destructors.
    return ttc_oldest___libc_start_main(main, argc, argv, ttc_run_constructors, NULL, rtld_fini,
                                        stack_end);
}

TTC_LOCAL void *dlopen(const char *file, int mode) { return ttc_oldest_dlopen(file, mode); }

TTC_LOCAL void *dlsym(void *handle, const char *name) { return ttc_oldest_dlsym(handle, name); }

TTC_LOCAL char *dlerror(void) { return ttc_oldest_dlerror(); }

TTC_LOCAL int dlclose(void *handle) { return ttc_oldest_dlclose(handle); }

#endif // __GLIBC__
#endif // __linux__
