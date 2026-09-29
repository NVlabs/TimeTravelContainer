// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build darwin && arm64

// Package machexc provides process-wide Mach exception handling for safecopy.
package machexc

/*
#cgo CFLAGS: -mmacosx-version-min=15.0
#cgo LDFLAGS: -mmacosx-version-min=15.0

#include <mach/arm/thread_status.h>
#include <mach/exc.h>
#include <mach/mach.h>
#include <pthread.h>
#include <signal.h>
#include <stdint.h>
#include <stdlib.h>
#include <string.h>

#define MAX_FAULT_RANGES 16

typedef struct {
    uint64_t begin;
    uint64_t end;
    uint64_t handler;
} fault_range_t;

static fault_range_t fault_ranges[MAX_FAULT_RANGES];
static size_t fault_range_count;
static mach_port_t exception_port = MACH_PORT_NULL;

// exc_server is provided by libsystem_kernel. The SDK's generated exc.h only
// declares the client stubs, so declare the server demultiplexer here.
extern boolean_t exc_server(mach_msg_header_t *, mach_msg_header_t *);

static kern_return_t add_fault_range(uint64_t begin, uint64_t end, uint64_t handler) {
    if (begin >= end || handler == 0) {
        return KERN_INVALID_ARGUMENT;
    }
    if (fault_range_count == MAX_FAULT_RANGES) {
        return KERN_RESOURCE_SHORTAGE;
    }
    fault_ranges[fault_range_count++] = (fault_range_t){begin, end, handler};
    return KERN_SUCCESS;
}

static uint64_t find_fault_handler(uint64_t pc) {
    for (size_t i = 0; i < fault_range_count; i++) {
        if (pc >= fault_ranges[i].begin && pc < fault_ranges[i].end) {
            return fault_ranges[i].handler;
        }
    }
    return 0;
}

kern_return_t catch_exception_raise(
    mach_port_t exception_port_arg,
    mach_port_t thread,
    mach_port_t task,
    exception_type_t exception,
    exception_data_t code,
    mach_msg_type_number_t code_count) {
    (void)exception_port_arg;
    (void)thread;
    (void)task;
    (void)exception;
    (void)code;
    (void)code_count;
    return KERN_FAILURE;
}

kern_return_t catch_exception_raise_state(
    mach_port_t exception_port_arg,
    exception_type_t exception,
    const exception_data_t code,
    mach_msg_type_number_t code_count,
    int *flavor,
    const thread_state_t old_state,
    mach_msg_type_number_t old_state_count,
    thread_state_t new_state,
    mach_msg_type_number_t *new_state_count) {
    (void)exception_port_arg;
    (void)exception;
    (void)code;
    (void)code_count;
    (void)flavor;
    (void)old_state;
    (void)old_state_count;
    (void)new_state;
    (void)new_state_count;
    return KERN_FAILURE;
}

kern_return_t catch_exception_raise_state_identity(
    mach_port_t exception_port_arg,
    mach_port_t thread,
    mach_port_t task,
    exception_type_t exception,
    exception_data_t code,
    mach_msg_type_number_t code_count,
    int *flavor,
    thread_state_t old_state,
    mach_msg_type_number_t old_state_count,
    thread_state_t new_state,
    mach_msg_type_number_t *new_state_count) {
    (void)exception_port_arg;
    (void)task;
    if (exception != EXC_BAD_ACCESS || code_count < 1 ||
        *flavor != ARM_THREAD_STATE64 ||
        old_state_count < ARM_THREAD_STATE64_COUNT ||
        *new_state_count < ARM_THREAD_STATE64_COUNT) {
        return KERN_FAILURE;
    }

    const arm_thread_state64_t *old_ts = (const arm_thread_state64_t *)old_state;
    arm_thread_state64_t *new_ts = (arm_thread_state64_t *)new_state;
    uint64_t handler = find_fault_handler(arm_thread_state64_get_pc(*old_ts));
    if (handler == 0) {
        return KERN_FAILURE;
    }

    // The legacy exc protocol carries 32-bit exception_data_t values, which
    // cannot represent an ARM64 fault address. Read FAR from the faulting
    // thread's architectural exception state instead.
    arm_exception_state64_t exception_state;
    mach_msg_type_number_t exception_state_count = ARM_EXCEPTION_STATE64_COUNT;
    kern_return_t kr = thread_get_state(
        thread,
        ARM_EXCEPTION_STATE64,
        (thread_state_t)&exception_state,
        &exception_state_count);
    if (kr != KERN_SUCCESS || exception_state_count < ARM_EXCEPTION_STATE64_COUNT) {
        return KERN_FAILURE;
    }

    memcpy(new_ts, old_ts, sizeof(*new_ts));
    arm_thread_state64_set_pc_fptr(*new_ts, (void *)handler);
    new_ts->__x[0] = exception_state.__far;
    new_ts->__x[1] = code[0] == KERN_MEMORY_ERROR ? SIGBUS : SIGSEGV;
    *new_state_count = ARM_THREAD_STATE64_COUNT;
    // A MIG server routine that succeeds owns the send rights that the request
    // carried; mach_msg_server destroys them only when the routine fails.
    // Leaking them would add a reference per fault, and leave a dead name
    // behind for every faulting thread that exits.
    (void)mach_port_deallocate(mach_task_self(), thread);
    (void)mach_port_deallocate(mach_task_self(), task);
    return KERN_SUCCESS;
}

static kern_return_t task_port_refs(mach_port_urefs_t *refs) {
    return mach_port_get_refs(mach_task_self(), mach_task_self(), MACH_PORT_RIGHT_SEND, refs);
}

static void *exception_server_main(void *unused) {
    (void)unused;
    // mach_msg_server uses libsystem's generated exc_server demultiplexer,
    // which validates message descriptors and constructs the correct reply.
    (void)mach_msg_server(exc_server, 4096, exception_port, 0);
    return NULL;
}

static kern_return_t install_exception_handler(void) {
    if (exception_port != MACH_PORT_NULL) {
        return KERN_SUCCESS;
    }
    kern_return_t kr = mach_port_allocate(mach_task_self(), MACH_PORT_RIGHT_RECEIVE, &exception_port);
    if (kr != KERN_SUCCESS) {
        return kr;
    }
    kr = mach_port_insert_right(mach_task_self(), exception_port, exception_port, MACH_MSG_TYPE_MAKE_SEND);
    if (kr != KERN_SUCCESS) {
        (void)mach_port_destruct(mach_task_self(), exception_port, 0, 0);
        exception_port = MACH_PORT_NULL;
        return kr;
    }
    kr = task_set_exception_ports(
        mach_task_self(),
        EXC_MASK_BAD_ACCESS,
        exception_port,
        EXCEPTION_STATE_IDENTITY,
        ARM_THREAD_STATE64);
    if (kr != KERN_SUCCESS) {
        // Drop the send right made above along with the receive right.
        (void)mach_port_destruct(mach_task_self(), exception_port, -1, 0);
        exception_port = MACH_PORT_NULL;
        return kr;
    }

    pthread_attr_t attributes;
    if (pthread_attr_init(&attributes) != 0) {
        return KERN_FAILURE;
    }
    (void)pthread_attr_setdetachstate(&attributes, PTHREAD_CREATE_DETACHED);
    pthread_t thread;
    int pthread_error = pthread_create(&thread, &attributes, exception_server_main, NULL);
    pthread_attr_destroy(&attributes);
    if (pthread_error != 0) {
        return KERN_FAILURE;
    }
    return KERN_SUCCESS;
}
*/
import "C"

import "fmt"

// AddFaultRange registers [begin, end) as recoverable by handler.
func AddFaultRange(begin, end, handler uintptr) error {
	if result := C.add_fault_range(C.uint64_t(begin), C.uint64_t(end), C.uint64_t(handler)); result != C.KERN_SUCCESS {
		return fmt.Errorf("add_fault_range returned Mach error %d", int(result))
	}
	return nil
}

// TaskPortRefs returns the user references of this task's send right to its
// own task port. Every Mach exception message carries another one, so tests
// use it to check that handling exceptions releases them.
func TaskPortRefs() (uint32, error) {
	var refs C.mach_port_urefs_t
	if result := C.task_port_refs(&refs); result != C.KERN_SUCCESS {
		return 0, fmt.Errorf("mach_port_get_refs returned Mach error %d", int(result))
	}
	return uint32(refs), nil
}

// Install installs the task exception port and starts its C-only server.
func Install() error {
	if result := C.install_exception_handler(); result != C.KERN_SUCCESS {
		return fmt.Errorf("install_exception_handler returned Mach error %d", int(result))
	}
	return nil
}
