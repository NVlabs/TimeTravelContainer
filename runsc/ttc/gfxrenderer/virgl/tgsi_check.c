// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build (darwin || linux) && cgo

// The TGSI of guests that Zink takes. Zink translates TGSI with Mesa's
// tgsi_to_nir, which was written for Gallium's own shaders: of others, it
// aborts, reads and writes past its arrays at guests' indexes, as
// tgsi_scan_shader, with which it scans them, does, or leaves NIR that
// Zink and Vulkan's drivers trap on; Mesa builds without its asserts.
// ttc_virgl_check_tgsi refuses the shaders that it does not translate as
// TGSI means them, as virglrenderer's translator refuses what it does
// not: of other stages than vertex, fragment and compute shaders, and of
// declarations, operands and instructions that it translates to nothing,
// or to another meaning.

#include <stdarg.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>

#include "pipe/p_defines.h"
#include "pipe/p_screen.h"
#include "pipe/p_shader_tokens.h"
#include "pipe/p_state.h"
#include "tgsi/tgsi_info.h"
#include "tgsi/tgsi_parse.h"
#include "util/format/u_format.h"
#include "util/u_math.h"
#include "virgl_int.h"

enum {
   // tgsi_sanity_check, which checks that the registers that instructions
   // name are declared, keys a register by its indexes, of 14 bits.
   MAX_INDEX = 1 << 14,
   MAX_TEMPS = MAX_INDEX,
   // tgsi_scan_shader masks samplers, images and buffers in 32 bits.
   MAX_RESOURCES = 32,
   // tgsi_to_nir's vertex shaders' inputs are VERT_ATTRIB_GENERIC0..15.
   MAX_ATTRIBS = 16,
   MAX_IO = PIPE_MAX_SHADER_INPUTS,
   MAX_DEPTH = 64,
};

// The flags of a declared register.
enum { DECLARED = 1, ARRAY = 2 };

struct check {
   mesa_shader_stage stage;
   const struct pipe_screen *screen;
   char *err;
   uint32_t errlen;

   // What the shader declared, in order: properties, then declarations
   // and immediates, then instructions, as Mesa's ureg emits them, as
   // tgsi_to_nir translates instructions of what it declared before them.
   bool declared, code;
   unsigned immediates;
   uint8_t temps[MAX_TEMPS];
   uint8_t inputs[MAX_IO], outputs[MAX_IO];
   // The semantics of inputs, outputs and system values, of declared
   // registers.
   uint8_t input_names[MAX_IO], output_names[MAX_IO], sv_names[MAX_IO];
   uint8_t svs[MAX_IO];
   // The varyings declared, by semantic, as bits of their indexes.
   uint32_t input_slots[TGSI_SEMANTIC_COUNT], output_slots[TGSI_SEMANTIC_COUNT];
   uint32_t samplers, views, images, buffers, ubos;
   bool addr;

   // The limits of the stage's resources.
   unsigned max_samplers, max_views, max_images, max_buffers, max_ubos;
   unsigned max_consts, max_ubo_consts;

   unsigned clip_distances, block[3];
   bool color0_writes_all;

   // The open IFs and loops, and whether each IF had its ELSE.
   unsigned depth;
   enum { CF_IF, CF_ELSE, CF_LOOP } cf[MAX_DEPTH];
};

static bool fail(struct check *k, const char *format, ...) {
   va_list ap;
   va_start(ap, format);
   vsnprintf(k->err, k->errlen, format, ap);
   va_end(ap);
   return false;
}

// varying_count returns the indexes of a semantic of the stage's inputs, or
// outputs, of which tgsi_to_nir has varying slots of the types of TGSI's
// registers, or 0 if it has none: it has fragment shaders' clip distances,
// layers and viewports, which are scalars, as vectors, and vertex
// shaders' viewports as floats.
static unsigned varying_count(const struct check *k, bool output, unsigned name) {
   if (k->stage == MESA_SHADER_VERTEX && output) {
      switch (name) {
      case TGSI_SEMANTIC_POSITION:
      case TGSI_SEMANTIC_FOG:
      case TGSI_SEMANTIC_PSIZE:
      case TGSI_SEMANTIC_CLIPVERTEX:
      case TGSI_SEMANTIC_LAYER:
         return 1;
      case TGSI_SEMANTIC_COLOR:
      case TGSI_SEMANTIC_BCOLOR:
      case TGSI_SEMANTIC_CLIPDIST:
         return 2;
      case TGSI_SEMANTIC_TEXCOORD:
         return 8;
      case TGSI_SEMANTIC_GENERIC:
         return 32;
      }
   } else if (k->stage == MESA_SHADER_FRAGMENT && !output) {
      switch (name) {
      case TGSI_SEMANTIC_POSITION:
      case TGSI_SEMANTIC_FACE:
      case TGSI_SEMANTIC_PCOORD:
      case TGSI_SEMANTIC_FOG:
         return 1;
      case TGSI_SEMANTIC_COLOR:
         return 2;
      case TGSI_SEMANTIC_TEXCOORD:
         return 8;
      case TGSI_SEMANTIC_GENERIC:
         return 32;
      }
   } else if (k->stage == MESA_SHADER_FRAGMENT && output) {
      switch (name) {
      case TGSI_SEMANTIC_POSITION:
      case TGSI_SEMANTIC_SAMPLEMASK:
         return 1;
      case TGSI_SEMANTIC_STENCIL:
         return k->screen->caps.shader_stencil_export ? 1 : 0;
      case TGSI_SEMANTIC_COLOR:
         return MIN2(k->screen->caps.max_render_targets, PIPE_MAX_COLOR_BUFS);
      }
   }
   return 0;
}

// sv_ok reports whether tgsi_to_nir has a system value of the stage.
static bool sv_ok(const struct check *k, unsigned name) {
   switch (k->stage) {
   case MESA_SHADER_VERTEX:
      return name == TGSI_SEMANTIC_VERTEXID || name == TGSI_SEMANTIC_VERTEXID_NOBASE ||
             name == TGSI_SEMANTIC_BASEVERTEX || name == TGSI_SEMANTIC_INSTANCEID;
   case MESA_SHADER_FRAGMENT:
      // tgsi_to_nir reads a FACE system value of the driver's, and a
      // POSITION and a PCOORD of any driver.
      return (name == TGSI_SEMANTIC_FACE && k->screen->caps.fs_face_is_integer_sysval) ||
             name == TGSI_SEMANTIC_POSITION || name == TGSI_SEMANTIC_PCOORD ||
             name == TGSI_SEMANTIC_SAMPLEID;
   case MESA_SHADER_COMPUTE:
      return name == TGSI_SEMANTIC_THREAD_ID || name == TGSI_SEMANTIC_BLOCK_ID ||
             name == TGSI_SEMANTIC_BLOCK_SIZE;
   default:
      return false;
   }
}

static bool check_property(struct check *k, struct tgsi_token *tokens, unsigned pos,
                           const struct tgsi_full_property *p) {
   if (k->declared || k->code)
      return fail(k, "a property after declarations");
   unsigned name = p->Property.PropertyName, value = p->u[0].Data;
   switch (name) {
   case TGSI_PROPERTY_FS_COORD_ORIGIN:
   case TGSI_PROPERTY_FS_COORD_PIXEL_CENTER:
   case TGSI_PROPERTY_LEGACY_MATH_RULES:
      if (value > 1)
         return fail(k, "property %u of %u", name, value);
      return true;
   case TGSI_PROPERTY_FS_EARLY_DEPTH_STENCIL:
      // Which tgsi_to_nir has not, but ttc_virgl_create_shader gives a
      // fragment shader's NIR.
      if (value > 1 || (value && k->stage != MESA_SHADER_FRAGMENT))
         return fail(k, "property %u of %u", name, value);
      return true;
   case TGSI_PROPERTY_FS_COLOR0_WRITES_ALL_CBUFS:
      if (value > 1)
         return fail(k, "property %u of %u", name, value);
      k->color0_writes_all = value;
      return true;
   case TGSI_PROPERTY_FS_DEPTH_LAYOUT:
      if (value > TGSI_FS_DEPTH_LAYOUT_UNCHANGED)
         return fail(k, "depth layout %u", value);
      return true;
   case TGSI_PROPERTY_NUM_CLIPDIST_ENABLED:
      if (value > PIPE_MAX_CLIP_OR_CULL_DISTANCE_COUNT)
         return fail(k, "%u clip distances", value);
      k->clip_distances = value;
      return true;
   case TGSI_PROPERTY_CS_FIXED_BLOCK_WIDTH:
   case TGSI_PROPERTY_CS_FIXED_BLOCK_HEIGHT:
   case TGSI_PROPERTY_CS_FIXED_BLOCK_DEPTH: {
      unsigned i = name - TGSI_PROPERTY_CS_FIXED_BLOCK_WIDTH;
      if (value > k->screen->compute_caps.max_block_size[i])
         return fail(k, "a block of %u in dimension %u", value, i);
      k->block[i] = value;
      return true;
   }
   case TGSI_PROPERTY_FS_BLEND_EQUATION_ADVANCED:
   case TGSI_PROPERTY_SEPARABLE_PROGRAM:
      // What the shader's code has, which tgsi_to_nir takes for a meaning
      // that it has not: Mesa's GLSL linker blends the advanced
      // equations in the shader with framebuffer fetch, of which this is
      // the equations; the renderer's shaders are separable anyway.
      if (p->Property.NrTokens > 1)
         ((struct tgsi_property_data *)&tokens[pos + 1])->Data = 0;
      return true;
   default:
      // Of the others, tgsi_to_nir has none, and Mesa's virgl driver sends
      // none of those that it has.
      if (value)
         return fail(k, "property %u", name);
      return true;
   }
}

// image_format_ok reports whether an image's format is one that the
// driver's shaders may access images of: tgsi_to_nir gives Zink the format
// for the image's type, of which Zink declares SPIR-V's capabilities.
static bool image_format_ok(const struct check *k, unsigned format, unsigned target) {
   if (format == PIPE_FORMAT_NONE)
      return true;
   if (format >= PIPE_FORMAT_COUNT)
      return false;
   struct pipe_screen *s = (struct pipe_screen *)k->screen;
   return s->is_format_supported(s, format, target == TGSI_TEXTURE_BUFFER ? PIPE_BUFFER : PIPE_TEXTURE_2D,
                                 0, 0, PIPE_BIND_SHADER_IMAGE);
}

static bool image_target_ok(unsigned target) {
   switch (target) {
   case TGSI_TEXTURE_BUFFER:
   case TGSI_TEXTURE_1D:
   case TGSI_TEXTURE_2D:
   case TGSI_TEXTURE_3D:
   case TGSI_TEXTURE_CUBE:
   case TGSI_TEXTURE_RECT:
   case TGSI_TEXTURE_1D_ARRAY:
   case TGSI_TEXTURE_2D_ARRAY:
   case TGSI_TEXTURE_CUBE_ARRAY:
   case TGSI_TEXTURE_2D_MSAA:
   case TGSI_TEXTURE_2D_ARRAY_MSAA:
      return true;
   default:
      return false;
   }
}

// check_varying checks a declaration of inputs or outputs of semantics:
// tgsi_to_nir gives the registers of a range that is not an array the
// range's first slot, each.
static bool check_varying(struct check *k, const struct tgsi_full_declaration *d, bool output) {
   unsigned first = d->Range.First, n = d->Range.Last - first + 1;
   bool array = d->Declaration.Array && d->Array.ArrayID;
   uint8_t *regs = output ? k->outputs : k->inputs;
   uint8_t *names = output ? k->output_names : k->input_names;
   if (k->stage == MESA_SHADER_VERTEX && !output) {
      if (d->Declaration.Semantic || d->Range.Last >= MAX_ATTRIBS)
         return fail(k, "vertex shader input %u", d->Range.Last);
   } else {
      unsigned name = d->Semantic.Name, index = d->Semantic.Index;
      if (!d->Declaration.Semantic)
         return fail(k, "a varying of no semantic");
      unsigned count = varying_count(k, output, name);
      if (index >= count || n > count - index)
         return fail(k, "%s %u of semantic %u, index %u", output ? "output" : "input", first,
                     name, index);
      if (n > 1 && (!array || (name != TGSI_SEMANTIC_GENERIC && name != TGSI_SEMANTIC_TEXCOORD)))
         return fail(k, "a range of semantic %u", name);
      uint32_t *slots = output ? k->output_slots : k->input_slots;
      uint32_t bits = BITFIELD_RANGE(index, n);
      if (slots[name] & bits)
         return fail(k, "semantic %u, index %u, twice", name, index);
      slots[name] |= bits;
      for (unsigned i = 0; i < n; i++)
         names[first + i] = name;
   }
   if (d->Declaration.Array && d->Array.ArrayID >= MAX_IO)
      return fail(k, "array %u", d->Array.ArrayID);
   for (unsigned i = 0; i < n; i++)
      regs[first + i] = DECLARED | (array ? ARRAY : 0);
   return true;
}

static bool check_declaration(struct check *k, const struct tgsi_full_declaration *d) {
   if (k->code)
      return fail(k, "a declaration after instructions");
   k->declared = true;
   unsigned file = d->Declaration.File, first = d->Range.First, last = d->Range.Last;
   if (first > last)
      return fail(k, "registers %u..%u", first, last);
   if (d->Declaration.Dimension && file != TGSI_FILE_CONSTANT)
      return fail(k, "a declaration of file %u of two dimensions", file);
   switch (file) {
   case TGSI_FILE_TEMPORARY:
      if (last >= MAX_TEMPS)
         return fail(k, "temporary %u", last);
      for (unsigned i = first; i <= last; i++)
         k->temps[i] = DECLARED | (d->Declaration.Array ? ARRAY : 0);
      return true;
   case TGSI_FILE_ADDRESS:
      // tgsi_to_nir has one address register.
      if (last != 0)
         return fail(k, "address register %u", last);
      k->addr = true;
      return true;
   case TGSI_FILE_CONSTANT: {
      unsigned buffer = d->Declaration.Dimension ? d->Dim.Index2D : 0;
      if (buffer >= k->max_ubos)
         return fail(k, "uniform buffer %u", buffer);
      if (last >= (buffer ? k->max_ubo_consts : k->max_consts))
         return fail(k, "constant %u of buffer %u", last, buffer);
      k->ubos |= 1u << buffer;
      return true;
   }
   case TGSI_FILE_SAMPLER:
      if (last >= k->max_samplers)
         return fail(k, "sampler %u", last);
      k->samplers |= BITFIELD_RANGE(first, last - first + 1);
      return true;
   case TGSI_FILE_SAMPLER_VIEW:
      if (last >= k->max_views)
         return fail(k, "sampler view %u", last);
      k->views |= BITFIELD_RANGE(first, last - first + 1);
      return true;
   case TGSI_FILE_IMAGE:
      if (last >= k->max_images)
         return fail(k, "image %u", last);
      if (!image_target_ok(d->Image.Resource) ||
          !image_format_ok(k, d->Image.Format, d->Image.Resource))
         return fail(k, "an image of target %u and format %u", d->Image.Resource, d->Image.Format);
      k->images |= BITFIELD_RANGE(first, last - first + 1);
      return true;
   case TGSI_FILE_BUFFER:
      if (last >= k->max_buffers)
         return fail(k, "shader buffer %u", last);
      k->buffers |= BITFIELD_RANGE(first, last - first + 1);
      return true;
   case TGSI_FILE_SYSTEM_VALUE:
      // tgsi_scan_shader records a declaration's first system value's
      // semantic only.
      if (first != last || last >= MAX_IO || !d->Declaration.Semantic ||
          !sv_ok(k, d->Semantic.Name))
         return fail(k, "system value %u of semantic %u", last, d->Semantic.Name);
      k->svs[first] = DECLARED;
      k->sv_names[first] = d->Semantic.Name;
      return true;
   case TGSI_FILE_INPUT:
   case TGSI_FILE_OUTPUT:
      if (last >= MAX_IO || k->stage == MESA_SHADER_COMPUTE)
         return fail(k, "varying %u of file %u", last, file);
      return check_varying(k, d, file == TGSI_FILE_OUTPUT);
   default:
      // tgsi_to_nir has no shared memory, atomic counters or others.
      return fail(k, "a declaration of file %u", file);
   }
}

// ind_ok reports whether an indirect index is of the address register,
// which is the only one that tgsi_to_nir has.
static bool ind_ok(const struct check *k, const struct tgsi_ind_register *ind) {
   return ind->File == TGSI_FILE_ADDRESS && ind->Index == 0 && k->addr;
}

// check_src checks a source operand of a value.
static bool check_src(struct check *k, const struct tgsi_full_instruction *inst, unsigned n) {
   const struct tgsi_full_src_register *s = &inst->Src[n];
   const struct tgsi_src_register *r = &s->Register;
   int index = r->Index;
   if (index < 0)
      return fail(k, "source %u of index %d", n, index);
   if (r->Dimension && (r->File != TGSI_FILE_CONSTANT || s->Dimension.Dimension))
      return fail(k, "source %u of file %u of two dimensions", n, r->File);
   if (r->Indirect && (!ind_ok(k, &s->Indirect) ||
                       (r->File != TGSI_FILE_CONSTANT && r->File != TGSI_FILE_TEMPORARY)))
      return fail(k, "source %u of file %u of an indirect index", n, r->File);
   switch (r->File) {
   case TGSI_FILE_TEMPORARY:
      // tgsi_to_nir indexes arrays only.
      if (index >= MAX_TEMPS || !(k->temps[index] & DECLARED) ||
          (r->Indirect && !(k->temps[index] & ARRAY)))
         return fail(k, "temporary %d", index);
      return true;
   case TGSI_FILE_ADDRESS:
      if (index != 0 || !k->addr)
         return fail(k, "address register %d", index);
      return true;
   case TGSI_FILE_IMMEDIATE:
      if ((unsigned)index >= k->immediates)
         return fail(k, "immediate %d of %u", index, k->immediates);
      return true;
   case TGSI_FILE_INPUT:
      if (index >= MAX_IO || !k->inputs[index])
         return fail(k, "input %d", index);
      return true;
   case TGSI_FILE_OUTPUT:
      // tgsi_to_nir reads a fragment shader's outputs of the framebuffer,
      // as FBFETCH does, and has no other reads of outputs.
      if (inst->Instruction.Opcode != TGSI_OPCODE_FBFETCH || index >= MAX_IO ||
          !k->outputs[index] || k->output_names[index] != TGSI_SEMANTIC_COLOR)
         return fail(k, "a read of output %d", index);
      return true;
   case TGSI_FILE_SYSTEM_VALUE:
      if (index >= MAX_IO || !k->svs[index])
         return fail(k, "system value %d", index);
      return true;
   case TGSI_FILE_CONSTANT: {
      if (index >= MAX_INDEX)
         return fail(k, "constant %d", index);
      if (!r->Dimension)
         return true;
      int buffer = s->Dimension.Index;
      if (s->Dimension.Indirect) {
         // tgsi_to_nir takes the address for the buffer's index among the
         // uniform buffers, which TGSI's buffer 1 is the first of.
         if (!ind_ok(k, &s->DimIndirect) || buffer != 1)
            return fail(k, "uniform buffer %d of an indirect index", buffer);
      } else if (buffer < 0 || buffer >= MAX_RESOURCES || !(k->ubos & (1u << buffer))) {
         return fail(k, "uniform buffer %d", buffer);
      }
      return true;
   }
   default:
      return fail(k, "source %u of file %u", n, r->File);
   }
}

// check_resource checks a source operand of a resource of file.
static bool check_resource(struct check *k, const struct tgsi_full_instruction *inst, unsigned n,
                           unsigned file) {
   const struct tgsi_src_register *r = &inst->Src[n].Register;
   uint32_t declared = file == TGSI_FILE_SAMPLER ? k->samplers
                       : file == TGSI_FILE_IMAGE ? k->images
                                                 : k->buffers;
   // tgsi_to_nir takes an indirect resource for its first.
   if (r->File != file || r->Indirect || r->Dimension || r->Index < 0 ||
       r->Index >= MAX_RESOURCES || !(declared & (1u << r->Index)))
      return fail(k, "resource %d of file %u for one of file %u", r->Index, r->File, file);
   return true;
}

static bool check_dst(struct check *k, const struct tgsi_full_instruction *inst, unsigned n) {
   const struct tgsi_full_dst_register *d = &inst->Dst[n];
   const struct tgsi_dst_register *r = &d->Register;
   int index = r->Index;
   if (index < 0 || r->Dimension)
      return fail(k, "destination %u of index %d", n, index);
   if (r->Indirect && !ind_ok(k, &d->Indirect))
      return fail(k, "destination %u of an indirect index", n);
   switch (r->File) {
   case TGSI_FILE_TEMPORARY:
      if (index >= MAX_TEMPS || !(k->temps[index] & DECLARED) ||
          (r->Indirect && !(k->temps[index] & ARRAY)))
         return fail(k, "temporary %d", index);
      return true;
   case TGSI_FILE_OUTPUT:
      if (index >= MAX_IO || !k->outputs[index] || (r->Indirect && !(k->outputs[index] & ARRAY)))
         return fail(k, "output %d", index);
      return true;
   case TGSI_FILE_ADDRESS:
      if (index != 0 || !k->addr || r->Indirect)
         return fail(k, "address register %d", index);
      return true;
   default:
      return fail(k, "destination %u of file %u", n, r->File);
   }
}

// The texture targets that tgsi_to_nir samples with each opcode, as TGSI
// lays out their coordinates: it takes TXB's and TXL's bias and level, and
// TXP's projector, from the coordinates' w, as it takes the comparator of
// targets of three coordinates; and the comparator of cube arrays from
// source 1, which TEX2 alone has.
#define T(t) (1u << TGSI_TEXTURE_##t)
#define T_LOD (T(1D) | T(2D) | T(3D) | T(CUBE) | T(1D_ARRAY) | T(2D_ARRAY) | T(SHADOW1D) | \
               T(SHADOW2D) | T(SHADOW1D_ARRAY))
static uint32_t tex_targets(unsigned opcode) {
   switch (opcode) {
   case TGSI_OPCODE_TEX:
      return T_LOD | T(RECT) | T(SHADOWRECT) | T(SHADOW2D_ARRAY) | T(SHADOWCUBE) | T(CUBE_ARRAY);
   case TGSI_OPCODE_TXP:
      return T(1D) | T(2D) | T(3D) | T(RECT) | T(SHADOW1D) | T(SHADOW2D) | T(SHADOWRECT);
   case TGSI_OPCODE_TXB:
   case TGSI_OPCODE_TXL:
      return T_LOD;
   case TGSI_OPCODE_TXD:
      return T_LOD | T(RECT) | T(SHADOWRECT) | T(SHADOW2D_ARRAY) | T(SHADOWCUBE) | T(CUBE_ARRAY);
   case TGSI_OPCODE_TEX2:
      return T(SHADOWCUBE_ARRAY);
   case TGSI_OPCODE_TXB2:
   case TGSI_OPCODE_TXL2:
      return T(CUBE_ARRAY) | T(SHADOWCUBE) | T(SHADOW2D_ARRAY);
   case TGSI_OPCODE_TXF:
      return T(1D) | T(2D) | T(3D) | T(RECT) | T(1D_ARRAY) | T(2D_ARRAY) | T(BUFFER) |
             T(2D_MSAA) | T(2D_ARRAY_MSAA);
   case TGSI_OPCODE_LODQ:
      return T(1D) | T(2D) | T(3D) | T(CUBE) | T(1D_ARRAY) | T(2D_ARRAY) | T(CUBE_ARRAY);
   case TGSI_OPCODE_TG4:
      return T(2D) | T(2D_ARRAY) | T(CUBE) | T(CUBE_ARRAY) | T(RECT) | T(SHADOW2D) |
             T(SHADOW2D_ARRAY) | T(SHADOWCUBE) | T(SHADOWRECT);
   case TGSI_OPCODE_TXQ:
      // tgsi_to_nir queries a level's size and the levels of each target,
      // of which buffers and multisampled textures have none; Zink makes
      // rectangles 2D textures.
      return T_LOD | T(RECT) | T(SHADOWRECT) | T(SHADOW2D_ARRAY) | T(SHADOWCUBE) |
             T(CUBE_ARRAY) | T(SHADOWCUBE_ARRAY);
   default:
      return 0;
   }
}
#undef T_LOD
#undef T

static bool check_texture(struct check *k, const struct tgsi_full_instruction *inst) {
   unsigned op = inst->Instruction.Opcode, target = inst->Texture.Texture;
   unsigned samp;
   switch (op) {
   case TGSI_OPCODE_TEX2:
   case TGSI_OPCODE_TXB2:
   case TGSI_OPCODE_TXL2:
   case TGSI_OPCODE_TG4:
      samp = 2;
      break;
   case TGSI_OPCODE_TXD:
      samp = 3;
      break;
   default:
      samp = 1;
   }
   if (!inst->Instruction.Texture || target >= TGSI_TEXTURE_COUNT || !(tex_targets(op) & (1u << target)))
      return fail(k, "%s of target %u", tgsi_get_opcode_name(op), target);
   if (inst->Instruction.NumSrcRegs != samp + 1)
      return fail(k, "%s of %u sources", tgsi_get_opcode_name(op), inst->Instruction.NumSrcRegs);
   if (!check_resource(k, inst, samp, TGSI_FILE_SAMPLER))
      return false;
   for (unsigned i = 0; i < samp; i++) {
      if (!check_src(k, inst, i))
         return false;
   }
   if (op == TGSI_OPCODE_TG4 && !k->screen->caps.tgsi_tg4_component_in_swizzle) {
      // tgsi_to_nir reads the component of an immediate.
      const struct tgsi_src_register *r = &inst->Src[1].Register;
      if (r->File != TGSI_FILE_IMMEDIATE || r->Indirect || r->Negate || r->Absolute)
         return fail(k, "TG4 of a component of file %u", r->File);
   }
   // tgsi_to_nir takes each offset for a source of its own, which NIR has
   // one of.
   unsigned offsets = inst->Texture.NumOffsets;
   if (offsets > 1 || (offsets && (op == TGSI_OPCODE_TXQ || op == TGSI_OPCODE_LODQ)))
      return fail(k, "%s of %u offsets", tgsi_get_opcode_name(op), offsets);
   if (offsets) {
      const struct tgsi_texture_offset *o = &inst->TexOffsets[0];
      bool ok = o->Index >= 0 &&
                ((o->File == TGSI_FILE_IMMEDIATE && (unsigned)o->Index < k->immediates) ||
                 (o->File == TGSI_FILE_TEMPORARY && o->Index < MAX_TEMPS &&
                  (k->temps[o->Index] & DECLARED)));
      if (!ok)
         return fail(k, "an offset of file %u, index %d", o->File, o->Index);
   }
   return true;
}

static bool check_memory(struct check *k, const struct tgsi_full_instruction *inst) {
   bool store = inst->Instruction.Opcode == TGSI_OPCODE_STORE;
   unsigned file = store ? inst->Dst[0].Register.File : inst->Src[0].Register.File;
   if (file != TGSI_FILE_BUFFER && file != TGSI_FILE_IMAGE)
      return fail(k, "a memory access of file %u", file);
   if (store) {
      const struct tgsi_dst_register *r = &inst->Dst[0].Register;
      uint32_t declared = file == TGSI_FILE_IMAGE ? k->images : k->buffers;
      if (r->Indirect || r->Dimension || r->Index < 0 || r->Index >= MAX_RESOURCES ||
          !(declared & (1u << r->Index)))
         return fail(k, "a store to resource %d", r->Index);
      if (!check_src(k, inst, 0) || !check_src(k, inst, 1))
         return false;
   } else if (!check_resource(k, inst, 0, file) || !check_src(k, inst, 1) || !check_dst(k, inst, 0)) {
      return false;
   }
   if (file == TGSI_FILE_IMAGE &&
       (!image_target_ok(inst->Memory.Texture) ||
        !image_format_ok(k, inst->Memory.Format, inst->Memory.Texture)))
      return fail(k, "an image access of target %u and format %u", inst->Memory.Texture,
                  inst->Memory.Format);
   return true;
}

// check_flow checks that IF, ELSE and ENDIF, and BGNLOOP and ENDLOOP, nest,
// as tgsi_to_nir's builder of NIR's control flow trusts that they do.
static bool check_flow(struct check *k, unsigned op) {
   switch (op) {
   case TGSI_OPCODE_IF:
   case TGSI_OPCODE_UIF:
   case TGSI_OPCODE_BGNLOOP:
      if (k->depth == MAX_DEPTH)
         return fail(k, "control flow %u deep", k->depth);
      k->cf[k->depth++] = op == TGSI_OPCODE_BGNLOOP ? CF_LOOP : CF_IF;
      return true;
   case TGSI_OPCODE_ELSE:
      if (!k->depth || k->cf[k->depth - 1] != CF_IF)
         return fail(k, "ELSE of no IF");
      k->cf[k->depth - 1] = CF_ELSE;
      return true;
   case TGSI_OPCODE_ENDIF:
      if (!k->depth || k->cf[k->depth - 1] == CF_LOOP)
         return fail(k, "ENDIF of no IF");
      k->depth--;
      return true;
   case TGSI_OPCODE_ENDLOOP:
      if (!k->depth || k->cf[k->depth - 1] != CF_LOOP)
         return fail(k, "ENDLOOP of no BGNLOOP");
      k->depth--;
      return true;
   case TGSI_OPCODE_BRK:
   case TGSI_OPCODE_CONT:
      for (unsigned i = 0; i < k->depth; i++) {
         if (k->cf[i] == CF_LOOP)
            return true;
      }
      return fail(k, "%s out of loops", tgsi_get_opcode_name(op));
   default:
      return true;
   }
}

// The classes of the opcodes that tgsi_to_nir translates.
enum op_class {
   OP_NONE,
   OP_ALU,
   OP_DOUBLE,
   OP_INT64,
   OP_FRAGMENT,
   OP_COMPUTE,
   OP_FLOW,
   OP_TEXTURE,
   OP_MEMORY,
};

static enum op_class op_class(unsigned op) {
   switch (op) {
   case TGSI_OPCODE_ARL: case TGSI_OPCODE_MOV: case TGSI_OPCODE_LIT: case TGSI_OPCODE_RCP:
   case TGSI_OPCODE_RSQ: case TGSI_OPCODE_EXP: case TGSI_OPCODE_LOG: case TGSI_OPCODE_MUL:
   case TGSI_OPCODE_ADD: case TGSI_OPCODE_DP3: case TGSI_OPCODE_DP4: case TGSI_OPCODE_DST:
   case TGSI_OPCODE_MIN: case TGSI_OPCODE_MAX: case TGSI_OPCODE_SLT: case TGSI_OPCODE_SGE:
   case TGSI_OPCODE_MAD: case TGSI_OPCODE_LRP: case TGSI_OPCODE_SQRT: case TGSI_OPCODE_FRC:
   case TGSI_OPCODE_FLR: case TGSI_OPCODE_ROUND: case TGSI_OPCODE_EX2: case TGSI_OPCODE_LG2:
   case TGSI_OPCODE_POW: case TGSI_OPCODE_COS: case TGSI_OPCODE_SEQ: case TGSI_OPCODE_SGT:
   case TGSI_OPCODE_SIN: case TGSI_OPCODE_SLE: case TGSI_OPCODE_SNE: case TGSI_OPCODE_ARR:
   case TGSI_OPCODE_SSG: case TGSI_OPCODE_CMP: case TGSI_OPCODE_DIV: case TGSI_OPCODE_DP2:
   case TGSI_OPCODE_CEIL: case TGSI_OPCODE_I2F: case TGSI_OPCODE_NOT: case TGSI_OPCODE_TRUNC:
   case TGSI_OPCODE_SHL: case TGSI_OPCODE_AND: case TGSI_OPCODE_OR: case TGSI_OPCODE_MOD:
   case TGSI_OPCODE_XOR: case TGSI_OPCODE_NOP: case TGSI_OPCODE_FSEQ: case TGSI_OPCODE_FSGE:
   case TGSI_OPCODE_FSLT: case TGSI_OPCODE_FSNE: case TGSI_OPCODE_F2I: case TGSI_OPCODE_IDIV:
   case TGSI_OPCODE_IMAX: case TGSI_OPCODE_IMIN: case TGSI_OPCODE_INEG: case TGSI_OPCODE_ISGE:
   case TGSI_OPCODE_ISHR: case TGSI_OPCODE_ISLT: case TGSI_OPCODE_F2U: case TGSI_OPCODE_U2F:
   case TGSI_OPCODE_UADD: case TGSI_OPCODE_UDIV: case TGSI_OPCODE_UMAD: case TGSI_OPCODE_UMAX:
   case TGSI_OPCODE_UMIN: case TGSI_OPCODE_UMOD: case TGSI_OPCODE_UMUL: case TGSI_OPCODE_USEQ:
   case TGSI_OPCODE_USGE: case TGSI_OPCODE_USHR: case TGSI_OPCODE_USLT: case TGSI_OPCODE_USNE:
   case TGSI_OPCODE_UARL: case TGSI_OPCODE_UCMP: case TGSI_OPCODE_IABS: case TGSI_OPCODE_ISSG:
   case TGSI_OPCODE_IMUL_HI: case TGSI_OPCODE_UMUL_HI: case TGSI_OPCODE_IBFE:
   case TGSI_OPCODE_UBFE: case TGSI_OPCODE_BFI: case TGSI_OPCODE_BREV: case TGSI_OPCODE_POPC:
   case TGSI_OPCODE_LSB: case TGSI_OPCODE_IMSB: case TGSI_OPCODE_UMSB:
      return OP_ALU;
   case TGSI_OPCODE_F2D: case TGSI_OPCODE_D2F: case TGSI_OPCODE_DMUL: case TGSI_OPCODE_D2U:
   case TGSI_OPCODE_U2D:
      return OP_DOUBLE;
   case TGSI_OPCODE_U64ADD: case TGSI_OPCODE_U64MUL: case TGSI_OPCODE_U64DIV:
   case TGSI_OPCODE_U64SNE: case TGSI_OPCODE_I64NEG: case TGSI_OPCODE_I64ABS:
      return OP_INT64;
   // Derivatives, discards, framebuffer fetches, and implicit levels of
   // detail but a texture's own, of which Zink gives other stages level 0.
   case TGSI_OPCODE_DDX: case TGSI_OPCODE_DDX_FINE: case TGSI_OPCODE_DDY:
   case TGSI_OPCODE_DDY_FINE: case TGSI_OPCODE_KILL: case TGSI_OPCODE_KILL_IF:
   case TGSI_OPCODE_FBFETCH:
      return OP_FRAGMENT;
   case TGSI_OPCODE_BARRIER:
      return OP_COMPUTE;
   case TGSI_OPCODE_IF: case TGSI_OPCODE_UIF: case TGSI_OPCODE_ELSE: case TGSI_OPCODE_ENDIF:
   case TGSI_OPCODE_BGNLOOP: case TGSI_OPCODE_ENDLOOP: case TGSI_OPCODE_BRK:
   case TGSI_OPCODE_CONT: case TGSI_OPCODE_RET: case TGSI_OPCODE_END:
      return OP_FLOW;
   case TGSI_OPCODE_TEX: case TGSI_OPCODE_TXP: case TGSI_OPCODE_TXB: case TGSI_OPCODE_TXL:
   case TGSI_OPCODE_TXD: case TGSI_OPCODE_TEX2: case TGSI_OPCODE_TXB2: case TGSI_OPCODE_TXL2:
   case TGSI_OPCODE_TXF: case TGSI_OPCODE_TG4: case TGSI_OPCODE_LODQ: case TGSI_OPCODE_TXQ:
      return OP_TEXTURE;
   case TGSI_OPCODE_LOAD: case TGSI_OPCODE_STORE:
      return OP_MEMORY;
   default:
      return OP_NONE;
   }
}

static bool check_instruction(struct check *k, const struct tgsi_full_instruction *inst) {
   k->code = true;
   unsigned op = inst->Instruction.Opcode;
   enum op_class class = op_class(op);
   bool ok;
   switch (class) {
   case OP_DOUBLE:
      ok = k->screen->caps.doubles;
      break;
   case OP_INT64:
      ok = k->screen->caps.int64;
      break;
   case OP_FRAGMENT:
      ok = k->stage == MESA_SHADER_FRAGMENT &&
           (op != TGSI_OPCODE_FBFETCH || k->screen->caps.fbfetch);
      break;
   case OP_COMPUTE:
      ok = k->stage == MESA_SHADER_COMPUTE;
      break;
   case OP_TEXTURE:
      // Implicit levels of detail, which only fragment shaders have the
      // derivatives of: Zink samples level 0 of TEX in other stages.
      ok = k->stage == MESA_SHADER_FRAGMENT ||
           (op != TGSI_OPCODE_TXB && op != TGSI_OPCODE_TXB2 && op != TGSI_OPCODE_LODQ);
      break;
   default:
      ok = class != OP_NONE;
   }
   if (!ok)
      return fail(k, "%s in stage %u", tgsi_get_opcode_name(op), k->stage);
   if (class == OP_FLOW && !check_flow(k, op))
      return false;
   if (class == OP_TEXTURE) {
      if (!check_texture(k, inst))
         return false;
   } else if (class == OP_MEMORY) {
      return check_memory(k, inst);
   } else {
      for (unsigned i = 0; i < inst->Instruction.NumSrcRegs; i++) {
         if (!check_src(k, inst, i))
            return false;
      }
   }
   for (unsigned i = 0; i < inst->Instruction.NumDstRegs; i++) {
      if (!check_dst(k, inst, i))
         return false;
   }
   return true;
}

// check_end checks what the shader declared of what it said of itself.
static bool check_end(struct check *k) {
   if (k->depth)
      return fail(k, "%u unclosed IF or BGNLOOP", k->depth);
   // tgsi_to_nir gives a vertex shader's clip distances a varying of the
   // property's, which an output of CLIPDIST 0 declares.
   uint32_t clip = k->output_slots[TGSI_SEMANTIC_CLIPDIST];
   if ((clip || k->clip_distances) && (!(clip & 1) || !k->clip_distances))
      return fail(k, "%u clip distances of outputs %#x", k->clip_distances, clip);
   // tgsi_to_nir gives every color output the first color, of which the
   // property writes each target.
   if (k->color0_writes_all && util_bitcount(k->output_slots[TGSI_SEMANTIC_COLOR]) > 1)
      return fail(k, "colors %#x of each to every target", k->output_slots[TGSI_SEMANTIC_COLOR]);
   if (k->stage == MESA_SHADER_COMPUTE) {
      // A block of a fixed size has each dimension's, and one of a
      // variable size none.
      unsigned *b = k->block;
      if ((b[0] || b[1] || b[2]) &&
          (!b[0] || !b[1] || !b[2] ||
           (uint64_t)b[0] * b[1] * b[2] > k->screen->compute_caps.max_threads_per_block))
         return fail(k, "a block of %ux%ux%u", b[0], b[1], b[2]);
   }
   return true;
}

uint32_t ttc_virgl_tgsi_property(struct tgsi_token *tokens, unsigned name, bool clear) {
   struct tgsi_parse_context p;
   if (tgsi_parse_init(&p, tokens) != TGSI_PARSE_OK)
      return 0;
   uint32_t value = 0;
   while (!tgsi_parse_end_of_tokens(&p)) {
      unsigned pos = p.Position;
      tgsi_parse_token(&p);
      const struct tgsi_full_property *prop = &p.FullToken.FullProperty;
      if (p.FullToken.Token.Type != TGSI_TOKEN_TYPE_PROPERTY || prop->Property.PropertyName != name ||
          prop->Property.NrTokens < 2)
         continue;
      value = prop->u[0].Data;
      if (clear)
         ((struct tgsi_property_data *)&tokens[pos + 1])->Data = 0;
   }
   tgsi_parse_free(&p);
   return value;
}

bool ttc_virgl_check_tgsi(struct tgsi_token *tokens, char *err, uint32_t errlen) {
   struct check *k = calloc(1, sizeof(*k));
   if (!k) {
      snprintf(err, errlen, "no memory to check the shader");
      return false;
   }
   k->err = err;
   k->errlen = errlen;
   k->screen = ttc_virgl_screen;
   k->stage = tgsi_get_processor_type(tokens);
   bool ok = k->stage == MESA_SHADER_VERTEX || k->stage == MESA_SHADER_FRAGMENT ||
             k->stage == MESA_SHADER_COMPUTE;
   if (!ok) {
      // tgsi_to_nir has no geometry or tessellation shaders.
      fail(k, "a shader of stage %u", k->stage);
      free(k);
      return false;
   }
   const struct pipe_shader_caps *caps = &k->screen->shader_caps[k->stage];
   k->max_samplers = MIN2(caps->max_texture_samplers, MAX_RESOURCES);
   k->max_views = MIN2(caps->max_sampler_views, MAX_RESOURCES);
   k->max_images = MIN2(caps->max_shader_images, MAX_RESOURCES);
   k->max_buffers = MIN2(caps->max_shader_buffers, MAX_RESOURCES);
   k->max_ubos = MIN2(caps->max_const_buffers, MAX_RESOURCES);
   if (k->stage == MESA_SHADER_VERTEX && ttc_virgl_xfb_emulated) {
      // The slots that the renderer keeps for transform feedback.
      k->max_buffers = MIN2(k->max_buffers, ttc_virgl_xfb_ssbo);
      k->max_ubos = MIN2(k->max_ubos, ttc_virgl_xfb_ubo);
   }
   k->max_consts = MIN2(caps->max_const_buffer0_size / 16, MAX_INDEX);
   k->max_ubo_consts = MIN2(k->screen->caps.max_constant_buffer_size / 16, MAX_INDEX);

   struct tgsi_parse_context p;
   if (tgsi_parse_init(&p, tokens) != TGSI_PARSE_OK) {
      snprintf(err, errlen, "unparsable TGSI");
      free(k);
      return false;
   }
   while (ok && !tgsi_parse_end_of_tokens(&p)) {
      unsigned pos = p.Position;
      tgsi_parse_token(&p);
      switch (p.FullToken.Token.Type) {
      case TGSI_TOKEN_TYPE_PROPERTY:
         ok = check_property(k, tokens, pos, &p.FullToken.FullProperty);
         break;
      case TGSI_TOKEN_TYPE_DECLARATION:
         ok = check_declaration(k, &p.FullToken.FullDeclaration);
         break;
      case TGSI_TOKEN_TYPE_IMMEDIATE:
         if (k->code)
            ok = fail(k, "an immediate after instructions");
         k->declared = true;
         k->immediates++;
         break;
      case TGSI_TOKEN_TYPE_INSTRUCTION:
         ok = check_instruction(k, &p.FullToken.FullInstruction);
         break;
      default:
         ok = fail(k, "a token of type %u", p.FullToken.Token.Type);
      }
   }
   tgsi_parse_free(&p);
   ok = ok && check_end(k);
   free(k);
   return ok;
}
