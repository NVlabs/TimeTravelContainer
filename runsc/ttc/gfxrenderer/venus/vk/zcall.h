/*
 * SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
 * SPDX-License-Identifier: Apache-2.0
 */

#ifndef TTC_VENUS_ZCALL_H
#define TTC_VENUS_ZCALL_H

#include <stdbool.h>
#include <stdint.h>

#include "vn_protocol_renderer_util.h"

/* vn_call calls the Vulkan command of type type, a VkCommandTypeEXT, with
 * args, its struct vn_command_*, through table, the procedure table of the
 * command's level: struct vn_global_proc_table, vn_instance_proc_table,
 * vn_physical_device_proc_table, or vn_device_proc_table. A command that
 * returns stores its result in args->ret. It returns false, calling
 * nothing, if the table lacks the command, as a device's does the commands
 * of the extensions that it did not enable. */
bool vn_call(int32_t type, const void *table, void *args);

#endif /* TTC_VENUS_ZCALL_H */
