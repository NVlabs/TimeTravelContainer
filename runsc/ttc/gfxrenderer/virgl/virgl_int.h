// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// What the renderer's virgl host shares between its C files.

#ifndef TTC_VIRGL_INT_H
#define TTC_VIRGL_INT_H

#include <stdbool.h>
#include <stdint.h>

#include "compiler/shader_enums.h"
#include "util/format/u_formats.h"

struct pipe_screen;

extern struct pipe_screen *ttc_virgl_screen;

// The slots of the vertex shader's state that the renderer keeps for
// transform feedback, which it emulates if Zink has none (pipe_darwin.c):
// a uniform buffer of a draw's parameters, and four shader buffers, the
// targets'.
extern bool ttc_virgl_xfb_emulated;
extern uint32_t ttc_virgl_xfb_ubo, ttc_virgl_xfb_ssbo;

// ttc_virgl_pipe_format returns the Gallium format of a virgl format.
enum pipe_format ttc_virgl_pipe_format(uint32_t format);

// ttc_virgl_stage returns the Mesa stage of a stage of virgl's protocol,
// which numbers them as Gallium did: vertex, fragment, geometry, tessellation
// control and evaluation, compute.
static inline mesa_shader_stage ttc_virgl_stage(uint32_t stage) {
   static const mesa_shader_stage stages[] = {
      MESA_SHADER_VERTEX,    MESA_SHADER_FRAGMENT,  MESA_SHADER_GEOMETRY,
      MESA_SHADER_TESS_CTRL, MESA_SHADER_TESS_EVAL, MESA_SHADER_COMPUTE,
   };
   return stage < 6 ? stages[stage] : MESA_SHADER_NONE;
}

#endif
